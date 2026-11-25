-- 任务租约与取消剩余节点；旧版本只读这些新增字段不会改变既有账户。
ALTER TABLE portal_update_batches ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;
ALTER TABLE portal_update_batches ADD COLUMN IF NOT EXISTS lease_expires_at timestamptz NOT NULL DEFAULT now() + interval '3 minutes';
