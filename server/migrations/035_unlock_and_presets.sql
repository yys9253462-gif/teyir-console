-- 035: 流媒体 / AI 解锁检测结果 + 主机预设任务（2026-10-15）。
--
-- 1) node_unlock：节点本机经 portal /api/v1/unlock-check 检测的结果。
--    results 是数组 [{key,name,status,region,note}]，status ∈ yes/partial/no/unknown。
--    与 node_ip_purity 同样的落库原则：检测失败只写 error，不覆盖上次成功的
--    results 与 checked_at。
CREATE TABLE IF NOT EXISTS node_unlock (
    node_id UUID PRIMARY KEY REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    results JSONB NOT NULL DEFAULT '[]'::jsonb,
    error TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2) operation_tasks.operation_type 的 CHECK 自 001 起只有
--    health_check/restart/deploy/config_apply/upgrade/logs，
--    而 API 早已接受 start/stop/reload/enable/disable —— 这些任务入库即被
--    约束拒绝（500）。这里补齐，并加入新的 system_preset（批量预设任务）。
ALTER TABLE operation_tasks DROP CONSTRAINT IF EXISTS operation_tasks_operation_type_check;
ALTER TABLE operation_tasks ADD CONSTRAINT operation_tasks_operation_type_check
    CHECK (operation_type IN ('health_check', 'restart', 'deploy', 'config_apply', 'upgrade', 'logs',
                              'start', 'stop', 'reload', 'enable', 'disable', 'system_preset'));
