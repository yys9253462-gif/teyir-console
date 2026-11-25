-- 批量门户更新状态持久化；只保存进度和文件指纹，不保存节点凭据。
CREATE TABLE IF NOT EXISTS portal_update_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status text NOT NULL CHECK (status IN ('running','completed','interrupted')),
    results jsonb NOT NULL,
    expected jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- 全局仅一个批次在执行，避免重复点击、多个标签页重复升级。
CREATE UNIQUE INDEX IF NOT EXISTS portal_update_one_running
ON portal_update_batches ((true)) WHERE status = 'running';
