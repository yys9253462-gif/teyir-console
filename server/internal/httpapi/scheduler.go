package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// 定时巡检（2026-10-16）。
//
// 三个任务，各自可开关、可立即运行；结果**有变化才**推送 Telegram，平时安静：
//   unlock_daily   每天：流媒体 / AI 解锁检测（某项从可用变成不可用，或反过来）
//   purity_weekly  每周：IP 纯净度（欺诈分涨跌 ≥20、原生 / 广播翻转、新出现代理/VPN 标记）
//   audit_weekly   每周：只读安全检查（评分 < 80 的主机）
// 调度器每小时醒一次，看各任务距上次运行是否已到间隔。上次运行时间存 system_settings，
// 面板重启不会让任务重复跑。

type scheduleJob struct {
	Key      string
	Label    string
	Interval time.Duration
	run      func(s *Server, ctx context.Context) string // 返回要推送的消息（空串 = 无变化）
}

var scheduleJobs = []scheduleJob{
	{"unlock_daily", "每日解锁检测", 24 * time.Hour, (*Server).scheduledUnlock},
	{"purity_weekly", "每周 IP 纯净度检测", 7 * 24 * time.Hour, (*Server).scheduledPurity},
	{"audit_weekly", "每周安全检查", 7 * 24 * time.Hour, (*Server).scheduledAudit},
}

const scheduleConfigKey = "schedule_config"

var scheduleRunning sync.Map // key -> bool

func (s *Server) scheduleConfig(ctx context.Context) (map[string]bool, error) {
	cfg := map[string]bool{}
	for _, j := range scheduleJobs {
		cfg[j.Key] = true // 默认开启：都只在有变化时才通知
	}
	var raw string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, scheduleConfigKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var saved map[string]bool
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return nil, fmt.Errorf("invalid schedule config: %w", err)
	}
	for k, v := range saved {
		if _, known := cfg[k]; known {
			cfg[k] = v
		}
	}
	return cfg, nil
}

func (s *Server) settingTime(ctx context.Context, key string) (*time.Time, error) {
	var raw string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, key).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("invalid schedule time %q: %w", key, err)
	}
	return &t, nil
}

// scheduleSettingTime 区分「没有记录」与「读取失败」，避免数据库错误触发重复巡检。
func (s *Server) scheduleSettingTime(ctx context.Context, key string) (*time.Time, error) {
	return s.settingTime(ctx, key)
}

// putSetting 写一项设置；失败时记日志并把错误返回给需要据此判断的调用方
// （例如 schedule_last_*：写不进去意味着重启后任务会重复跑）。
func (s *Server) putSetting(ctx context.Context, key, value, desc string) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, value, desc)
	if err != nil {
		s.logger.Warn("setting write failed", "key", key, "error", err)
	}
	return err
}

// scheduleDBTimeout：调度器每一步读写设置的上限。原来直接用进程级 ctx，
// 数据库卡住时调度 goroutine 会无限期挂住，之后所有巡检都不再跑。
const scheduleDBTimeout = 30 * time.Second

// putScheduleSetting 用独立的短超时写调度状态。刻意不继承任务的 runCtx：
// 任务跑满 30 分钟超时后 runCtx 已取消，再用它写「最近运行时间」必然失败。
func (s *Server) putScheduleSetting(parent context.Context, key, value, desc string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), scheduleDBTimeout)
	defer cancel()
	return s.putSetting(ctx, key, value, desc)
}

// scheduleAttemptLease 是一次执行占用「正在跑」标记的时长：略长于单次执行上限（30 分钟）。
const scheduleAttemptLease = 35 * time.Minute

// claimScheduleAttempt 原子地占下这次执行（2026-11-06）。原来互斥只靠进程内的 scheduleRunning，
// schedule_attempt_* 只写不读：每周纯净度（消耗第三方配额，最长 30 分钟）跑到一半时重启 / 部署 api，
// 新进程 3 分钟后发现 last 没更新就整轮重跑；两个进程同时在线（重建容器的重叠期）时也各跑一遍。
// 现在只有「最近一次尝试已超过租期」时才能占下，占不到就跳过。
func (s *Server) claimScheduleAttempt(parent context.Context, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), scheduleDBTimeout)
	defer cancel()
	now := time.Now().UTC()
	tag, err := s.db.Exec(ctx, `INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '巡检最近尝试时间')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
WHERE system_settings.value = '' OR system_settings.value IS NULL
   OR NOT (system_settings.value ~ '^\d{4}-\d{2}-\d{2}T')
   OR system_settings.value::timestamptz < $3`,
		"schedule_attempt_"+key, now.Format(time.RFC3339), now.Add(-scheduleAttemptLease))
	if err != nil {
		s.logger.Warn("schedule: claim attempt failed", "job", key, "error", err)
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

type scheduleSetting struct{ key, value, desc string }

// 完成时间与重试状态必须一起提交；部分写入会让已成功的任务继续重试。
func (s *Server) putScheduleSettings(parent context.Context, settings []scheduleSetting) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), scheduleDBTimeout)
	defer cancel()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, v := range settings {
		if _, err := tx.Exec(ctx, `INSERT INTO system_settings (key, value, description) VALUES ($1,$2,$3)
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, v.key, v.value, v.desc); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Server) runScheduleWork(ctx context.Context, j scheduleJob) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			scheduleFailed(ctx)
			s.logger.Error("schedule: job panic", "job", j.Key, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	return j.run(s, ctx)
}

// StartScheduler 每小时检查一次哪些任务到点了。
func (s *Server) StartScheduler(ctx context.Context) {
	go func() {
		sleepCtx(ctx, 3*time.Minute) // 启动后稍等，避开部署时的抖动
		for ctx.Err() == nil {
			s.schedulerTick(ctx)
			sleepCtx(ctx, time.Minute)
			if ctx.Err() != nil {
				return
			}
		}
	}()
}

func (s *Server) schedulerTick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("schedule: tick panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	// 读配置与到点判断都在 30 秒上限内完成；任务本身另有 30 分钟预算（runScheduleJob）。
	readCtx, cancel := context.WithTimeout(ctx, scheduleDBTimeout)
	cfg, cfgErr := s.scheduleConfig(readCtx)
	var due []scheduleJob
	if cfgErr != nil {
		s.logger.Warn("schedule: read task configuration failed", "error", cfgErr)
	} else {
		for _, j := range scheduleJobs {
			if !cfg[j.Key] {
				continue
			}
			retry, err := s.scheduleSettingTime(readCtx, "schedule_retry_"+j.Key)
			if err != nil {
				s.logger.Warn("schedule: read retry failed", "job", j.Key, "error", err)
				continue
			}
			if retry != nil {
				if time.Now().Before(*retry) {
					continue
				}
				due = append(due, j)
				continue
			}
			last, err := s.scheduleSettingTime(readCtx, "schedule_last_"+j.Key)
			if err != nil {
				s.logger.Warn("schedule: read last run failed", "job", j.Key, "error", err)
				continue
			}
			if last != nil && time.Since(*last) < j.Interval {
				continue
			}
			if readCtx.Err() != nil {
				break // 读设置超时：宁可这一轮不跑，也不把「读不到」当成「从没跑过」
			}
			due = append(due, j)
		}
	}
	if cfgErr != nil || readCtx.Err() != nil {
		due = nil // 配置或到点状态读取不完整，不执行本轮任务
	}
	cancel()
	for _, j := range due {
		s.runScheduleJob(ctx, j)
	}
	// 上一轮安全检查任务跑完了没有 —— 跑完就汇总推送。
	digestCtx, cancelDigest := context.WithTimeout(ctx, scheduleDBTimeout)
	s.collectAuditDigest(digestCtx)
	cancelDigest()
}

func (s *Server) runScheduleJob(ctx context.Context, j scheduleJob) {
	if _, busy := scheduleRunning.LoadOrStore(j.Key, true); busy {
		return
	}
	defer scheduleRunning.Delete(j.Key)
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("schedule: state panic", "job", j.Key, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if ctx.Err() != nil {
		return
	}
	claimed, err := s.claimScheduleAttempt(ctx, j.Key)
	if err != nil {
		return // 无法记账时不执行有配额消耗的任务
	}
	if !claimed {
		s.logger.Info("schedule: 另一进程（或重启前的本进程）正在执行，本轮跳过", "job", j.Key)
		return
	}
	// 结束（成功 / 失败 / 记账失败）时都放掉占用，否则 5 分钟后的退避重试会被 35 分钟的租期挡住。
	// 进程中途被杀时放不掉 —— 那正是租期要兜住的情况。
	defer func() {
		if err := s.putScheduleSetting(ctx, "schedule_attempt_"+j.Key, "", "巡检最近尝试时间（已结束）"); err != nil {
			s.logger.Warn("schedule: release attempt failed", "job", j.Key, "error", err)
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	state := &scheduleRunState{}
	runCtx = context.WithValue(runCtx, scheduleRunKey{}, state)
	msg := s.runScheduleWork(runCtx, j)
	var settings []scheduleSetting
	var exhausted bool
	if runCtx.Err() != nil {
		state.inc()
	}
	failedCount := state.count()
	if failedCount > 0 {
		// 重试有上限（2026-10-16 审查后加）：最多重试 3 次，间隔 5 → 15 → 45 分钟；仍失败就放弃，等下一个正常周期。
		readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), scheduleDBTimeout)
		var raw string
		err := s.db.QueryRow(readCtx, `SELECT value FROM system_settings WHERE key=$1`, "schedule_retry_count_"+j.Key).Scan(&raw)
		cancelRead()
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Error("schedule: 读取重试次数失败", "job", j.Key, "error", err)
			return
		}
		previous, err := strconv.Atoi(strings.TrimSpace(raw))
		if raw != "" && (err != nil || previous < 0) {
			s.logger.Error("schedule: 重试次数无效", "job", j.Key)
			return
		}
		attempt := previous + 1
		if attempt <= scheduleMaxRetries {
			wait := scheduleRetryWaits[attempt-1]
			settings = []scheduleSetting{
				{"schedule_retry_count_" + j.Key, fmt.Sprint(attempt), "巡检已重试次数"},
				{"schedule_retry_" + j.Key, time.Now().Add(wait).UTC().Format(time.RFC3339), "失败巡检退避重试"},
				{"schedule_error_" + j.Key, fmt.Sprintf("%d 项检查失败，%d 分钟后第 %d/%d 次重试", failedCount, int(wait.Minutes()), attempt, scheduleMaxRetries), "巡检错误"},
			}
		} else {
			// 放弃：按「已运行」记账，下一次在正常周期（每天 / 每周）再跑。
			exhausted = true
			settings = []scheduleSetting{
				{"schedule_last_" + j.Key, time.Now().UTC().Format(time.RFC3339), "巡检最近运行时间（重试用尽）"},
				{"schedule_retry_" + j.Key, "", "巡检重试已清除"},
				{"schedule_retry_count_" + j.Key, "0", "巡检已重试次数"},
				{"schedule_error_" + j.Key, fmt.Sprintf("连续 %d 次重试仍有 %d 项失败，已停止重试，等下个周期", scheduleMaxRetries, failedCount), "巡检错误"},
			}
		}
	} else {
		settings = []scheduleSetting{
			{"schedule_last_" + j.Key, time.Now().UTC().Format(time.RFC3339), "巡检最近成功时间"},
			{"schedule_retry_" + j.Key, "", "巡检重试已清除"},
			{"schedule_retry_count_" + j.Key, "0", "巡检已重试次数"},
			{"schedule_error_" + j.Key, "", "巡检错误已清除"},
		}
	}
	if err := s.putScheduleSettings(ctx, settings); err != nil {
		s.logger.Error("schedule: 保存巡检状态失败", "job", j.Key, "error", err)
		return
	}
	if exhausted {
		s.telegramNotifyAsync(fmt.Sprintf("%s：连续重试 %d 次仍有 %d 项检查失败，已停止重试，等下个周期再跑。", j.Label, scheduleMaxRetries, failedCount))
	}
	s.logger.Info("schedule: job finished", "job", j.Key, "changes", msg != "")
	if msg != "" {
		s.telegramNotifyAsync("⏰ " + j.Label + "\n" + msg)
	}
}

// 重试间隔：5 → 15 → 45 分钟（scheduleMaxRetries 与它的长度一致）。
var scheduleRetryWaits = []time.Duration{5 * time.Minute, 15 * time.Minute, 45 * time.Minute}

const scheduleMaxRetries = 3

// liveNodeIDs 是定时巡检要检查的节点：已被「故障节点自动下线」判为离线或被墙的跳过 ——
// 它们本来就检测不通，算作失败只会触发无意义的重试，还白白消耗第三方配额。
func (s *Server) liveNodeIDs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	rows, err := s.db.Query(ctx, `SELECT id::text, name FROM hysteria_nodes WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		scheduleFailed(ctx)
		return nil, err
	}
	defer rows.Close()
	unhealthy := s.unhealthyNodes(ctx)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			scheduleFailed(ctx)
			return nil, err
		}
		if _, bad := unhealthy[id]; bad {
			continue
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		scheduleFailed(ctx)
		return nil, err
	}
	return out, nil
}

func (s *Server) unlockSnapshot(ctx context.Context, nodeID string) (map[string]string, bool, error) {
	var raw []byte
	out := map[string]string{}
	err := s.db.QueryRow(ctx, `SELECT results FROM node_unlock WHERE node_id=$1::uuid AND checked_at IS NOT NULL`, nodeID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rs []unlockResult
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, false, err
	}
	for _, r := range rs {
		out[r.Name] = r.Status
	}
	return out, true, nil
}

var unlockStatusZH = map[string]string{"yes": "解锁", "partial": "部分", "no": "不解锁", "unknown": "检测失败"}

func (s *Server) scheduledUnlock(ctx context.Context) string {
	var b strings.Builder
	nodes, err := s.liveNodeIDs(ctx)
	if err != nil {
		return ""
	}
	for id, name := range nodes {
		before, hadSnapshot, err := s.unlockSnapshot(ctx, id)
		if err != nil {
			s.logger.Warn("schedule: read previous unlock snapshot failed", "node_id", id, "error", err)
			scheduleFailed(ctx)
			continue
		}
		if _, err := s.checkAndStoreUnlock(ctx, id); err != nil {
			// 2026-11-12：没接节点 API 的节点（只走 SSH 采集、或已解绑）解锁检测对它不适用，不计失败。
			// 原来一律 scheduleFailed：这类节点每天必然「失败」，于是整轮在 5、15、45 分钟后各重跑一次 ——
			// 其余正常节点每天去访问流媒体与 AI 站点 4 轮而不是 1 轮，最后还推送一条「重试用尽」。
			if errors.Is(err, errUnlockNoAPI) {
				continue
			}
			scheduleFailed(ctx)
			continue
		}
		after, _, err := s.unlockSnapshot(ctx, id)
		if err != nil {
			s.logger.Warn("schedule: read updated unlock snapshot failed", "node_id", id, "error", err)
			scheduleFailed(ctx)
			continue
		}
		if !hadSnapshot {
			continue
		}
		for svc, now := range after {
			prev, had := before[svc]
			// 「检测失败」是一次性抖动，不当成变化来吵人
			if !had || prev == now || now == "unknown" || prev == "unknown" {
				continue
			}
			fmt.Fprintf(&b, "%s · %s：%s → %s\n", name, svc, unlockStatusZH[prev], unlockStatusZH[now])
		}
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) scheduledPurity(ctx context.Context) string {
	var b strings.Builder
	nodes, err := s.liveNodeIDs(ctx)
	if err != nil {
		return ""
	}
	for id, name := range nodes {
		before, had, err := s.loadIPPurity(ctx, id)
		if err != nil {
			s.logger.Warn("schedule: read previous IP purity snapshot failed", "node_id", id, "error", err)
			scheduleFailed(ctx)
			continue
		}
		after, err := s.checkAndStorePurity(ctx, id, true)
		if err != nil {
			scheduleFailed(ctx)
			continue
		}
		if !had || after.IP == "" {
			continue
		}
		if before.FraudScore != nil && after.FraudScore != nil {
			if d := *after.FraudScore - *before.FraudScore; d >= 20 || d <= -20 {
				fmt.Fprintf(&b, "%s：评分 %d → %d\n", name, *before.FraudScore, *after.FraudScore)
			}
		}
		if before.Native != nil && after.Native != nil && *before.Native != *after.Native {
			fmt.Fprintf(&b, "%s：%s → %s\n", name, map[bool]string{true: "原生", false: "广播"}[*before.Native], map[bool]string{true: "原生", false: "广播"}[*after.Native])
		}
		for _, f := range []struct {
			label  string
			before *bool
			after  *bool
		}{{"代理", before.Proxy, after.Proxy}, {"VPN", before.VPN, after.VPN}, {"滥用", before.Abuser, after.Abuser}} {
			if f.before != nil && f.after != nil && !*f.before && *f.after {
				fmt.Fprintf(&b, "%s：新出现「%s」标记\n", name, f.label)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// scheduledAudit 给所有有 SSH 凭据的主机排队只读安全检查；结果由 collectAuditDigest 汇总。
func (s *Server) scheduledAudit(ctx context.Context) string {
	// 2026-11-06：已有排队 / 执行中安全检查的主机不再派发。部分主机派发失败触发重试时，原来给全部主机
	// 再排一遍（重复 SSH 检查），并用新 id 覆盖待汇总列表 —— 上一批照样执行，却永远不会被汇总推送。
	rows, err := s.db.Query(ctx, `
SELECT s.id::text FROM servers s
WHERE s.deleted_at IS NULL AND EXISTS (SELECT 1 FROM server_credentials c WHERE c.server_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM operation_tasks t WHERE t.server_id = s.id AND t.status IN ('queued','running')
                  AND t.operation_type = 'system_preset' AND t.parameters->>'preset' = 'security_audit')`)
	if err != nil {
		scheduleFailed(ctx)
		return ""
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			scheduleFailed(ctx)
			return ""
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		scheduleFailed(ctx)
		return ""
	}
	rows.Close()
	var taskIDs []string
	for _, sid := range ids {
		var tid string
		if err := s.db.QueryRow(ctx, `INSERT INTO operation_tasks (server_id, operation_type, parameters) VALUES ($1::uuid, 'system_preset', '{"preset":"security_audit"}'::jsonb) RETURNING id::text`, sid).Scan(&tid); err == nil {
			taskIDs = append(taskIDs, tid)
		} else {
			scheduleFailed(ctx)
		}
	}
	// 追加到待汇总列表（去重），不覆盖：上一批还没汇总的任务也要进这次的汇总。
	var pending []string
	var prevRaw string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_audit_tasks'`).Scan(&prevRaw); err == nil && prevRaw != "" {
		_ = json.Unmarshal([]byte(prevRaw), &pending)
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		scheduleFailed(ctx)
		return ""
	}
	seen := make(map[string]bool, len(pending)+len(taskIDs))
	merged := make([]string, 0, len(pending)+len(taskIDs))
	for _, id := range append(pending, taskIDs...) {
		if id != "" && !seen[id] {
			seen[id] = true
			merged = append(merged, id)
		}
	}
	raw, _ := json.Marshal(merged)
	if err := s.putSetting(ctx, "schedule_audit_tasks", string(raw), "定时安全检查待汇总的任务"); err != nil {
		scheduleFailed(ctx)
	}
	return "" // 结果等任务跑完再汇总
}

var auditScorePattern = regexp.MustCompile(`安全评分\s*(\d+)/100`)

func (s *Server) collectAuditDigest(ctx context.Context) {
	var raw string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_audit_tasks'`).Scan(&raw); err != nil || raw == "" || raw == "[]" {
		return
	}
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil || len(ids) == 0 {
		return
	}
	rows, err := s.db.Query(ctx, `
SELECT t.status, COALESCE(t.result_summary,''), s.name FROM operation_tasks t JOIN servers s ON s.id = t.server_id
WHERE t.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return
	}
	type res struct{ status, summary, name string }
	var all []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.status, &r.summary, &r.name); err != nil {
			rows.Close()
			return
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return
	}
	rows.Close()
	// 仅在查询失败时才中断；若部分旧任务记录已被历史清理，剩余任务全部结束时仍应照常汇总并清空待办列表，防止周报永久卡死。
	if len(all) == 0 {
		_ = s.putSetting(ctx, "schedule_audit_tasks", "[]", "定时安全检查待汇总的任务")
		return
	}
	for _, r := range all {
		if r.status == "queued" || r.status == "running" {
			return // 还没跑完，下个钟头再看
		}
	}
	var b strings.Builder
	for _, r := range all {
		if r.status != "success" {
			fmt.Fprintf(&b, "%s：检查失败（%s）\n", r.name, truncateRunes(r.summary, 60))
			continue
		}
		if m := auditScorePattern.FindStringSubmatch(r.summary); m != nil {
			if score, _ := strconv.Atoi(m[1]); score < 80 {
				fmt.Fprintf(&b, "%s：安全评分 %d/100，详见运维任务日志\n", r.name, score)
			}
		}
	}
	// 2026-11-03：先送达再清空待汇总列表。原来先清空、再异步发送 ——
	// 发送失败（超长被拒、网络抖动）后这周的结果再也发不出来。
	// 未绑定机器人时 telegramNotify 返回 false，仍要清空，否则每小时都重算一遍。
	if msg := strings.TrimSpace(b.String()); msg != "" {
		// 仅当机器人聊天编号与令牌均已配置时保留待办以供后续重试，避免未配令牌时每分钟空转查询。
		if !s.telegramNotify("⏰ 每周安全检查\n"+msg) && s.telegramChatID(ctx) != "" && s.telegramToken() != "" {
			return // 下次调度重试
		}
	}
	if err := s.putSetting(ctx, "schedule_audit_tasks", "[]", "定时安全检查待汇总的任务"); err != nil {
		return
	}
}

// ---------------------------------------------------------------- 接口

// GET /api/schedules
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		claims, _ := claimsFrom(r.Context())
		if !s.enforceReadLimit(w, claims.Subject) {
			return
		}
	}
	cfg, err := s.scheduleConfig(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	type item struct {
		Key         string     `json:"key"`
		Label       string     `json:"label"`
		Interval    string     `json:"interval"`
		Enabled     bool       `json:"enabled"`
		LastRun     *time.Time `json:"last_run"`
		NextRun     *time.Time `json:"next_run"`
		Running     bool       `json:"running"`
		LastAttempt *time.Time `json:"last_attempt"`
		LastError   string     `json:"last_error"`
	}
	out := []item{}
	for _, j := range scheduleJobs {
		it := item{Key: j.Key, Label: j.Label, Enabled: cfg[j.Key]}
		var err error
		it.LastRun, err = s.settingTime(r.Context(), "schedule_last_"+j.Key)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		it.LastAttempt, err = s.settingTime(r.Context(), "schedule_attempt_"+j.Key)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		err = s.db.QueryRow(r.Context(), `SELECT value FROM system_settings WHERE key=$1`, "schedule_error_"+j.Key).Scan(&it.LastError)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.databaseError(w, r, err)
			return
		}
		it.Interval = map[bool]string{true: "每天", false: "每周"}[j.Interval == 24*time.Hour]
		if it.Enabled {
			next := time.Now()
			if it.LastRun != nil {
				next = it.LastRun.Add(j.Interval)
			}
			it.NextRun = &next
			retry, err := s.settingTime(r.Context(), "schedule_retry_"+j.Key)
			if err != nil {
				s.databaseError(w, r, err)
				return
			}
			if retry != nil {
				it.NextRun = retry
			}
		}
		_, it.Running = scheduleRunning.Load(j.Key)
		out = append(out, it)
	}
	// 2026-11-06：读绑定失败不能显示成「未配置 Telegram」—— 会让人以为绑定丢了、去重新配置。
	// telegram_configured 回 null 表示没读到，前端显示「未确认」。
	payload := map[string]any{"items": out}
	chatID, chatErr := s.telegramChatIDChecked(r.Context())
	if chatErr != nil {
		s.logger.Warn("schedules: 读取 Telegram 绑定失败", "error", chatErr)
		payload["telegram_configured"] = nil
		payload["telegram_error"] = "Telegram 绑定状态读取失败"
	} else {
		payload["telegram_configured"] = chatID != ""
	}
	writeJSON(w, http.StatusOK, payload)
}

// PUT /api/schedules  {"key": "unlock_daily", "enabled": false}
func (s *Server) setSchedule(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	cfg, err := s.scheduleConfig(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if _, ok := cfg[in.Key]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知的定时任务"})
		return
	}
	// 2026-11-06：只改这一项，在库里原子合并。原来读出整份配置、改一项、整份写回：两个管理员同时切换
	// 不同任务的开关，后写的那次会把先写的那次覆盖掉。库里还没有配置时，以当前默认值为底。
	cfg[in.Key] = in.Enabled
	base, _ := json.Marshal(cfg)
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '定时巡检开关')
ON CONFLICT (key) DO UPDATE SET
  value = (CASE WHEN left(ltrim(system_settings.value), 1) = chr(123) THEN system_settings.value::jsonb ELSE $2::jsonb END
           || jsonb_build_object($3::text, $4::boolean))::text,
  updated_at = now()`, scheduleConfigKey, string(base), in.Key, in.Enabled); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "set_schedule", "system_setting", scheduleConfigKey, map[string]any{"key": in.Key, "enabled": in.Enabled})
	s.listSchedules(w, r)
}

// POST /api/schedules/{key}/run —— 立即在后台跑一次。
func (s *Server) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：走写限流。「立即运行」会对所有节点跑解锁检测 / 第三方纯净度查询（消耗付费配额），
	// 原来是唯一一个不限流的写接口。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	key := r.PathValue("key")
	for _, j := range scheduleJobs {
		if j.Key != key {
			continue
		}
		if _, busy := scheduleRunning.Load(key); busy {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "这个任务正在运行"})
			return
		}
		s.audit(r.Context(), r, "run_schedule", "system_setting", key, nil)
		go s.runScheduleJob(context.WithoutCancel(r.Context()), j)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "未知的定时任务"})
}

type scheduleRunKey struct{}
type scheduleRunState struct {
	mu     sync.Mutex
	Failed int
}

func (st *scheduleRunState) inc() {
	st.mu.Lock()
	st.Failed++
	st.mu.Unlock()
}

func (st *scheduleRunState) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.Failed
}

func scheduleFailed(ctx context.Context) {
	if state, ok := ctx.Value(scheduleRunKey{}).(*scheduleRunState); ok {
		state.inc()
	}
}
