-- 017: 给测速结果加「轮次」标识，让"取最近一轮"变得精确。
--
-- 为什么要加
-- ----------
-- 016 建表时用 measured_at 当唯一键的一部分，读取时用「最大时间戳
-- ± 60 秒窗口」来圈定一轮。真机跑起来立刻暴露问题：
--
--   手动触发两次测速（间隔 < 60 秒）→ 两轮都被当成"同一轮"读出来
--   → 界面上同一个靶点出现两次，by_carrier 的平均值把两轮混着算。
--
-- 实测复现（2026-10-02，se）：两次测速间隔 24 秒，
-- 响应里 `mobile-bj` 出现两行（171.969 与 171.805），
-- 「移动」平均显示 214.543ms —— 那是 4 个样本的平均
-- （两轮各 2 个测到的靶点），而不是一轮的真实值。
--
-- 根因：**「一轮」是执行语义，不是时间语义**。用时间窗口去逼近它，
-- 窗口大小就成了一个必然出错的魔法数 —— 调大了混轮次，调小了漏行
-- （同一轮各行的时间戳本来就不同）。
--
-- 修法：显式记录轮次。
--
-- 关于 round_id 的生成方式：**由应用侧生成（UUID）**，而不是数据库的
-- now() 或序列。理由：
--   1. 一次探针执行只生成一个 round_id，之后该轮所有行共用它 ——
--      这是"同一轮"的权威定义，不依赖任何时间比较
--   2. 应用侧生成可以跨节点复用同一个值（一次全量测速里，
--      多台节点并发执行；用同一个 round_id 便于按"第几次全量"聚合）
--   3. 不引入序列（序列会在事务回滚时跳号，且与"谁是最近一轮"无关）
--
-- 已有的历史行怎么办
-- ------------------
-- 存量行没有 round_id。**用 measured_at 截到秒生成一个确定性 UUID**
-- 作为回填值 —— 同一秒内的行会得到同一个 round_id，这与旧的
-- "同一轮在秒级内连续写入"的假设一致，是最接近事实的回填。
--
-- 为什么不用 gen_random_uuid() 逐行生成：那会让原本属于同一轮的行
-- 各自拿到不同 round_id，历史数据就永久"碎"成一行一轮，反而比现在更差。

ALTER TABLE node_speedtests
    ADD COLUMN IF NOT EXISTS round_id UUID;

-- 回填：用 measured_at（截到秒）+ node_id 生成确定性 UUID。
--
-- md5() 在 Postgres 里是 immutable，可以稳定复现；
-- 用它构造 UUID 字面量（取 32 位十六进制拼成 8-4-4-4-12）。
UPDATE node_speedtests
   SET round_id = (
       substr(h, 1, 8) || '-' || substr(h, 9, 4) || '-' || substr(h, 13, 4) || '-' ||
       substr(h, 17, 4) || '-' || substr(h, 21, 12)
   )::uuid
  FROM (
      SELECT id,
             md5(node_id::text || ':' ||
                 date_trunc('second', measured_at AT TIME ZONE 'UTC')::text) AS h
        FROM node_speedtests
       WHERE round_id IS NULL
  ) AS computed
 WHERE node_speedtests.id = computed.id;

-- 回填完成后设为 NOT NULL：新插入的行必须带轮次，
-- 否则读取侧的 `JOIN latest ON round_id` 会静默漏掉它们
-- （表现为"测速跑了但页面显示无数据"）。
ALTER TABLE node_speedtests
    ALTER COLUMN round_id SET NOT NULL;

COMMENT ON COLUMN node_speedtests.round_id IS
    '测速轮次标识。一次探针执行生成的同一个值，用于精确判定"最近一轮"。由应用侧生成 UUID。';

-- 读取侧的主要查询是「按节点找最新一轮」，这个索引直接服务它。
--
-- 排序方向 DESC：查询总是要最新的那一轮。
CREATE INDEX IF NOT EXISTS idx_node_speedtests_round
    ON node_speedtests (node_id, round_id, measured_at DESC);
