ALTER TABLE servers ADD COLUMN allow_write_ops BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE servers ADD COLUMN latency_ms INTEGER;

CREATE INDEX idx_server_credentials_server ON server_credentials(server_id, created_at DESC);
CREATE INDEX idx_operation_tasks_queued ON operation_tasks(created_at) WHERE status = 'queued';
