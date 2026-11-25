-- 059：任务列表按 created_at 倒序取前 N 条，但没有可用索引（2026-11-06）。
--
-- operation_tasks 上的索引是 (status, created_at DESC) 与部分索引 (created_at) WHERE status='queued'：
-- 前导列不是 created_at，而 tasks 端点的查询不带 status 过滤 ⇒ 只能全表扫 + 排序。
-- 这张表只增不减（没有 DELETE 路径），任务页与批量预设轮询器每几秒打一次，
-- 表越大越慢。补一条纯 created_at 倒序索引覆盖这种「最近 N 条」查询。
CREATE INDEX IF NOT EXISTS idx_operation_tasks_created_at
    ON operation_tasks (created_at DESC);
