package httpapi

// retention.go — 时序采样表的数据保留（2026-10-14）。
//
// 此前这几张表只写不删：server_metrics / node_speedtests / traffic_samples
// 每台机器每 30 秒到每分钟一行，13 天已到 ~18 万行、120MB，一年会过 3GB，
// 且所有读取路径最多只回看 7 天（见各处 interval '24 hours' / '7 days'）。
// 保留 30 天：比最长读取窗口宽出足够余量，也够事后排查一个月内的问题。
//
// audit_logs / alert_events 不在这里清理 —— 它们体量很小，且是追责依据。
//
// ⚠️ 2026-10-24 审计 P1-2 修：**DELETE 不等于空间归还**。
//
// 上面这段注释写于 10-14，但从那之后三张表一直涨到 314MB（35 万行），
// 而 audit 时全库 grep 不到任何一次实际生效的清理记录 —— 原因不是保留期
// 太宽（数据才 24 天，确实还没到 30 天），而是这套代码有个更隐蔽的漏洞：
// PostgreSQL 的 DELETE 只把行标成死元组，**数据页不会消失**。没有 VACUUM，
// 文件大小只增不减，`pg_total_relation_size` 一年后仍然是 3GB。
// 也就是说：即便保留期到了、行删掉了，磁盘也一点没回来。
//
// 现在的处理：
//   1. 删完立刻对这张表做一次 VACUUM ANALYZE（非 FULL，不锁表、不重写文件），
//      把死元组空间标成可复用 —— 新采样会填进这些空洞，文件不再单调增长。
//   2. 不清空整表时**不做 VACUUM FULL**：它会拿 ACCESS EXCLUSIVE 锁，
//      期间采集器写不进数据、面板查询全部卡住，得不偿失。
//   3. 每轮清理后把三张表的实际占用记进日志。留一条可核对的数字，
//      下次谁再问「空间去哪了」不用重新排查一遍。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	sampleRetention     = 30 * 24 * time.Hour
	retentionInterval   = 6 * time.Hour
	retentionBatchLimit = 5000
)

// retentionTables 只列「时间列 + 主键」明确的采样表；表名与列名是常量，不来自输入。
var retentionTables = []struct{ table, timeColumn string }{
	{"server_metrics", "sampled_at"},
	{"traffic_samples", "sampled_at"},
	{"node_speedtests", "measured_at"},
}

// StartSampleRetention 定期清理过期采样。启动后先等一会再跑，避免和启动时的
// 采集、迁移挤在一起。
func (s *Server) StartSampleRetention(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
		ticker := time.NewTicker(retentionInterval)
		defer ticker.Stop()
		for {
			s.pruneSamples(ctx, time.Now().Add(-sampleRetention))
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) pruneSamples(ctx context.Context, cutoff time.Time) {
	for _, t := range retentionTables {
		var total int64
		if t.table == "server_metrics" {
			total = s.pruneServerMetrics(ctx, cutoff)
		} else if t.table == "traffic_samples" {
			total = s.pruneTrafficSamples(ctx, cutoff)
		} else {
			total = s.pruneBatches(ctx, t.table, `DELETE FROM `+t.table+` WHERE ctid IN (
				SELECT ctid FROM `+t.table+` WHERE `+t.timeColumn+` < $1 LIMIT $2)`, cutoff)
		}
		if total > 0 {
			s.logger.Info("retention: pruned old samples", "table", t.table, "rows", total)
		}
		// 删完就 VACUUM：只在真的删掉行时才做（没删东西就没死元组要回收，
		// 白跑一次 vacuum 只是给库添负担）。
		//
		// VACUUM 不能在事务块里跑，所以走独立的 Exec 并且**不接受参数**
		// （pgx 对 VACUUM + 占位符会报错）。表名来自上面的常量白名单，
		// 不是外部输入，拼接是安全的 —— 这一点必须写清楚，
		// 否则下一个人会以为这里有个 SQL 注入。
		if total > 0 {
			s.vacuumTable(ctx, t.table)
		}
	}
	s.logRetentionFootprint(ctx)
}

// retentionVacuumTimeout 限制单次 VACUUM ANALYZE 的时长：它和分批 DELETE
// 共用后台 goroutine，卡住会挂住整个保留期循环。
var retentionVacuumTimeout = 10 * time.Minute

// vacuumExec 便于测试替换；默认走连接池。
var vacuumExec = func(s *Server, ctx context.Context, sql string) error {
	_, err := s.db.Exec(ctx, sql)
	return err
}

// vacuumTable 给每次 VACUUM 套独立超时，超时或失败只记 Warn，继续下一张表。
func (s *Server) vacuumTable(ctx context.Context, table string) {
	vctx, cancel := context.WithTimeout(ctx, retentionVacuumTimeout)
	defer cancel()
	if err := vacuumExec(s, vctx, `VACUUM ANALYZE `+table); err != nil {
		if errors.Is(vctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			s.logger.Warn("retention: vacuum timed out", "table", table, "timeout", retentionVacuumTimeout.String(), "error", err)
			return
		}
		s.logger.Warn("retention: vacuum failed", "table", table, "error", err)
	}
}

// logRetentionFootprint 把三张采样表的实际占用写进日志（每轮一次）。
//
// 为什么值得为一行日志开一个函数：这三张表是整库唯一会无限增长的，
// 它们的曲线就是「磁盘什么时候会满」的答案。Audit 时为了回答这个问题，
// 不得不一台台机器去 psql 里手查 —— 有这条日志就不用。
func (s *Server) logRetentionFootprint(ctx context.Context) {
	rows, err := s.db.Query(ctx, `
SELECT relname, pg_total_relation_size(relid), n_live_tup, n_dead_tup
  FROM pg_stat_user_tables
 WHERE relname = ANY($1)`, retentionTableNames())
	if err != nil {
		s.logger.Warn("retention: 读取表占用失败", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var bytes, live, dead int64
		if err := rows.Scan(&name, &bytes, &live, &dead); err != nil {
			s.logger.Warn("retention: footprint row scan failed; remaining table stats omitted", "error", err)
			return
		}
		s.logger.Info("retention: footprint", "table", name,
			"size_mb", fmt.Sprintf("%.1f", float64(bytes)/(1<<20)),
			"rows", live, "dead", dead)
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("retention: footprint rows ended incomplete", "error", err)
	}
}

// retentionTableNames 给 logRetentionFootprint 用：从同一份白名单取表名，
// 避免「清理了哪些表」与「统计了哪些表」两份清单各写一遍、改一处漏一处。
func retentionTableNames() []string {
	out := make([]string, 0, len(retentionTables))
	for _, t := range retentionTables {
		out = append(out, t.table)
	}
	return out
}

// pruneBatches 分批删：一次大 DELETE 会长时间持锁并产生巨量 WAL，期间采集器的写入会被阻塞。
// query 的 $1 为截止时间、$2 为批大小，最后一个参数可选为附加条件值。
func (s *Server) pruneBatches(ctx context.Context, table, query string, args ...any) int64 {
	var total int64
	params := append([]any{args[0], retentionBatchLimit}, args[1:]...)
	for {
		tag, err := s.db.Exec(ctx, query, params...)
		if err != nil {
			s.logger.Warn("retention: prune failed", "table", table, "error", err)
			break
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < retentionBatchLimit || ctx.Err() != nil {
			break
		}
	}
	return total
}

// retentionOwnerIDs 列出逐个清理时要遍历的 id。
func (s *Server) retentionOwnerIDs(ctx context.Context, query, what string) ([]string, bool) {
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		s.logger.Warn("retention: list "+what+" failed", "error", err)
		return nil, false
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("retention: list "+what+" failed", "error", err)
		return nil, false
	}
	return ids, true
}

// pruneTrafficSamples 按节点逐个删除（2026-11-04，与 pruneServerMetrics 同理）。
//
// 054 删掉了 traffic_samples 上唯一以 sampled_at 打头之外的辅助索引，剩下的是
// 主键 id 与唯一键 (node_id, sampled_at, source)。「WHERE sampled_at < $1 LIMIT 5000」
// 每批都顺序扫全表：生产实测 59 万行、每批 150ms，只为找出 220 条过期行。
// 带上 node_id 后走唯一键前缀。节点要包括已软删的 —— traffic_samples.node_id 是
// ON DELETE RESTRICT，软删节点的历史行同样要按期清理。
func (s *Server) pruneTrafficSamples(ctx context.Context, cutoff time.Time) int64 {
	ids, ok := s.retentionOwnerIDs(ctx, `SELECT id::text FROM hysteria_nodes`, "nodes")
	if !ok {
		return 0
	}
	var total int64
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		total += s.pruneBatches(ctx, "traffic_samples", `DELETE FROM traffic_samples WHERE ctid IN (
			SELECT ctid FROM traffic_samples WHERE node_id = $3::uuid AND sampled_at < $1 LIMIT $2)`, cutoff, id)
	}
	return total
}

// pruneServerMetrics 按主机逐台删除（2026-11-02 B6）。
//
// 055 撤掉了 server_metrics(sampled_at) 单列索引，原先「WHERE sampled_at < $1 LIMIT 5000」
// 每一批都要顺序扫一遍 56 万行。改成 server_id=$3 AND sampled_at<$1 之后，
// 每批都能走 (server_id, sampled_at) 唯一索引的前缀，不需要重新加回那个会让
// /api/servers 计划退化的单列索引。主机只有几十台，逐台循环开销可以忽略。
// server_metrics.server_id 是 ON DELETE RESTRICT，表里的行一定属于 servers 中的主机。
func (s *Server) pruneServerMetrics(ctx context.Context, cutoff time.Time) int64 {
	ids, ok := s.retentionOwnerIDs(ctx, `SELECT id::text FROM servers`, "servers")
	if !ok {
		return 0
	}
	var total int64
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		total += s.pruneBatches(ctx, "server_metrics", `DELETE FROM server_metrics WHERE ctid IN (
			SELECT ctid FROM server_metrics WHERE server_id = $3::uuid AND sampled_at < $1 LIMIT $2)`, cutoff, id)
	}
	return total
}
