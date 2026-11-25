-- 018: 节点状态探测支持走「节点门户 API」，不再只依赖 SSH。
--
-- 问题（2026-10-02 生产实测复现）
-- ------------------------------
-- hysteria_nodes.status 此前**只有一个写入点**：collector.probeNodes，
-- 而它要求所属主机配了 SSH 凭据（要 SSH 过去跑 `ss -lun` 看端口）。
--
-- 于是「通过 API 导入的节点」陷入死循环：
--   ensureHostForImportedNode 为 API 导入的节点建的主机行**刻意不带 SSH 凭据**
--   （注释里写得很清楚：不想让运维者去编一个他没有的 SSH 用户）
--   → collector 看到无凭据就直接 return
--   → status 永远是 unknown，last_checked_at 永远是 null
--   → 界面上那台节点永远显示「未知 / 未探测」
--
-- 典型状态分布示例：
--   node2.example.com  有 SSH 凭据 → online，最近探测有值
--   node1.example.com  无 SSH 凭据 → unknown，从未探测（但它的 API 明明通！）
--   node3.example.com       无 SSH 凭据 → unknown，从未探测
--
-- 而这三台的**节点门户 API 都是已绑定的**，调 /api/v1/node/meta 能直接
-- 拿到 listen_port / users_count / node_time —— 判断节点在线与否绰绰有余。
--
-- 修法：给状态探测加一条 API 路径，优先级与取舍见 collector 里的注释。
--
-- 本迁移只加两个记账列，让界面上能区分「这个状态是怎么得来的」。

ALTER TABLE hysteria_nodes
    -- 最近一次成功/失败的状态探测走的是哪条路径：api / ssh / none。
    -- 界面上据此显示「API 探测」或「SSH 探测」，用户才知道
    -- 「为什么这台节点的 CPU 是空的但状态是在线」—— 因为走的是 API。
    ADD COLUMN IF NOT EXISTS status_source TEXT NOT NULL DEFAULT '',
    -- API 探测的结果细分：ok / unreachable / unauthorized / no_api。
    -- 与 status 分开是因为它们回答不同问题：
    --   status=offline 说明"节点没在跑"
    --   status_error=auth 说明"节点可能在跑，但我们的密钥不对"
    -- 后者的处理动作完全不同（改密钥，而不是去查节点）。
    ADD COLUMN IF NOT EXISTS status_error TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN hysteria_nodes.status_source IS
    '最近一次状态探测走的路径：api / ssh / none（空=还没探测过）。';
COMMENT ON COLUMN hysteria_nodes.status_error IS
    'API 探测失败时的分类：unreachable / unauthorized / no_api。空表示无错误。';
