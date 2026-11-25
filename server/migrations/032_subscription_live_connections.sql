CREATE TABLE IF NOT EXISTS subscription_live_connections (
 subscription_id uuid NOT NULL REFERENCES subscription_tokens(id) ON DELETE CASCADE,
 node_id uuid NOT NULL REFERENCES hysteria_nodes(id) ON DELETE CASCADE,
 user_id text NOT NULL,
 ips jsonb NOT NULL DEFAULT '[]'::jsonb,
 ok boolean NOT NULL DEFAULT false,
 error text NOT NULL DEFAULT '',
 hysteria_ok boolean NOT NULL DEFAULT false,
 reality_ok boolean NOT NULL DEFAULT false,
 reality_error text NOT NULL DEFAULT '',
 collected_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(subscription_id,node_id)
);
