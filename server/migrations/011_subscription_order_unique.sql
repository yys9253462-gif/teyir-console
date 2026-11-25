-- 011: 同一个 order_id 只允许存在一条订阅（并发下也要成立）。
--
-- 背景：storeCreateOrder 的幂等原本是「先查 loadSubscriptionByOrder，没有
-- 再 INSERT」。这是典型的 check-then-insert：8 个并发请求会在任何一次插入
-- 落库之前全都读到「没有」，于是全都插入，一个订单变成三条订阅、三个不同的
-- 交付链接。商城重试 webhook 是很常见的事，所以这不是理论竞态。
--
-- 实测证据（tests/concurrency.py C 段，8 线程同一 order_id）：
--     RACE-C-1790744068 -> 3 行，三个不同 token，created_at 相差不到 1ms
--
-- 修法分两半，缺一不可：
--   1. 这里加一个部分唯一索引，让数据库成为幂等的最终裁判。用
--      `WHERE deleted_at IS NULL AND order_id IS NOT NULL`：
--        * 软删除的旧订阅不该挡住同一订单重新下单（沿用 005 的做法）；
--        * order_id 为 NULL 的运营自建订阅可以有任意多条。
--   2. 应用层把「查到就返回、否则插入」改成一条带 ON CONFLICT 的语句，
--      靠数据库的原子性收敛，而不是靠两次查询之间的时间差。
--
-- 注意：009 里那个 idx_subscription_tokens_order_id 是非唯一索引，用于加速
-- 按 order_id 查询，依然有用，保留。本迁移只补唯一性。
--
-- 若线上已存在重复行，本迁移会失败（这是有意为之）：先人工确认重复的来源，
-- 把同一 order_id 的多余订阅处理掉（通常保留最早一条、把其余的按客户实际
-- 拿到的那条修正），再重跑。不要为了让它过而改成非唯一索引 —— 那就把刚修
-- 好的竞态又放开了一次。
CREATE UNIQUE INDEX IF NOT EXISTS subscription_tokens_order_id_active
    ON subscription_tokens (order_id)
    WHERE deleted_at IS NULL AND order_id IS NOT NULL;
