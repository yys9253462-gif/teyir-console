-- 056：商城撤销过的订单号（2026-11-06）。
--
-- subscription_tokens 的 order_id 唯一索引只覆盖未删除的行：商城退款撤单（revoke delete=true）
-- 软删订阅后，同一 order_id 的重试下单（商城的重试队列、重放的 webhook）会再建一份新订阅、
-- 在所有节点上重新开户 —— 退款之后货又发了一次。
--
-- 只记「商城明确撤销」的订单号，不记到期回收：商城续费时若订阅已因到期被清掉，
-- 会用同一 order_id 重新下单补发（node_service._ops_extend_subscription），那条路必须照常可用。
-- 只增不删：撤销是业务事实，不能因为订阅行被清理而变得可以重用。
CREATE TABLE IF NOT EXISTS store_revoked_orders (
    order_id text PRIMARY KEY CHECK (length(order_id) BETWEEN 1 AND 128),
    subscription_id uuid NOT NULL,
    revoked_at timestamptz NOT NULL DEFAULT now()
);
