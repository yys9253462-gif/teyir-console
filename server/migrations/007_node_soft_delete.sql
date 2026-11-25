-- 007: 节点改为软删除。
-- hysteria_nodes 被 traffic_samples / alerts / operation_tasks / node_configs 以
-- ON DELETE RESTRICT 引用，任何产生过采样、告警或任务的节点都删不掉（直接 500）。
-- 与 servers 保持一致：登记只置 deleted_at，历史数据留在库里。
ALTER TABLE hysteria_nodes ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
ALTER TABLE hysteria_nodes DROP CONSTRAINT IF EXISTS hysteria_nodes_server_id_listen_port_key;
CREATE UNIQUE INDEX IF NOT EXISTS hysteria_nodes_server_port_active ON hysteria_nodes (server_id, listen_port) WHERE deleted_at IS NULL;
