-- COL-002: keep the absolute counter baseline beside the chart sample writer.
-- The remote legacy JSON is used only to seed this table's first row.
CREATE TABLE IF NOT EXISTS collector_traffic_cursors (
    node_id uuid NOT NULL REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
    source_key text NOT NULL,
    cursor jsonb NOT NULL,
    observed_ns bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, source_key),
    CHECK (observed_ns >= 0)
);
