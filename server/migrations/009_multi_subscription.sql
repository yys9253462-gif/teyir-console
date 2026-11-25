-- 009: 多订阅、商城对接与到期控制。
--
-- 扩展 subscription_tokens 表，从最初的「单机唯一合并订阅」升级为
-- 「多订阅 / 商城交付体系」：
--   1. 移除全局单例索引，允许创建多条独立的客户订阅；
--   2. 支持到期时间 (expires_at) 与到期自动删除/停用 (auto_delete)；
--   3. 支持 IP 数量限制 (ip_limit)，记录活跃 IP 列表 (active_ips)；
--   4. 关联商城订单号 (order_id) 与买家联系方式 (user_email)；
--   5. 引入软删除 (deleted_at) 与系统设置表 (system_settings)。

-- 1. 移除单例索引，支持多订阅并发共存
DROP INDEX IF EXISTS idx_subscription_tokens_singleton;

-- 2. 扩充订阅字段
ALTER TABLE subscription_tokens
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ip_limit INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS auto_delete BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS order_id TEXT,
    ADD COLUMN IF NOT EXISTS user_email TEXT,
    ADD COLUMN IF NOT EXISTS note TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS active_ips JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- 3. 索引
CREATE INDEX IF NOT EXISTS idx_subscription_tokens_token_active
    ON subscription_tokens (token) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_subscription_tokens_order_id
    ON subscription_tokens (order_id) WHERE deleted_at IS NULL AND order_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_subscription_tokens_expires
    ON subscription_tokens (expires_at) WHERE deleted_at IS NULL;

-- 4. 系统设置表（用于持久化商城对接密钥与全局交付配置）
CREATE TABLE IF NOT EXISTS system_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
