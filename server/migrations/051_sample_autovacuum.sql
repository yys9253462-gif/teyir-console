-- 051：高频时序表的 autovacuum 按表收紧（2026-10-24 审计 P3-3）。
--
-- 为什么需要（实测数据，2026-10-24）：
--   server_metrics  35.2 万行 / 105MB，每 30 秒 × 12 台主机 ≈ 每分钟 24 行
--   traffic_samples 30.7 万行 /  87MB
--   node_speedtests 20.1 万行 / 122MB
--
-- 三张表用的全是**全局默认** autovacuum 参数：
--   autovacuum_vacuum_scale_factor = 0.2、threshold = 50
-- 触发条件是 dead >= 50 + 0.2 × live。对 server_metrics 来说等于要攒
-- **7 万个死元组**才清一次。而保留策略（retention.go，30 天）每次
-- 批量删 5000 行 —— 删了 5 万行都还够不到真空门槛。
--
-- 后果不是"多占一点空间"，而是两层叠加：
--   1. 死元组长期滞留，索引和堆都在膨胀，顺序扫描要越过大量死行；
--   2. 空间永远不回到操作系统（这是 2026-10-24 审计里"314MB 且只增不减"
--      的直接成因 —— 不只是没 DELETE，更是删了也不回收）。
--
-- 调参依据（不是拍脑袋）：
--   scale_factor 0.2 → 0.02：阈值从 7 万降到 7 千行，约等于"半天到一天"的量，
--     与 6 小时一轮的清理节奏匹配，不会比清理跑得还勤。
--   threshold 50 → 1000：小表（节点数少时）不至于每几行就触发一次；
--     对 20 万行的表来说这个基数可以忽略。
--   autovacuum_vacuum_cost_delay 0 → 2ms 是 PG 的默认值，这里**不动** ——
--     调成 0 会让 vacuum 跑满 IO，影响采集写入，得不偿失。
--
-- 只改这三张**只增不查历史**的采样表；业务表（alerts、subscription_tokens）
-- 行数少、读多写少，默认参数是合适的，改它们只会增加不必要的 autovacuum 频率。
--
-- 纯增量、幂等：ALTER TABLE SET (...) 可重复执行，不重建表、不锁业务写入。

ALTER TABLE server_metrics   SET (autovacuum_vacuum_scale_factor = 0.02,
                                  autovacuum_vacuum_threshold = 1000,
                                  autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE traffic_samples  SET (autovacuum_vacuum_scale_factor = 0.02,
                                  autovacuum_vacuum_threshold = 1000,
                                  autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE node_speedtests  SET (autovacuum_vacuum_scale_factor = 0.02,
                                  autovacuum_vacuum_threshold = 1000,
                                  autovacuum_analyze_scale_factor = 0.01);
