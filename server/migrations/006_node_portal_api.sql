-- 006: 支持用 hysteria2-installer 自带 portal 的 /api/v1/node/meta 登记并刷新节点。
-- api_key 能读出节点的 obfs/auth 口令，所以它和 SSH 私钥同级：AES-256-GCM 后存 BYTEA，
-- 任何接口都不回显，只回 has_api 与 URL。
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_url TEXT;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_key BYTEA;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS users_count INTEGER;
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS api_checked_at TIMESTAMPTZ;
