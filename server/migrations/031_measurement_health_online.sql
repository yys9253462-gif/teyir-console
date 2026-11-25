-- 2026-11-08 L17：生产上这张表是早期手工建的，此前没有任何迁移创建它，空库从 001 重放会在这里失败。
-- 结构与东京线上一致（pg_dump --schema-only 核对）；生产库已存在，IF NOT EXISTS 为空操作。
CREATE TABLE IF NOT EXISTS subscription_measured_usage (
    subscription_id uuid NOT NULL REFERENCES subscription_tokens(id) ON DELETE CASCADE,
    node_id uuid NOT NULL,
    user_id text NOT NULL,
    measured_bytes bigint CHECK (measured_bytes >= 0),
    started_at timestamptz,
    source_sampled_at timestamptz,
    collected_at timestamptz NOT NULL DEFAULT now(),
    quality_ok boolean NOT NULL DEFAULT false,
    error text NOT NULL DEFAULT '',
    sources jsonb NOT NULL DEFAULT '{}'::jsonb,
    gaps jsonb NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (subscription_id, node_id)
);
ALTER TABLE subscription_measured_usage ADD COLUMN IF NOT EXISTS current_ok boolean;
ALTER TABLE subscription_measured_usage ADD COLUMN IF NOT EXISTS online_devices bigint;
