package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nodeops/server/internal/secrets"
	"nodeops/server/internal/sshclient"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

type Executor struct {
	logger   *slog.Logger
	db       *pgxpool.Pool
	interval time.Duration
	box      *secrets.Box
	// inflight 是正在跑的任务数。领取不再等上一批全部结束（见 runClaimed），
	// 这里给并发设上限，避免慢任务堆积时每个 tick 都再开一批。
	inflight atomic.Int32
	wg       sync.WaitGroup
	// 与 collector 对齐的循环生命周期同步：保证 Wait 先等 Run 循环彻底退出，
	// 彻底消除停机时 claimAndRun 刚提交事务尚未 wg.Add(1) 就被 Wait 抢跑放行关闭连接池的竞态。
	runMu      sync.Mutex
	activeRuns int
	runDone    chan struct{}
	// runBody 仅用于包内单测注入可控循环体，生产路径保持 nil。
	runBody func(context.Context)
}

// maxInflightTasks 与单批上限配合，确保全进程在途执行任务总数严格不超过上限。
const maxInflightTasks = 8

func New(logger *slog.Logger, db *pgxpool.Pool, intervalSeconds int, box *secrets.Box) *Executor {
	if intervalSeconds <= 0 {
		intervalSeconds = 5
	}
	return &Executor{logger: logger, db: db, interval: time.Duration(intervalSeconds) * time.Second, box: box}
}

func (e *Executor) beginRun() {
	e.runMu.Lock()
	if e.activeRuns == 0 {
		e.runDone = make(chan struct{})
	}
	e.activeRuns++
	e.runMu.Unlock()
}

func (e *Executor) endRun() {
	e.runMu.Lock()
	e.activeRuns--
	if e.activeRuns == 0 && e.runDone != nil {
		close(e.runDone)
		e.runDone = nil
	}
	e.runMu.Unlock()
}

// Start 在当前协程同步登记生命周期后派生后台执行循环，避免启停瞬间与 Wait 竞态。
func (e *Executor) Start(ctx context.Context) {
	e.beginRun()
	go func() {
		defer e.endRun()
		e.Run(ctx)
	}()
}

// Wait 先等待执行器调度循环彻底停止（不再领取新任务），再等待全部在途任务完成收尾与状态写回。
func (e *Executor) Wait() {
	for {
		e.runMu.Lock()
		done := e.runDone
		e.runMu.Unlock()
		if done == nil {
			break
		}
		<-done
	}
	e.wg.Wait()
}

type task struct {
	id          string
	serverID    string
	host        string
	port        int
	user        string
	fingerprint string
	allowWrite  bool
	writeUnits  []string
	nodePort    *int
	operation   string
	parameters  []byte
	cred        sshclient.Credential
	hasCred     bool
	// credBroken：库里有密文但解不开（面板加密密钥轮换 / 未配置）。与「没配凭据」处理方式不同（2026-11-06）。
	credBroken bool
}

type logLine struct {
	level   string
	message string
}

var safeUnit = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$`)

func (e *Executor) Run(ctx context.Context) {
	e.beginRun()
	defer e.endRun()
	if e.runBody != nil {
		e.runBody(ctx)
		return
	}
	e.reapStaleRunning(ctx)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reapStaleRunning(ctx)
			e.claimAndRun(ctx)
		}
	}
}

// Never replay an interrupted operation: it may already have had side effects.
func (e *Executor) reapStaleRunning(ctx context.Context) {
	reapCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := e.db.Exec(reapCtx, `UPDATE operation_tasks SET status = 'failed',
        error_code = 'EXECUTOR_INTERRUPTED', result_summary = 'Executor interrupted; inspect before manual retry',
        finished_at = now() WHERE status = 'running' AND started_at < now() - interval '10 minutes'`)
	if err != nil {
		e.logger.Warn("executor: stale task reconciliation failed", "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		e.logger.Warn("executor: interrupted tasks marked failed", "count", tag.RowsAffected())
	}
}

func (e *Executor) claimAndRun(ctx context.Context) {
	if e.inflight.Load() >= maxInflightTasks {
		return
	}
	avail := int(maxInflightTasks - e.inflight.Load())
	if avail <= 0 {
		return
	}
	if avail > 4 {
		avail = 4
	}
	claimCtx, cancelClaim := context.WithTimeout(ctx, 10*time.Second)
	defer cancelClaim()
	tx, err := e.db.Begin(claimCtx)
	if err != nil {
		e.logger.Warn("executor: begin claim failed", "error", err)
		return
	}
	defer func() {
		rollbackCtx, stopRollback := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopRollback()
		_ = tx.Rollback(rollbackCtx)
	}()
	// 先锁主机，再用下一条语句的新快照复核 running/lease。仅锁任务行不能防住
	// 两个执行器从不同快照中分别领取同一主机的不同任务。
	rows, err := tx.Query(claimCtx, claimServersQuery, avail)
	if err != nil {
		e.logger.Warn("executor: lock servers failed", "error", err)
		return
	}
	var servers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			e.logger.Warn("executor: scan server failed", "error", err)
			return
		}
		servers = append(servers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		e.logger.Warn("executor: read servers failed", "error", err)
		return
	}
	if len(servers) == 0 {
		return
	}
	rows, err = tx.Query(claimCtx, claimQuery, servers)
	if err != nil {
		e.logger.Warn("executor: claim failed", "error", err)
		return
	}
	tasks, err := e.readClaimed(rows)
	if err != nil {
		e.logger.Warn("executor: read claimed tasks failed", "error", err)
		return
	}
	if err := tx.Commit(claimCtx); err != nil {
		e.logger.Warn("executor: commit claim failed", "error", err)
		return
	}
	e.runClaimed(ctx, tasks)
}

const claimServersQuery = `
SELECT s.id::text FROM servers s
JOIN (
  SELECT q.server_id, min(q.created_at) AS oldest FROM operation_tasks q
  WHERE q.status = 'queued'
    AND NOT EXISTS (SELECT 1 FROM operation_tasks r WHERE r.server_id = q.server_id AND r.status = 'running')
    AND NOT EXISTS (SELECT 1 FROM host_operation_leases l WHERE l.server_id = q.server_id)
  GROUP BY q.server_id
) pending ON pending.server_id = s.id
-- 2026-11-03：已删除的主机不再领取（原来删主机后排队任务仍会 SSH 过去执行）。
WHERE s.deleted_at IS NULL
ORDER BY pending.oldest, s.id
LIMIT $1 FOR UPDATE OF s SKIP LOCKED`

// claimQuery 一批最多领 4 个任务，但**每台主机至多 1 个**（同一台机器上并发跑
// 重启、预设等写操作会互相踩踏），并跳过：
//   - 已有 running 任务的主机（跨批也不叠加）；
//   - 正被一键安装/迁移占用的主机（host_operation_leases 有租约）。
//
// 先用 DISTINCT ON 在全部排队任务里给每台主机挑最早的一条（不受单台主机积压量影响，
// 不会饿死别的主机），再对挑中的行 FOR UPDATE SKIP LOCKED 并复核仍是 queued；
// 被别的执行器实例抢先锁住的，本轮直接跳过。
const claimQuery = `
WITH picked AS (
  SELECT id FROM (
    SELECT DISTINCT ON (q.server_id) q.id, q.created_at FROM operation_tasks q
    WHERE q.status = 'queued' AND q.server_id = ANY($1::uuid[])
      AND NOT EXISTS (SELECT 1 FROM operation_tasks r WHERE r.server_id = q.server_id AND r.status = 'running')
      AND NOT EXISTS (SELECT 1 FROM host_operation_leases l WHERE l.server_id = q.server_id)
    ORDER BY q.server_id, q.created_at
  ) per_server ORDER BY created_at LIMIT 4
), next_task AS (
  SELECT id, server_id FROM operation_tasks
  WHERE id IN (SELECT id FROM picked) AND status = 'queued'
  FOR UPDATE SKIP LOCKED
)
UPDATE operation_tasks t SET status = 'running', started_at = now()
FROM next_task n
JOIN servers s ON s.id = n.server_id AND s.deleted_at IS NULL
LEFT JOIN LATERAL (
    SELECT auth_type, ciphertext FROM server_credentials x WHERE x.server_id = s.id ORDER BY x.created_at DESC LIMIT 1
) cred ON true
WHERE t.id = n.id
RETURNING t.id, t.operation_type, t.parameters, t.server_id, s.host, s.ssh_port, s.ssh_user,
          COALESCE(s.host_fingerprint, ''), s.allow_write_ops, s.write_units,
          COALESCE(cred.auth_type, ''), cred.ciphertext,
          (SELECT nd.listen_port FROM hysteria_nodes nd WHERE nd.id = t.node_id) AS node_port`

func (e *Executor) readClaimed(rows pgx.Rows) ([]task, error) {
	defer rows.Close()
	var tasks []task
	for rows.Next() {
		var t task
		var authType string
		var secret []byte
		if err := rows.Scan(&t.id, &t.operation, &t.parameters, &t.serverID, &t.host, &t.port, &t.user, &t.fingerprint, &t.allowWrite, &t.writeUnits, &authType, &secret, &t.nodePort); err != nil {
			return nil, err
		}
		if len(secret) > 0 {
			if e.box == nil {
				t.credBroken = true
			} else if plain, err := e.box.Open(secret); err == nil {
				if authType == "" {
					authType = "ssh_key"
				}
				t.cred = sshclient.Credential{Type: authType, Secret: plain}
				t.hasCred = true
			} else {
				t.credBroken = true
			}
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (e *Executor) runClaimed(ctx context.Context, tasks []task) {
	if len(tasks) == 0 {
		return
	}
	// Run the claimed batch concurrently instead of serially, and do NOT wait for it
	// before the next tick.
	//
	// 2026-11-03：原来 tick 要等整批结束才返回。一条 system_preset（apt upgrade，8 分钟）
	// 就让所有主机的排队任务都等它跑完 —— 另一台机器上一个 10 秒的重启也要排 8 分钟。
	// 同一台主机不会被重复领取：claimServersQuery 跳过已有 running 任务的主机，
	// 领取在事务里先锁主机行；所以每个 tick 继续领取其他主机的任务是安全的。
	// 每个任务的结果写回用独立上下文（resultWritebackContext），不依赖这一轮 tick。
	for _, t := range tasks {
		e.inflight.Add(1)
		e.wg.Add(1)
		go func(t task) {
			defer e.wg.Done()
			defer e.inflight.Add(-1)
			defer func() {
				if r := recover(); r != nil {
					e.logger.Error("executor: task panic recovered", "task_id", t.id, "panic", r, "stack", string(debug.Stack()))
					// 任务协程异常退出时必须立即回写终态释放主机锁，避免该主机被白白卡死 10 分钟。
					msg := fmt.Sprintf("执行器发生内部异常: %v", r)
					e.writeTaskResult(t, "failed", msg, "exec_panic", []logLine{{"error", msg}})
				}
			}()
			e.runTask(ctx, t)
		}(t)
	}
}

const resultWritebackTimeout = 45 * time.Second

// resultWritebackContext is deliberately independent of the operation deadline:
// after dispatch times out, the task context is canceled but we still must persist
// the terminal status and diagnostic logs.
func resultWritebackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), resultWritebackTimeout)
}

func (e *Executor) writeTaskResult(t task, status, summary, errorCode string, logs []logLine) {
	writebackCtx, cancelWriteback := resultWritebackContext()
	defer cancelWriteback()

	// 先批量写入日志再落终态，消除前端看到终态却读到空日志的竞态，同时减少往返次数。
	// 日志写入带短退避重试（ON CONFLICT DO NOTHING 保证完全幂等），避免数据库瞬断导致终态成功而日志永久空白。
	if len(logs) > 0 {
		seqs := make([]int, len(logs))
		levels := make([]string, len(logs))
		msgs := make([]string, len(logs))
		for i, entry := range logs {
			seqs[i] = i + 1
			levels[i] = entry.level
			msgs[i] = entry.message
		}
		for attempt := 0; attempt < 3; attempt++ {
			_, dbErr := e.db.Exec(writebackCtx, `
INSERT INTO task_logs (task_id, sequence, level, message)
SELECT $1, u.seq, u.lvl, u.msg
FROM unnest($2::int[], $3::text[], $4::text[]) AS u(seq, lvl, msg)
ON CONFLICT DO NOTHING`, t.id, seqs, levels, msgs)
			if dbErr == nil {
				break
			}
			e.logger.Warn("executor: write logs batch failed", "task_id", t.id, "attempt", attempt+1, "count", len(logs), "error", dbErr)
			if attempt == 2 {
				break
			}
			select {
			case <-writebackCtx.Done():
				attempt = 3
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
	}

	// 2026-11-04：结果写回带重试（在 resultWritebackTimeout 预算内），并只改仍是 running 的行。
	// 原来只试一次：数据库短暂不可用时，任务（包括已经成功的更新 / 重启内核）一直停在 running，
	// 那台主机 10 分钟内不再领任务、也不能安装 / 换 IP；随后 reapStaleRunning 把它标成
	// 「执行器中断，人工核查后重试」—— 运维看到的结论与实际相反。
	// 只改 running：重试期间若已被回收器判为中断，不再覆盖它的结论。
	for attempt := 0; ; attempt++ {
		tag, dbErr := e.db.Exec(writebackCtx, `UPDATE operation_tasks SET status=$1, result_summary=$2, error_code=NULLIF($3,''), finished_at=now() WHERE id=$4 AND status='running'`, status, summary, errorCode, t.id)
		if dbErr == nil {
			if tag.RowsAffected() == 0 {
				e.logger.Warn("executor: 任务终态未更新（可能已被回收或覆盖）", "task_id", t.id, "status", status)
			}
			break
		}
		e.logger.Warn("executor: write result failed", "task_id", t.id, "attempt", attempt+1, "error", dbErr)
		select {
		case <-writebackCtx.Done():
		case <-time.After(time.Duration(attempt+1) * time.Second):
			continue
		}
		e.logger.Error("executor: task result could not be saved; it will be reaped as interrupted", "task_id", t.id, "status", status)
		break
	}
	e.logger.Info("executor: task finished", "task_id", t.id, "operation", t.operation, "status", status)
}

func (e *Executor) runTask(ctx context.Context, t task) {
	// 预设任务（apt upgrade 等）比服务控制慢得多，单独给 8 分钟 ——
	// 仍短于 reapStaleRunning 的 10 分钟，避免跑着的任务被误判为中断。
	timeout := 90 * time.Second
	if t.operation == "system_preset" {
		timeout = 8 * time.Minute
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	summary, logs, err := e.dispatch(taskCtx, t)
	status := "success"
	errorCode := ""
	if err != nil {
		status = "failed"
		if ctx.Err() != nil {
			errorCode = "EXECUTOR_INTERRUPTED"
			summary = "执行器退出导致任务中断，请核查远端主机状态后再决定是否重试"
		} else {
			errorCode = "exec_failed"
			if summary == "" {
				summary = err.Error()
			}
		}
		logs = append(logs, logLine{"error", err.Error()})
	}

	e.writeTaskResult(t, status, summary, errorCode, logs)
}

func (e *Executor) dispatch(ctx context.Context, t task) (string, []logLine, error) {
	switch t.operation {
	case "health_check":
		return e.healthCheck(ctx, t)
	case "logs":
		return e.fetchLogs(ctx, t)
	// The systemd service control family. Each of these mutates the host and so
	// goes through the same gates (allow_write_ops + unit whitelist) inside
	// controlService.
	case "restart":
		return e.controlService(ctx, t, serviceRestart)
	case "start":
		return e.controlService(ctx, t, serviceStart)
	case "stop":
		return e.controlService(ctx, t, serviceStop)
	case "reload":
		return e.controlService(ctx, t, serviceReload)
	case "enable":
		return e.controlService(ctx, t, serviceEnable)
	case "disable":
		return e.controlService(ctx, t, serviceDisable)
	case "system_preset":
		return e.runPreset(ctx, t)
	default:
		// Reachable only if a task row predates a supported-type change or was
		// inserted directly into the database. The API rejects unknown types
		// before they are queued, so this is a last line of defence.
		return "", nil, fmt.Errorf("操作 %s 尚未接入执行器", t.operation)
	}
}

func (e *Executor) dial(ctx context.Context, t task) (*ssh.Client, error) {
	if t.credBroken {
		// 原来与「没配凭据」同一句话：运维照着去重新上传凭据，而问题在面板侧的加密密钥。
		return nil, fmt.Errorf("SSH 凭据解密失败（面板加密密钥与保存凭据时不一致），请检查面板的 CREDENTIAL_KEY，无需重新上传凭据")
	}
	if !t.hasCred {
		return nil, fmt.Errorf("该主机未配置 SSH 凭据（私钥或密码），无法执行")
	}
	client, fingerprint, err := sshclient.Dial(ctx, t.host, t.port, t.user, t.cred, t.fingerprint)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败: %w", err)
	}
	if t.fingerprint == "" && fingerprint != "" {
		tag, err := e.db.Exec(ctx, `UPDATE servers SET host_fingerprint=$1 WHERE id=$2 AND host_fingerprint IS NULL AND deleted_at IS NULL`, fingerprint, t.serverID)
		if err != nil {
			e.logger.Warn("executor: pin host key failed", "error", err)
		} else if tag.RowsAffected() == 1 {
			// 首次信任（TOFU）留痕，见 collector 同名分支（审计 L1）。
			e.logger.Warn("executor: 首次信任主机密钥", "server_id", t.serverID, "fingerprint", fingerprint)
			if _, err := e.db.Exec(ctx, `INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details) VALUES (NULL, 'host_key_first_trust', 'server', $1::uuid, jsonb_build_object('fingerprint', $2::text, 'source', 'executor'))`, t.serverID, fingerprint); err != nil {
				e.logger.Warn("executor: 首次信任审计写入失败", "error", err)
			}
		}
	}
	return client, nil
}

func unitFrom(parameters []byte) (string, error) {
	var payload struct {
		Unit string `json:"unit"`
	}
	if len(parameters) > 0 {
		if err := json.Unmarshal(parameters, &payload); err != nil {
			return "", fmt.Errorf("parameters 不是合法 JSON")
		}
	}
	unit := strings.TrimSpace(payload.Unit)
	if unit == "" {
		unit = "hysteria-server"
	}
	if !safeUnit.MatchString(unit) {
		return "", fmt.Errorf("非法的服务名: %s", unit)
	}
	return unit, nil
}

func (e *Executor) healthCheck(ctx context.Context, t task) (string, []logLine, error) {
	// 先校验参数与服务白名单，拒绝非法请求前不再白费一次远程 SSH 握手。
	unit, err := unitFrom(t.parameters)
	if err != nil {
		return "", nil, err
	}
	if unit != "hysteria-server" && !containsUnit(t.writeUnits, unit) {
		return "", nil, fmt.Errorf("健康检查只能针对 hysteria-server 或该主机操作白名单内的服务（%s 不在其中）", unit)
	}
	dialStarted := time.Now()
	client, err := e.dial(ctx, t)
	latencyMs := time.Since(dialStarted).Milliseconds()
	if err != nil {
		return "", []logLine{{"error", fmt.Sprintf("SSH %s:%d 连接失败: %v", t.host, t.port, err)}}, err
	}
	defer client.Close()
	logs := []logLine{{"info", fmt.Sprintf("SSH 握手 %s:%d 成功，耗时 %d ms", t.host, t.port, latencyMs)}}
	command := fmt.Sprintf(`printf "LOAD=%%s\n" "$(cut -d' ' -f1-3 /proc/loadavg)"; s=$(systemctl is-active %[1]s 2>/dev/null); printf "SERVICE=%%s\n" "${s:-unknown}"`, unit)
	stdout, stderr, exit, err := sshclient.Run(ctx, client, command)
	if err != nil {
		return "", logs, fmt.Errorf("执行健康检查失败: %w", err)
	}
	out := sshclient.CleanCommandOutput(stdout)
	logs = append(logs, logLine{"info", fmt.Sprintf("exit=%d", exit)})
	if exit != 0 {
		if stderr != "" {
			logs = append(logs, logLine{"warn", sshclient.CleanCommandOutput(stderr)})
		}
		if out != "" {
			logs = append(logs, logLine{"info", out})
		}
		return "", logs, fmt.Errorf("执行健康检查命令异常退出（exit=%d）", exit)
	}
	if t.nodePort != nil {
		// 2026-11-04：与采集器同一套探测命令与判据。原来 `ss … | head | wc -l` 的退出码来自 wc：
		// 主机上没有 ss（或 ss 不认 -H）时照样输出 0、退出 0，日志写「节点端口 N 未监听」，
		// 运维按这条结论去重启一台其实正常的节点。
		portOut, _, portCode, portErr := sshclient.Run(ctx, client, sshclient.PortProbeCommand(*t.nodePort))
		if portErr != nil {
			logs = append(logs, logLine{"warn", fmt.Sprintf("节点端口 %d 探测失败：%v", *t.nodePort, portErr)})
		} else if listening, parseErr := sshclient.ParsePortProbe(portOut, portCode); parseErr != nil {
			logs = append(logs, logLine{"warn", fmt.Sprintf("节点端口 %d 探测失败：%v", *t.nodePort, parseErr)})
		} else {
			state := "未监听"
			if listening {
				state = "监听中"
			}
			logs = append(logs, logLine{"info", fmt.Sprintf("节点端口 %d %s", *t.nodePort, state)})
		}
	}
	if stderr != "" {
		logs = append(logs, logLine{"warn", sshclient.CleanCommandOutput(stderr)})
	}
	if out != "" {
		logs = append(logs, logLine{"info", out})
	}
	return fmt.Sprintf("SSH 正常；负载 %s；服务 %s 为 %s", loadOf(out), unit, serviceOf(out)), logs, nil
}

func (e *Executor) fetchLogs(ctx context.Context, t task) (string, []logLine, error) {
	unit, err := unitFrom(t.parameters)
	if err != nil {
		return "", nil, err
	}
	// 2026-11-06：只能读 hysteria-server 或该主机白名单里的服务（与服务控制同一张白名单）。
	// 原来任意合法服务名都能读 50 行日志：运维账号可以读 sshd、商城、数据库等任何服务的 journal，
	// 里面常有登录 IP、令牌、连接串。hysteria-server 总是允许：多数只读主机的白名单是空的，
	// 而这正是面板管理的那个服务。
	if unit != "hysteria-server" && !containsUnit(t.writeUnits, unit) {
		return "", nil, fmt.Errorf("只能查看 hysteria-server 或该主机操作白名单内的服务日志（%s 不在其中）", unit)
	}
	client, err := e.dial(ctx, t)
	if err != nil {
		return "", nil, err
	}
	defer client.Close()

	stdout, stderr, exit, err := sshclient.Run(ctx, client, fmt.Sprintf(`journalctl -u %s -n 50 --no-pager 2>&1`, unit))
	if err != nil {
		return "", nil, fmt.Errorf("拉取日志失败: %w", err)
	}
	if exit != 0 {
		msg := strings.TrimSpace(sshclient.CleanCommandOutput(firstNonEmptyLog(stderr, stdout)))
		return "", []logLine{{"error", msg}}, fmt.Errorf("拉取日志退出码 %d：%s", exit, msg)
	}
	logs := make([]logLine, 0, 50)
	for _, line := range strings.Split(sshclient.CleanCommandOutput(stdout), "\n") {
		if strings.TrimSpace(line) != "" {
			logs = append(logs, logLine{"info", line})
		}
	}
	if len(logs) == 0 {
		logs = append(logs, logLine{"warn", "该服务暂无日志输出"})
	}
	return fmt.Sprintf("已拉取 %s 最近 %d 行日志", unit, len(logs)), logs, nil
}

// serviceAction describes one systemd verb the panel is allowed to drive.
//
// `readOnly` marks verbs that do not change service state. They are deliberately
// still routed through the unit whitelist so an operator cannot use `status` to
// probe units outside the declared blast radius, but they do not require
// allow_write_ops to be on.
type serviceAction struct {
	verb     string // systemctl subcommand
	readOnly bool
	// label is the human-readable action name used in result summaries.
	label string
	// afterCmd is appended to the systemctl call. `is-active` gives the operator
	// immediate confirmation that the verb actually took effect; `is-enabled`
	// is the equivalent for the enable/disable pair.
	afterCmd string
}

var (
	serviceRestart = serviceAction{verb: "restart", label: "重启", afterCmd: "is-active"}
	serviceStart   = serviceAction{verb: "start", label: "启动", afterCmd: "is-active"}
	serviceStop    = serviceAction{verb: "stop", label: "停止", afterCmd: "is-active"}
	serviceReload  = serviceAction{verb: "reload", label: "重载配置", afterCmd: "is-active"}
	serviceEnable  = serviceAction{verb: "enable", label: "设为开机自启", afterCmd: "is-enabled"}
	serviceDisable = serviceAction{verb: "disable", label: "取消开机自启", afterCmd: "is-enabled"}
)

// controlService is the single implementation behind every systemd verb the
// panel exposes. Keeping them in one place means the two safety gates cannot
// drift apart between actions — previously each action would have needed its own
// copy of the allow_write_ops and whitelist checks, and a future addition could
// easily forget one.
func (e *Executor) controlService(ctx context.Context, t task, action serviceAction) (string, []logLine, error) {
	if !action.readOnly && !t.allowWrite {
		return "", nil, fmt.Errorf("该主机未开启写操作（allow_write_ops=false），%s 类操作已拒绝", action.label)
	}
	unit, err := unitFrom(t.parameters)
	if err != nil {
		return "", nil, err
	}
	// Even read-only verbs are whitelist-bound: the whitelist defines what this
	// host is willing to talk about at all, not merely what may be changed.
	if !containsUnit(t.writeUnits, unit) {
		return "", nil, fmt.Errorf("服务 %s 不在该主机的操作白名单内（允许：%s）", unit, strings.Join(t.writeUnits, ", "))
	}

	client, err := e.dial(ctx, t)
	if err != nil {
		return "", nil, err
	}
	defer client.Close()

	// Use non-interactive sudo when not already root, so a missing sudoers rule
	// fails fast with a clear exit code instead of hanging on a password prompt.
	prefix := "sudo -n "
	if strings.TrimSpace(t.user) == "root" {
		prefix = ""
	}
	command := serviceCommand(prefix, unit, action)
	stdout, stderr, exit, err := sshclient.Run(ctx, client, command)
	if err != nil {
		return "", nil, fmt.Errorf("%s执行失败: %w", action.label, err)
	}
	if exit != 0 {
		message := strings.TrimSpace(sshclient.CleanCommandOutput(stderr))
		if message == "" {
			message = strings.TrimSpace(sshclient.CleanCommandOutput(stdout))
		}
		return "", []logLine{{"error", message}}, fmt.Errorf("%s命令退出码 %d：%s", action.label, exit, message)
	}

	state := strings.TrimSpace(sshclient.CleanCommandOutput(stdout))
	// systemctl prints to stdout on success; fall back to stderr if stdout was empty.
	if state == "" {
		state = strings.TrimSpace(sshclient.CleanCommandOutput(stderr))
	}
	if state == "" {
		state = "unknown"
	}
	summary := fmt.Sprintf("服务 %s 已%s，当前 %s", unit, action.label, state)
	return summary, []logLine{{"info", fmt.Sprintf("systemctl %s %s", action.verb, unit)}, {"info", state}}, nil
}

func serviceCommand(prefix, unit string, action serviceAction) string {
	command := fmt.Sprintf("%ssystemctl %s %s", prefix, action.verb, unit)
	if action.verb == "stop" || action.verb == "disable" {
		expected, expectedExit := "inactive", 3
		if action.verb == "disable" {
			expected, expectedExit = "disabled", 1
		}
		// 先保留动作失败，再按目标状态判定；探针在 inactive/disabled 时本来就返回非零。
		return command + fmt.Sprintf(" && { state=$(systemctl %s %s); probe_exit=$?; printf '%%s\\n' \"$state\"; [ \"$probe_exit\" -eq %d ] && [ \"$state\" = %q ]; }", action.afterCmd, unit, expectedExit, expected)
	}
	return command + fmt.Sprintf(" && systemctl %s %s", action.afterCmd, unit)
}

func firstNonEmptyLog(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func containsUnit(units []string, want string) bool {
	for _, unit := range units {
		if unit == want {
			return true
		}
	}
	return false
}

func loadOf(out string) string {
	return valueOf(out, "LOAD")
}

func serviceOf(out string) string {
	return valueOf(out, "SERVICE")
}

func valueOf(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if prefix, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && prefix == key {
			if strings.TrimSpace(value) == "" {
				return "unknown"
			}
			return value
		}
	}
	return "—"
}
