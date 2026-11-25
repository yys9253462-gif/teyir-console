-- 052：删除两张采样表上**完全冗余**的索引（2026-10-31）。
--
-- 依据是生产库的 pg_stat_user_indexes 实测，不是"看着像重复"：
--
--   idx_node_speedtests_node_time  btree (node_id, measured_at DESC)     0 次扫描 / 19MB
--   idx_node_speedtests_node_measured btree (node_id, measured_at DESC)
--                                      INCLUDE (round_id)            3541 次扫描 / 21MB
--     ↑ 两列完全相同，后者还覆盖了 round_id。前者从建表起**一次都没被用过**。
--
--   idx_server_metrics_sampled  btree (server_id, sampled_at DESC)     865 次扫描 / 33MB
--   server_metrics_server_id_sampled_at_key btree (server_id, sampled_at) 58.7 万次 / 31MB
--     ↑ 同列同序（唯一索引反向扫描即可提供 DESC）。Postgres 对
--       `WHERE server_id=$1 ORDER BY sampled_at DESC LIMIT 1` 用两者任一都能做，
--       实测绝大多数请求走的是唯一索引。
--
-- 为什么不顺手删 idx_traffic_samples_sampled（30MB）：
--   它有 4.2 万次扫描，是 24 小时区间扫描的主力。虽然 (node_id, sampled_at, source)
--   的唯一索引前缀理论上也能服务，但换计划有变慢风险 —— 省 30MB 不值这个不确定性。
--   留着，并在注释里写明"评估过、故意没删"，免得后人再纠结一次。
--
-- 收益：约 52MB 磁盘，以及三张高频写入表每行少维护一个索引。
-- 风险：DROP INDEX 会短暂拿 ACCESS EXCLUSIVE 锁；这两个索引 20-30MB，
--       重建量级秒级完成。纯增量、幂等（IF EXISTS）。

DROP INDEX IF EXISTS idx_node_speedtests_node_time;
DROP INDEX IF EXISTS idx_server_metrics_sampled;