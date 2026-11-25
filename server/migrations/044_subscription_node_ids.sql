-- 订阅可以直接圈定一组节点（2026-10-17）。
-- NULL / 空数组 = 不按节点圈定（继续看 category_id，二者都空 = 全部上架节点）。
-- 有值时优先于 category_id：只下发这些节点。
ALTER TABLE subscription_tokens ADD COLUMN IF NOT EXISTS node_ids uuid[];
