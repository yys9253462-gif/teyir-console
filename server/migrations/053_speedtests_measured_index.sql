-- 053：给 node_speedtests 补一个以 measured_at 开头的索引（2026-11-02）。
--
-- 依据是生产库实测，不是"看着该有"：
--
--   chinaLossConditionsSQL（china_quality.go）每轮告警评估都跑，约每分钟 2 次：
--     WHERE st.measured_at > now() - interval '1 hour' AND carrier IN (...) AND probe_ok
--   现有索引全部以 node_id / server_id 开头，这条跨节点的时间窗查询用不上，
--   EXPLAIN ANALYZE：Seq Scan，扫 32.7 万行只留 600 行，170ms / 次。
--   pg_stat_user_tables.seq_scan 两分钟 +4、seq_tup_read 两分钟 +131 万行，与之吻合。
--
-- 不用 CONCURRENTLY：cmd/migrate 把每个文件包在一个事务里，CONCURRENTLY 不能在事务内执行。
-- 表 59MB，普通 CREATE INDEX 秒级完成；期间阻塞写入，而这张表只在测速轮次写入，可接受。
-- 纯增量、幂等（IF NOT EXISTS）。回滚：DROP INDEX IF EXISTS idx_node_speedtests_measured;

CREATE INDEX IF NOT EXISTS idx_node_speedtests_measured ON node_speedtests (measured_at);
