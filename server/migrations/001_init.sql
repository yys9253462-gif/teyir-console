CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'operator', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE servers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    host TEXT NOT NULL,
    ssh_port INTEGER NOT NULL DEFAULT 22 CHECK (ssh_port BETWEEN 1 AND 65535),
    ssh_user TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'online', 'warning', 'offline')),
    host_fingerprint TEXT,
    os_info JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (host, ssh_port)
);

CREATE TABLE server_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
    auth_type TEXT NOT NULL CHECK (auth_type IN ('ssh_key', 'password')),
    ciphertext BYTEA NOT NULL,
    key_version TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE hysteria_nodes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    listen_port INTEGER NOT NULL CHECK (listen_port BETWEEN 1 AND 65535),
    domain TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'online', 'warning', 'offline')),
    last_checked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server_id, listen_port)
);

CREATE TABLE node_configs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id UUID NOT NULL REFERENCES hysteria_nodes(id) ON DELETE RESTRICT,
    version INTEGER NOT NULL,
    config_ciphertext BYTEA NOT NULL,
    config_hash TEXT NOT NULL,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    apply_status TEXT NOT NULL DEFAULT 'pending' CHECK (apply_status IN ('pending', 'applied', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at TIMESTAMPTZ,
    UNIQUE (node_id, version)
);

CREATE TABLE server_metrics (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
    sampled_at TIMESTAMPTZ NOT NULL,
    cpu_percent REAL NOT NULL CHECK (cpu_percent BETWEEN 0 AND 100),
    memory_used_bytes BIGINT NOT NULL CHECK (memory_used_bytes >= 0),
    memory_total_bytes BIGINT NOT NULL CHECK (memory_total_bytes >= 0),
    disk_used_bytes BIGINT NOT NULL CHECK (disk_used_bytes >= 0),
    disk_total_bytes BIGINT NOT NULL CHECK (disk_total_bytes >= 0),
    load_average JSONB NOT NULL DEFAULT '[]'::jsonb,
    UNIQUE (server_id, sampled_at)
);

CREATE TABLE traffic_samples (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id UUID NOT NULL REFERENCES hysteria_nodes(id) ON DELETE RESTRICT,
    sampled_at TIMESTAMPTZ NOT NULL,
    rx_bytes BIGINT NOT NULL CHECK (rx_bytes >= 0),
    tx_bytes BIGINT NOT NULL CHECK (tx_bytes >= 0),
    source TEXT NOT NULL,
    UNIQUE (node_id, sampled_at, source)
);

CREATE TABLE operation_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID NOT NULL REFERENCES servers(id) ON DELETE RESTRICT,
    node_id UUID REFERENCES hysteria_nodes(id) ON DELETE RESTRICT,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    operation_type TEXT NOT NULL CHECK (operation_type IN ('health_check', 'restart', 'deploy', 'config_apply', 'upgrade', 'logs')),
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'success', 'failed', 'cancelled')),
    parameters JSONB NOT NULL DEFAULT '{}'::jsonb,
    result_summary TEXT NOT NULL DEFAULT '',
    error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);

CREATE TABLE task_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id UUID NOT NULL REFERENCES operation_tasks(id) ON DELETE RESTRICT,
    sequence INTEGER NOT NULL,
    logged_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    level TEXT NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message TEXT NOT NULL,
    UNIQUE (task_id, sequence)
);

CREATE TABLE alerts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    server_id UUID REFERENCES servers(id) ON DELETE RESTRICT,
    node_id UUID REFERENCES hysteria_nodes(id) ON DELETE RESTRICT,
    type TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'acknowledged', 'resolved')),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    CHECK (server_id IS NOT NULL OR node_id IS NOT NULL)
);

CREATE TABLE alert_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alert_id UUID NOT NULL REFERENCES alerts(id) ON DELETE RESTRICT,
    event_type TEXT NOT NULL CHECK (event_type IN ('triggered', 'repeated', 'acknowledged', 'resolved')),
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_logs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID,
    ip_address INET,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_servers_status ON servers(status) WHERE deleted_at IS NULL;
CREATE INDEX idx_server_metrics_sampled ON server_metrics(server_id, sampled_at DESC);
CREATE INDEX idx_traffic_samples_sampled ON traffic_samples(node_id, sampled_at DESC);
CREATE INDEX idx_operation_tasks_status_created ON operation_tasks(status, created_at DESC);
CREATE INDEX idx_task_logs_task_sequence ON task_logs(task_id, sequence);
CREATE INDEX idx_alerts_status_severity ON alerts(status, severity, last_seen_at DESC);
CREATE INDEX idx_audit_logs_created ON audit_logs(created_at DESC);
