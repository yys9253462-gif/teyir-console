CREATE TABLE IF NOT EXISTS store_signature_nonces (
 nonce_hash text PRIMARY KEY,
 expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS store_signature_nonces_expires_idx ON store_signature_nonces(expires_at);
INSERT INTO system_settings(key,value,description)
VALUES('store_signature_legacy_until',extract(epoch from now()+interval '1 hour')::bigint::text,'签名v1临时兼容截止，商城升级v2后设为0')
ON CONFLICT(key) DO NOTHING;
CREATE TABLE IF NOT EXISTS subscription_sync_jobs (
 subscription_id uuid PRIMARY KEY REFERENCES subscription_tokens(id) ON DELETE CASCADE,
 status text NOT NULL DEFAULT 'pending',
 error text NOT NULL DEFAULT '',
 attempts int NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscription_sync_jobs_retry_idx ON subscription_sync_jobs(status,next_attempt);
-- 与权益变更同事务入队，进程崩溃也不会丢失节点同步任务。
CREATE OR REPLACE FUNCTION enqueue_subscription_sync() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO subscription_sync_jobs(subscription_id,status,next_attempt)
 VALUES(NEW.id,CASE WHEN NEW.enabled AND (NEW.expires_at IS NULL OR NEW.expires_at>now()) AND NEW.deleted_at IS NULL THEN 'pending' ELSE 'cancelled' END,now())
 ON CONFLICT(subscription_id) DO UPDATE SET status=EXCLUDED.status,error='',next_attempt=now(),updated_at=now();
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS subscription_sync_outbox ON subscription_tokens;
CREATE TRIGGER subscription_sync_outbox AFTER INSERT OR UPDATE OF expires_at,enabled,ip_limit,category_id,deleted_at ON subscription_tokens FOR EACH ROW EXECUTE FUNCTION enqueue_subscription_sync();
