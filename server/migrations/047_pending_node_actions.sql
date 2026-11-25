-- Additive migration. Apply before deploying the pending node-action worker.
-- A subscription whose node-side enforcement did not complete:
--   action='revoke'  shop refund/revoke: delete the buyer's accounts on every node
--   action='disable' admin stop: set the accounts to disabled (reversible)
-- The worker retries with backoff until every node confirms, or until the
-- subscription is active again (renewed / re-enabled), which cancels the action.
-- No foreign key on purpose: the row must not vanish before the work is done.
CREATE TABLE IF NOT EXISTS subscription_pending_node_actions (
    subscription_id uuid PRIMARY KEY,
    action text NOT NULL CHECK (action IN ('revoke', 'disable')),
    then_delete boolean NOT NULL DEFAULT false,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscription_pending_node_actions_due_idx
    ON subscription_pending_node_actions (next_attempt_at);
