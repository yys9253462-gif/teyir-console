-- 058：待补做的回收动作是否要登记「商城撤销的订单号」（2026-11-06）。
--
-- 迁移 056 只该记录商城明确撤销的订单（storeRevokeOrder）。原来后台补完任何「回收后删除」都登记：
-- 管理员在订阅中心手动删一条商城订阅、恰好有节点不可达走了后台补删 —— 这个订单号被永久拉黑，
-- 买家之后在商城续费（renew 404 → 同一 order_id 重新下单）拿到 410，付了钱却拿不到货。
-- 而同样的手动删除若一次回收成功走的是硬删，不拉黑：两条路径语义不一致。
ALTER TABLE subscription_pending_node_actions
    ADD COLUMN IF NOT EXISTS register_revoked_order boolean NOT NULL DEFAULT false;

-- 已有的「回收后删除」待办来源无法区分；按旧行为保留登记（宁可多挡一次重下单，也不让退款单被重新开通）。
UPDATE subscription_pending_node_actions SET register_revoked_order = true WHERE then_delete;
