-- Additive migration. Apply before deploying the renewal handler.
-- Keep events when subscriptions are deleted: payment IDs must never become reusable.
CREATE TABLE IF NOT EXISTS store_renewal_events (
    renewal_id text PRIMARY KEY CHECK (length(renewal_id) BETWEEN 1 AND 200),
    subscription_id uuid NOT NULL,
    request_payload jsonb NOT NULL,
    result_expires_at timestamptz,
    result_ip_limit integer,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS store_renewal_events_subscription_idx
    ON store_renewal_events (subscription_id);
