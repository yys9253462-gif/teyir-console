-- 057：显式标记面板自建的「合并订阅」（2026-11-06）。
--
-- 原来按「最早一条没有 order_id 的行」认定合并订阅。订阅中心手工开给客户的订阅同样没有 order_id：
-- 合并订阅那一行被删掉后（或首次打开订阅中心之前已经手工开过订阅），最早的手工客户订阅就被当成
-- 合并订阅 —— 只读账号能看到它的链接，「换新链接」让客户的链接 404，「停用」把客户在所有节点上停掉。
--
-- 回填：把当前代码会选中的那一行（最早一条未删除、无 order_id 的行）标记为合并订阅，行为不变。
ALTER TABLE subscription_tokens ADD COLUMN IF NOT EXISTS is_panel_merged boolean NOT NULL DEFAULT false;

UPDATE subscription_tokens SET is_panel_merged = true
 WHERE id = (SELECT id FROM subscription_tokens
              WHERE deleted_at IS NULL AND COALESCE(order_id, '') = ''
              ORDER BY created_at ASC, id ASC LIMIT 1)
   AND NOT EXISTS (SELECT 1 FROM subscription_tokens WHERE is_panel_merged AND deleted_at IS NULL);

-- 同一时刻只能有一条在用的合并订阅；并发首次创建时 ON CONFLICT DO NOTHING 靠它收敛到一行。
CREATE UNIQUE INDEX IF NOT EXISTS subscription_tokens_panel_merged_uniq
    ON subscription_tokens (is_panel_merged) WHERE is_panel_merged AND deleted_at IS NULL;
