-- Additive migration: indexes for two periodic queries.
-- Both tables are small today, so plain CREATE INDEX finishes instantly.
-- detectSharing / live-connection reads filter by recency.
CREATE INDEX IF NOT EXISTS subscription_live_connections_collected_idx
    ON subscription_live_connections (collected_at);
-- The health gate looks up open node_offline / node_blocked alerts every 30 seconds.
CREATE INDEX IF NOT EXISTS alerts_open_node_type_idx
    ON alerts (node_id, type) WHERE status <> 'resolved';
