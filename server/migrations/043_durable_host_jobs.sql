CREATE TABLE IF NOT EXISTS host_operation_jobs (
 id text PRIMARY KEY,
 payload jsonb NOT NULL,
 status text NOT NULL,
 owner text NOT NULL,
 heartbeat timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS host_operation_leases (
 server_id uuid PRIMARY KEY REFERENCES servers(id),
 job_id text NOT NULL REFERENCES host_operation_jobs(id)
);
CREATE INDEX IF NOT EXISTS host_operation_jobs_heartbeat_idx ON host_operation_jobs(status,heartbeat);
