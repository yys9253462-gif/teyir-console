-- Additive migration. Apply before deploying the revoke backoff.
-- One row per (subscription, node) whose account deletion failed. The node-account
-- mapping in subscription_tokens stays the source of truth for "still to revoke";
-- this table only spaces out retries so a dead node is not dialled every sweep.
-- No foreign key: a row must not vanish together with the subscription.
CREATE TABLE IF NOT EXISTS subscription_revoke_backoff (
    subscription_id uuid NOT NULL,
    node_id uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    last_error text NOT NULL DEFAULT '',
    first_failed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (subscription_id, node_id)
);
CREATE INDEX IF NOT EXISTS subscription_revoke_backoff_node_idx
    ON subscription_revoke_backoff (node_id);
