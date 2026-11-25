-- 055：撤掉 054 加的 server_metrics(sampled_at) 单列索引（2026-11-02 审计 M2 返工）。
--
-- 054 在生产库 CONCURRENTLY 建好后实测：规划器会在
--   `WHERE server_id=$1 ORDER BY sampled_at DESC LIMIT 1`（/api/servers 每台主机一次）
-- 上改走该索引反向扫描、再逐行过滤 server_id，单台 28ms，/api/servers 从 40ms
-- 退化到 500ms（采样稀疏的主机会更差）。生产库已手工 DROP INDEX CONCURRENTLY，
-- 本文件对生产库是幂等空操作；新建的库靠它保持与生产一致。
--
-- M2 改为在查询侧解决：recentMetrics 与 /api/runtime 的 MAX(sampled_at)
-- 都改写成按主机走 (server_id, sampled_at) 唯一索引（见 server.go recentMetrics、
-- manage.go runtimeStatus），不再全表扫 server_metrics。

DROP INDEX IF EXISTS idx_server_metrics_sampled_at;
