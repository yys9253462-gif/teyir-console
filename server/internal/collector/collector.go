package collector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nodeops/server/internal/secrets"
	"nodeops/server/internal/sshclient"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"
)

// maxHostCollectors caps concurrent SSH/API host probes in one cycle.
// Unbounded fan-out used to scale DB and outbound connections with host count.
const maxHostCollectors = 8

type Collector struct {
	logger     *slog.Logger
	db         *pgxpool.Pool
	interval   time.Duration
	box        *secrets.Box
	sshRetryMu sync.Mutex
	sshRetry   map[string]sshRetryEntry
	// 使用互斥锁与完成通道协同管理启停生命周期，避免 sync.WaitGroup 并发 Add 与 Wait 竞态。
	runMu      sync.Mutex
	activeRuns int
	runDone    chan struct{}
	// runBody 仅用于包内单测注入可控循环体，生产路径保持 nil 走默认采集循环。
	runBody func(context.Context)
}

func NewCollector(logger *slog.Logger, db *pgxpool.Pool, intervalSeconds int, box *secrets.Box) *Collector {
	if intervalSeconds <= 0 {
		intervalSeconds = 30
	}
	return &Collector{logger: logger, db: db, interval: time.Duration(intervalSeconds) * time.Second, box: box, sshRetry: make(map[string]sshRetryEntry)}
}

func (c *Collector) beginRun() {
	c.runMu.Lock()
	if c.activeRuns == 0 {
		c.runDone = make(chan struct{})
	}
	c.activeRuns++
	c.runMu.Unlock()
}

func (c *Collector) endRun() {
	c.runMu.Lock()
	c.activeRuns--
	if c.activeRuns == 0 && c.runDone != nil {
		close(c.runDone)
		c.runDone = nil
	}
	c.runMu.Unlock()
}

// Start 在当前协程同步登记生命周期后派生后台采集协程，避免紧接调用 Wait 时出现竞态或抢跑。
func (c *Collector) Start(ctx context.Context) {
	c.beginRun()
	go func() {
		defer c.endRun()
		c.executeRun(ctx)
	}()
}

// Wait 等待全部已登记的采集循环完成收尾退出，供进程优雅停机阶段调用。
func (c *Collector) Wait() {
	for {
		c.runMu.Lock()
		done := c.runDone
		c.runMu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

type sshRetryEntry struct {
	credential [32]byte
	retryAfter time.Time
}

func credentialDigest(t target) [32]byte {
	h := sha256.New()
	writePart := func(s string) {
		_, _ = fmt.Fprintf(h, "%d:%s\x00", len(s), s)
	}
	writePart(t.host)
	writePart(strconv.Itoa(t.port))
	writePart(t.user)
	writePart(t.fingerprint)
	writePart(t.cred.Type)
	_, _ = fmt.Fprintf(h, "%d:", len(t.cred.Secret))
	_, _ = h.Write(t.cred.Secret)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
func (c *Collector) sshCoolingDown(t target) bool {
	c.sshRetryMu.Lock()
	defer c.sshRetryMu.Unlock()
	entry, ok := c.sshRetry[t.id]
	return ok && entry.credential == credentialDigest(t) && time.Now().Before(entry.retryAfter)
}
func (c *Collector) setSSHRetry(t target, failed bool) {
	c.sshRetryMu.Lock()
	defer c.sshRetryMu.Unlock()
	if failed {
		c.sshRetry[t.id] = sshRetryEntry{credential: credentialDigest(t), retryAfter: time.Now().Add(10 * time.Minute)}
	} else {
		delete(c.sshRetry, t.id)
	}
}

// pruneSSHRetry 清理已过期或已从活跃主机列表中移除的冷却记录，防止长期运行后内存持续增长。
func (c *Collector) pruneSSHRetry(active map[string]struct{}, now time.Time) {
	c.sshRetryMu.Lock()
	defer c.sshRetryMu.Unlock()
	for id, entry := range c.sshRetry {
		if _, ok := active[id]; !ok || !now.Before(entry.retryAfter) {
			delete(c.sshRetry, id)
		}
	}
}

func (c *Collector) Run(ctx context.Context) {
	c.beginRun()
	defer c.endRun()
	c.executeRun(ctx)
}

func (c *Collector) executeRun(ctx context.Context) {
	if c.runBody != nil {
		c.runBody(ctx)
		return
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	c.collectOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectOnce(ctx)
		}
	}
}

type target struct {
	id          string
	name        string
	host        string
	port        int
	user        string
	fingerprint string
	cred        sshclient.Credential
	hasCred     bool
	trafficURL  string
	trafficFile string
}

type nodeRow struct {
	id   string
	port int
}

// One SSH connection per host per cycle. Deliberately no bare TCP probes: a
// connect-then-close against port 22 shows up as "Connection closed ...
// [preauth]" and gets the collector's own IP banned by fail2ban.
const metricsScript = `
cpu_snapshot() { awk '/^cpu /{t=0; for(i=2;i<=NF;i++) t+=$i; print t, $5+$6 }' /proc/stat; }
set -- $(cpu_snapshot); t1=$1; i1=$2
sleep 1
set -- $(cpu_snapshot); t2=$1; i2=$2
awk -v a="$t1" -v b="$t2" -v ia="$i1" -v ib="$i2" 'BEGIN{dt=b-a; di=ib-ia; printf "CPU=%.2f\n", (dt>0?(dt-di)*100/dt:0)}'
awk '/^MemTotal:/{print "MEM_TOTAL_KB="$2} /^MemAvailable:/{print "MEM_AVAIL_KB="$2}' /proc/meminfo
df -P -B1 / | awk 'NR==2{print "DISK_USED="$3; print "DISK_TOTAL="$2}'
echo "LOAD=$(cut -d' ' -f1-3 /proc/loadavg)"
echo "KERNEL=$(uname -r)"
`

// cycleTimeout 是一轮采集的总预算：约一个采集周期，但不少于 60 秒
// （单台主机 SSH 预算 25 秒，再加上订阅用量那一段）。
// 原来整轮直接用进程级 ctx：数据库或某个节点 API 卡住时 collectOnce 永不返回，
// ticker 之后的每一轮都被跳过，状态、告警全部停在那一刻。
func (c *Collector) cycleTimeout() time.Duration {
	return max(c.interval, 60*time.Second)
}

func (c *Collector) collectOnce(parent context.Context) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("collector: 采集轮次 panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, c.cycleTimeout())
	defer cancel()
	rows, err := c.db.Query(ctx, `
SELECT s.id, s.name, s.host, s.ssh_port, s.ssh_user, COALESCE(s.host_fingerprint, ''),
       COALESCE(sc.auth_type, ''), sc.ciphertext,
       COALESCE(s.traffic_api_url, ''), COALESCE(s.traffic_secret_file, '')
FROM servers s
LEFT JOIN LATERAL (
    SELECT auth_type, ciphertext FROM server_credentials x WHERE x.server_id = s.id ORDER BY x.created_at DESC LIMIT 1
) sc ON true WHERE s.deleted_at IS NULL ORDER BY s.created_at`)
	if err != nil {
		c.logger.Warn("collector: query servers failed", "error", err)
		return
	}
	var targets []target
	for rows.Next() {
		var t target
		var authType string
		var secret []byte
		if err := rows.Scan(&t.id, &t.name, &t.host, &t.port, &t.user, &t.fingerprint, &authType, &secret, &t.trafficURL, &t.trafficFile); err != nil {
			c.logger.Warn("collector: scan server failed", "error", err)
			continue
		}
		if len(secret) > 0 && c.box != nil {
			plain, err := c.box.Open(secret)
			if err != nil {
				c.logger.Warn("collector: decrypt credential failed", "server", t.name, "error", err)
			} else {
				if authType == "" {
					authType = "ssh_key"
				}
				t.cred = sshclient.Credential{Type: authType, Secret: plain}
				t.hasCred = true
			}
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		c.logger.Warn("collector: read servers failed", "error", err)
		return
	}
	activeIDs := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		activeIDs[t.id] = struct{}{}
	}
	c.pruneSSHRetry(activeIDs, time.Now())

	// Collect hosts concurrently. A serial loop meant one slow or unreachable host
	// (25s timeout) delayed every host behind it, so the cycle overran its own
	// interval as soon as a handful of machines were registered. The wait group
	// keeps collectOnce synchronous from the caller's perspective, so a tick can
	// never overlap with the next one and stack up SSH sessions.
	// Reserve at least half the cycle for subscription API reads and writes.
	hostStageCtx, stopHosts := context.WithTimeout(ctx, c.cycleTimeout()/2)
	defer stopHosts()
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxHostCollectors)
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			// 单台主机的 panic（解析异常输出、驱动 bug 等）只记日志，不能带走整个进程 ——
			// 否则一台机器的怪数据会让所有主机都停止采集。
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("collector: 采集主机时 panic", "server", t.name, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-hostStageCtx.Done():
				return
			}
			c.collectServer(hostStageCtx, t)
		}(t)
	}
	wg.Wait()

	// 订阅用量落库（2026-10-02 起）。
	//
	// 放在主机遍历**之后**、串行执行：它走的是各节点的 portal API，
	// 与上面的 SSH 链路完全无关（按产品定位，节点能力不该依赖 SSH）。
	// 串行是因为内部已按节点缓存 users/list，并发收益很小，
	// 而并发会让"同时对 N 台节点发请求"这件事在节点数增长后变成新的风险源。
	c.collectSubscriptionUsage(ctx)
}

// fallbackAPIProbeContext 在 SSH 拨号耗尽单机预算后，为降级 API 探测分配独立的有界预算，
// 同时仍然响应外层进程退出取消（context.Canceled）。
func fallbackAPIProbeContext(parent, hostCtx context.Context) (context.Context, context.CancelFunc) {
	if d, ok := hostCtx.Deadline(); ok && time.Until(d) >= 12*time.Second && hostCtx.Err() == nil {
		return hostCtx, func() {}
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(hostCtx), 12*time.Second)
	stopWatch := context.AfterFunc(parent, func() {
		if errors.Is(parent.Err(), context.Canceled) {
			cancel()
		}
	})
	return probeCtx, func() {
		stopWatch()
		cancel()
	}
}

func (c *Collector) collectServer(parent context.Context, t target) {
	hostCtx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	ctx := hostCtx
	if !t.hasCred || c.sshCoolingDown(t) {
		// 没有 SSH 凭据不再直接放弃 —— 改走节点门户 API 探测状态。
		//
		// 这是 2026-10-02 修的一个真实缺口：API 导入的节点刻意不带 SSH
		// 凭据，而状态探测此前**只有 SSH 一条路**，于是这类节点的状态
		// 永远停在 unknown、last_checked_at 永远是 null，界面上一直显示
		// 「未知 / 未探测」—— 可它们的 API 明明是通的。
		//
		// 详细论证见 nodeapi_probe.go 的文件头注释。
		c.probeNodesViaAPI(ctx, t)
		return
	}

	dialStarted := time.Now()
	client, fingerprint, err := sshclient.Dial(hostCtx, t.host, t.port, t.user, t.cred, t.fingerprint)
	latencyMs := int(time.Since(dialStarted).Milliseconds())
	if err != nil {
		// 进程正在优雅退出导致的取消不是主机故障，绝不写入离线或错误状态。
		if errors.Is(parent.Err(), context.Canceled) {
			return
		}
		// SSH 连不上。这里要分两种情况，处理方式完全不同：
		//
		//   A. **认证失败**（密钥/密码不对）—— 网络是通的、机器是活的，
		//      只是我们的凭据过期了。这种情况**仍有办法判断节点状态**：
		//      退回 API 探测。否则节点会一直显示 unknown，
		//      而用户完全不知道"其实只需要换一下密钥"。
		//
		//   B. **网络不通**（超时/拒绝连接）—— 机器可能真挂了。
		//      这时再去试 API 只是白等一次超时（API 也在这台机器上），
		//      直接标 offline 更干脆。
		//
		// 生产实测（2026-10-02）：Stockholm-AWS 就是情况 A ——
		// SSH 端口通但认证失败，而它的 API 是好的。
		// 修之前它永远显示 unknown，看不出"换个密钥就好"。
		// SSH 拨号若已耗尽 25 秒单机预算，为后续 nodeHasAPI 与降级 API 探测重新派生有界上下文，
		// 同时仍监听 parent 的进程退出取消信号，避免超时后直接查库或降级探测失败。
		hostCtx, stopFallback := fallbackAPIProbeContext(parent, hostCtx)
		defer stopFallback()
		hasAPI, apiErr := c.nodeHasAPI(hostCtx, t.id)
		if apiErr != nil {
			// 不知道能不能改走 API：这一轮只记原因，不改主机 / 节点状态（下一轮再判）。
			c.logger.Warn("collector: SSH 失败后查询节点 API 绑定失败，本轮不改状态", "server", t.name, "ssh_error", err, "error", apiErr)
			recordCtx, stopRecord := detachedWriteContext(hostCtx, 2*time.Second)
			defer stopRecord()
			if _, dbErr := c.db.Exec(recordCtx, `UPDATE servers SET status_error=$2, updated_at=now() WHERE id=$1`, t.id, sshDialFailureReason(err)); dbErr != nil {
				c.logger.Warn("collector: record ssh failure failed", "server", t.name, "error", dbErr)
			}
			return
		}
		if isSSHAuthFailure(err) && hasAPI {
			// 2026-10-05: 这是已知降级（缺 SSH 凭据但 API 可用），改为 Warn 避免每30秒刷 Info/ERROR 淹没真故障；
			// 状态已由 markServerSSHBroken 写入 servers.status_error，不会丢失告警信息。
			c.logger.Warn("collector: SSH 认证失败，改用 API 探测节点状态",
				"server", t.name, "error", err)
			c.setSSHRetry(t, true)
			markCtx, stopMark := detachedWriteContext(hostCtx, 2*time.Second)
			c.markServerSSHBroken(markCtx, t.id, err)
			stopMark()
			c.probeNodesViaAPI(hostCtx, t)
			return
		}
		// 2026-11-06：其它 SSH 失败（sshd 挂了、22 端口被 fail2ban 封了面板 IP、主机重装后指纹对不上）
		// 只说明 SSH 走不通，不说明节点挂了。节点绑了 API 时同样改走 API 探测，主机与节点状态由它决定，
		// status_error 留着 SSH 失败原因。原来直接把主机写 offline、节点一行都不写：节点卡在上一次的
		// 「在线」，server_offline 告警又让健康闸门把这台主机上能用的节点全部下线。
		if hasAPI {
			c.logger.Warn("collector: SSH 连不上，改用 API 探测节点状态", "server", t.name, "error", err)
			c.setSSHRetry(t, true)
			c.probeNodesViaAPI(hostCtx, t)
			writeCtx, stopWrite := detachedWriteContext(hostCtx, 2*time.Second)
			if _, dbErr := c.db.Exec(writeCtx, `UPDATE servers SET status_error=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, t.id, sshDialFailureReason(err)); dbErr != nil {
				c.logger.Warn("collector: record ssh failure failed", "server", t.name, "error", dbErr)
			}
			stopWrite()
			return
		}
		// 使用脱离超时的短写库上下文，避免 25 秒 SSH 拨号超时后离线状态写不进数据库。
		writeCtx, stopWrite := detachedWriteContext(hostCtx, 2*time.Second)
		if _, dbErr := c.db.Exec(writeCtx, `UPDATE servers SET status='offline', latency_ms=NULL, status_error=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, t.id, sshDialFailureReason(err)); dbErr != nil {
			c.logger.Warn("collector: mark offline failed", "server", t.name, "error", dbErr)
		}
		stopWrite()
		c.logger.Info("collector: host unreachable", "server", t.name, "error", err)
		return
	}
	defer client.Close()
	c.setSSHRetry(t, false)

	if t.fingerprint == "" && fingerprint != "" {
		tag, err := c.db.Exec(hostCtx, `UPDATE servers SET host_fingerprint=$1 WHERE id=$2 AND host_fingerprint IS NULL AND deleted_at IS NULL`, fingerprint, t.id)
		if err != nil {
			c.logger.Warn("collector: pin host key failed", "server", t.name, "error", err)
		} else if tag.RowsAffected() == 1 {
			// 首次信任（TOFU）留痕：此前未固定指纹的主机这次接受了任意主机密钥，
			// 审计里记下指纹，事后可与主机上 ssh-keygen -lf 的结果核对（审计 L1）。
			c.logger.Warn("collector: 首次信任主机密钥", "server", t.name, "fingerprint", fingerprint)
			if _, err := c.db.Exec(hostCtx, `INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details) VALUES (NULL, 'host_key_first_trust', 'server', $1::uuid, jsonb_build_object('fingerprint', $2::text, 'source', 'collector'))`, t.id, fingerprint); err != nil {
				c.logger.Warn("collector: 首次信任审计写入失败", "server", t.name, "error", err)
			}
		}
	}

	stdout, stderr, exit, err := sshclient.Run(hostCtx, client, metricsScript)
	if err != nil || exit != 0 {
		if errors.Is(parent.Err(), context.Canceled) {
			return
		}
		// ⚠️ 2026-10-24 审计 P0-2：这里原来无条件置 warning，下一轮成功又置回
		// online，而告警评估对每次 warning↔online 都新开一条 server_warning、
		// 恢复时再 resolve 一条 —— 一次 SSH 抖动 = 开一条 + 关一条。
		//
		// 实测：美国-阿什本一台机产生 229 条 server_warning（单小时最高 24 条），
		// 全库 reopened 382 次 vs triggered 64 次，真故障被噪声淹没。
		//
		// 现在与节点侧 unstableAfterFailures 保持一致：连续失败达到阈值才置
		// warning，未达阈值**保持原状态不动**（不新增一条状态翻转，也就不新增
		// 告警）。失败次数落库而不是放内存 —— 采集器重启后计数不该归零，
		// 否则「重启即恢复正常」又会造出一批虚假的恢复通知。
		var fails int
		writeCtx, stopWrite := detachedWriteContext(hostCtx, 2*time.Second)
		if dbErr := c.db.QueryRow(writeCtx, `
UPDATE servers
   SET consecutive_host_failures = consecutive_host_failures + 1,
       status = CASE WHEN consecutive_host_failures + 1 >= $3 THEN 'warning' ELSE status END,
       status_error = $4,
       latency_ms = $2, updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL
 RETURNING consecutive_host_failures`, t.id, latencyMs, hostStableAfterFailures,
			"主机指标采集失败: "+truncateCollectorErr(firstNonEmptyString(sshclient.CleanCommandOutput(stderr), fmt.Sprint(err), fmt.Sprintf("退出码 %d", exit)))).Scan(&fails); dbErr != nil {
			c.logger.Warn("collector: mark warning failed", "server", t.name, "error", dbErr)
		}
		stopWrite()
		c.logger.Warn("collector: metrics command failed", "server", t.name, "exit", exit,
			"consecutive_failures", fails, "threshold", hostStableAfterFailures,
			"stderr", sshclient.CleanCommandOutput(stderr), "error", err)
		return
	}
	// 走到这里说明 SSH 会话是通的、命令也退出了，但必须再确认一件事：
	// 我们真的读到了本轮输出吗？
	//
	// ⚠️ 2026-10-24 审计 P2-1：空标准输出 + exit=0 **不是成功**。
	// 典型成因是 SSH 连接在命令执行中途被 RST（本机 8 台机器实测只在
	// 断线那一次留下 status_source=ssh 且指标停留在 7~21 天前，
	// 因为下一次采集会因连不上而走 offline/API 分支，再也没人改回 status）。
	// 原来的代码不校验这个，空 stdout 会 parseKeyValues 出一堆零值，
	// 然后照样写入 server_metrics —— 库里多一条「CPU 0%、内存 0」的假采样，
	// 看板显示一切正常，而机器其实早已不可采。
	//
	// 判据取 KERNEL 而不是「stdout 非空」：metricsScript 的每一行走的都是
	// `KEY=value`，只要脚本真的跑完过，KERNEL 必然有值；反过来，只要
	// KERNEL 缺失，就说明输出不完整（截断、空、或者根本不是这个脚本的输出）。
	// 只判非空会被「只回了一行就断」骗过去。
	values := parseKeyValues(stdout)
	if values["KERNEL"] == "" {
		if errors.Is(parent.Err(), context.Canceled) {
			return
		}
		var fails int
		writeCtx, stopWrite := detachedWriteContext(hostCtx, 2*time.Second)
		if dbErr := c.db.QueryRow(writeCtx, `
UPDATE servers
   SET consecutive_host_failures = consecutive_host_failures + 1,
       status = CASE WHEN consecutive_host_failures + 1 >= $3 THEN 'warning' ELSE status END,
       status_error = $4,
       latency_ms = $2, updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL
 RETURNING consecutive_host_failures`, t.id, latencyMs, hostStableAfterFailures,
			"主机指标输出不完整（SSH 会话可能中途断开）").Scan(&fails); dbErr != nil {
			c.logger.Warn("collector: mark warning failed", "server", t.name, "error", dbErr)
		}
		stopWrite()
		c.logger.Warn("collector: metrics output incomplete; keeping previous status",
			"server", t.name, "stdout_bytes", len(stdout),
			"consecutive_failures", fails, "threshold", hostStableAfterFailures)
		return
	}

	cpu := clampCollectorPercent(parseFloat(values["CPU"]))
	memTotalKb := max(parseInt(values["MEM_TOTAL_KB"]), int64(0))
	memAvailKb := max(parseInt(values["MEM_AVAIL_KB"]), int64(0))
	memTotal := memTotalKb * 1024
	memUsed := max((memTotalKb-memAvailKb)*1024, int64(0))
	if memTotal > 0 && memUsed > memTotal {
		memUsed = memTotal
	}
	diskUsed := max(parseInt(values["DISK_USED"]), int64(0))
	diskTotal := max(parseInt(values["DISK_TOTAL"]), int64(0))
	diskPercent := 0.0
	if diskTotal > 0 {
		diskPercent = float64(diskUsed) * 100 / float64(diskTotal)
	}

	loadJSON := formatLoadAverageJSON(values["LOAD"])
	if _, err := c.db.Exec(hostCtx, `INSERT INTO server_metrics (server_id, sampled_at, cpu_percent, memory_used_bytes, memory_total_bytes, disk_used_bytes, disk_total_bytes, load_average)
VALUES ($1, now(), $2, $3, $4, $5, $6, $7::jsonb)`, t.id, cpu, memUsed, memTotal, diskUsed, diskTotal, loadJSON); err != nil {
		c.logger.Warn("collector: insert metrics failed", "server", t.name, "error", err)
		return
	}

	// 资源告警判定（2026-10-17 降噪）：原来只看**这一次**的 CPU 瞬时值 ≥85%，
	// 阈值附近的主机（美国-阿什本 7 天 213 次、每次约 1 分钟）告警 → 恢复来回跳。
	//   - CPU 用最近 3 次采样的平均值，单次尖峰不算；
	//   - 滞回：已在告警中的主机，要降到 CPU <75% 且磁盘 <85% 才恢复。
	cpuAvg := cpu
	_ = c.db.QueryRow(hostCtx, `SELECT COALESCE(avg(cpu_percent), $2) FROM (
SELECT cpu_percent FROM server_metrics WHERE server_id=$1 ORDER BY sampled_at DESC LIMIT 3) recent`, t.id, cpu).Scan(&cpuAvg)
	var prevStatus string
	_ = c.db.QueryRow(hostCtx, `SELECT status FROM servers WHERE id=$1 AND deleted_at IS NULL`, t.id).Scan(&prevStatus)
	status := resourceStatus(prevStatus, cpuAvg, diskPercent)
	// consecutive_host_failures 在此归零：本轮 metrics 脚本跑通了，说明主机
	// 侧采集链路是好的。与 status 同一条 UPDATE 写入，避免「状态改了计数没改」
	// 导致下一轮一开始就被判成 warning（见审计 P0-2）。
	if _, err := c.db.Exec(hostCtx, `UPDATE servers SET status=$1, latency_ms=$2, status_error='', consecutive_host_failures=0, updated_at=now(), os_info=jsonb_set(COALESCE(os_info, '{}'::jsonb), '{kernel}', to_jsonb($3::text)) WHERE id=$4 AND deleted_at IS NULL`, status, latencyMs, values["KERNEL"], t.id); err != nil {
		c.logger.Warn("collector: update status failed", "server", t.name, "error", err)
	}
	c.probeNodes(hostCtx, client, t)
	c.logger.Debug("collector: sample collected", "server", t.name, "cpu", cpu, "ssh_handshake_ms", latencyMs)
}

// isSSHAuthFailure 判断 SSH 错误是不是"认证失败"（而非网络不通）。
//
// 为什么要区分：认证失败说明**机器是活的**（它应答了 SSH 握手），
// 只是我们凭据不对。这种情况还值得退回 API 探测；而网络不通
// （超时/拒绝连接）说明机器可能真挂了，再试 API 只是白等一次超时
// —— 那个 API 也在同一台机器上。
//
// 判定依据是错误文本。golang.org/x/crypto/ssh 没有导出可判定的
// 错误类型（`ssh: unable to authenticate` 是普通 errors.New），
// 所以只能匹配文本 —— 这在别的场景下是坏味道，但这里可接受，因为
// 这个字符串是**该库内部固定的**（不是业务文案），升级时若变了，
// 最坏结果是退回"直接标 offline"，不会造成错误的安全判断。
//
// ⚠️ 必须**大小写不敏感**：实测同一条语义的报错有两种写法 ——
//
//	ssh 库自己给的是小写  "ssh: unable to authenticate"
//	而 "Permission denied (publickey,password)." 是大写 P
//
// 只匹配小写会让后一种漏过去，表现是"那台机器永远显示 unknown"。
// 我的第一版就是这么写的，被单测抓住（TestIsSSHAuthFailure）。
func isSSHAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "no supported methods remain") ||
		strings.Contains(msg, "permission denied")
}

// nodeHasAPI 判断这台主机下有没有节点绑定了可用的 API 地址。
//
// 有才值得退回 API 探测 —— 否则退回去也只是立刻返回 no_api，
// 白跑一趟。
// 2026-11-06：查库失败要报出来，不能当成「没有 API」—— 那会让调用方把主机写成 offline，
// 触发 server_offline，健康闸门随即下线这台主机上好好的节点。
func (c *Collector) nodeHasAPI(ctx context.Context, serverID string) (bool, error) {
	var count int
	err := c.db.QueryRow(ctx, `
SELECT count(*) FROM hysteria_nodes
 WHERE server_id = $1 AND deleted_at IS NULL
   AND api_url IS NOT NULL AND api_url <> '' AND api_key IS NOT NULL`, serverID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// markServerSSHBroken 记下"SSH 凭据失效"这件事，但不改节点状态
// （节点状态由随后的 API 探测决定）。
//
// 为什么不直接把服务器标 offline：机器明明是活的（SSH 端口应答了），
// 标 offline 会让用户去查一台没问题的机器。这里只写 status_error，
// 让界面能显示"SSH 凭据失效，指标采不到，但节点在线"这种精确描述。
func (c *Collector) markServerSSHBroken(ctx context.Context, serverID string, cause error) {
	if _, err := c.db.Exec(ctx, `
UPDATE servers
   SET status_error = $2, updated_at = now()
 WHERE id = $1 AND deleted_at IS NULL`, serverID, "SSH 凭据失效: "+shortSSHErr(cause)); err != nil {
		c.logger.Warn("collector: 记录 SSH 失效失败", "server_id", serverID, "error", err)
	}
}

// 2026-11-03：指标采集失败原来只累计次数、不写 status_error ⇒ 未到 warning 阈值前
// 界面看不出任何问题，到了阈值也只是一个「警告」没有原因。成功的一轮会清空 status_error。
func truncateCollectorErr(msg string) string {
	msg = strings.TrimSpace(msg)
	if r := []rune(msg); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return msg
}

func clampCollectorPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" && v != "<nil>" {
			return v
		}
	}
	return ""
}

// sshDialFailureReason 把拨号失败归成操作者能据此行动的一句话。
func sshDialFailureReason(err error) string {
	var mismatch *sshclient.HostKeyMismatchError
	switch {
	case errors.As(err, &mismatch):
		return "SSH 主机指纹与登记的不一致（主机重装过？请核对后在主机页重新接受指纹）: " + shortSSHErr(err)
	case isSSHAuthFailure(err):
		return "SSH 凭据失效: " + shortSSHErr(err)
	default:
		return "SSH 连接失败: " + shortSSHErr(err)
	}
}

func shortSSHErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToValidUTF8(err.Error(), "")
	if len(msg) > 120 {
		cut := 120
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		return msg[:cut] + "…"
	}
	return msg
}

// probeNodesViaAPI 在没有 SSH 凭据时，用节点门户的 API 来判断节点是否在线。
//
// 它覆盖的能力与 SSH 路径的差异（诚实记录，界面上也要如实展示）：
//
//	能力             SSH 路径    API 路径
//	节点是否在线        ✅          ✅
//	监听端口是否在听     ✅          ⚠️ 只能看到节点**自报**的端口，不是实际监听状态
//	用户数              ⚠️ 间接      ✅ 直接返回
//	CPU/内存/磁盘       ✅          ❌ 拿不到
//	流量采样            ✅          ❌ 拿不到
//
// 所以走 API 的节点在界面上会显示"状态在线，但指标为空" ——
// 那是**如实反映**，不是 bug。status_source 字段让前端能说明这一点，
// 否则用户会以为采集器坏了。
func (c *Collector) probeNodesViaAPI(ctx context.Context, t target) {
	rows, err := c.db.Query(ctx, `
SELECT id, name, COALESCE(api_url,''), api_key
  FROM hysteria_nodes
 WHERE server_id = $1 AND deleted_at IS NULL
 ORDER BY created_at`, t.id)
	if err != nil {
		c.logger.Warn("collector: query nodes for api probe failed", "server", t.name, "error", err)
		return
	}
	type apiNode struct {
		id     string
		name   string
		apiURL string
		apiKey []byte
	}
	var nodes []apiNode
	for rows.Next() {
		var n apiNode
		if err := rows.Scan(&n.id, &n.name, &n.apiURL, &n.apiKey); err != nil {
			rows.Close()
			c.logger.Warn("collector: scan api nodes failed; skipping host this round", "server", t.name, "error", err)
			return
		}
		nodes = append(nodes, n)
	}
	// 2026-11-03：读不全时不能往下走。nodes 为空会把主机写成 unknown（有凭据时
	// 还会触发 server_unknown 告警），读了一半则只更新部分节点 —— 都是凭残缺数据下结论。
	if err := rows.Err(); err != nil {
		rows.Close()
		c.logger.Warn("collector: read api nodes failed; skipping host this round", "server", t.name, "error", err)
		return
	}
	rows.Close()

	if len(nodes) == 0 {
		// 主机行存在但没有节点（可能是删剩的空壳）。
		// 标成 unknown 而不是 offline —— 我们**没测过**，不该断言它挂了。
		if _, err := c.db.Exec(ctx, `UPDATE servers SET status='unknown', latency_ms=NULL, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, t.id); err != nil {
			c.logger.Warn("collector: mark unknown failed", "server", t.name, "error", err)
		}
		return
	}

	// 一台主机上可能有多个节点，但状态探测只需要成功一个就能判定
	// "这台主机是活的"。逐个试，**第一个成功的就定调**，
	// 后面的仍会各自更新自己的行（它们可能各自绑了不同的 API）。
	serverOnline := false
	serverLatency := 0
	var lastErrKind, lastErrDetail string
	// 2026-11-06：至少一个节点真的「探不通」（网络层失败）才能把主机判成 offline。
	// 原来全部节点「没法测」（密钥解不开、节点没绑 API、地址格式不对）时主机也写 offline ——
	// server_offline 告警随之触发，健康闸门 3 分钟内把这台主机上的节点从客户订阅里拿掉，而节点本身完好。
	anyUnreachable := false
	probedAny := false

	for _, n := range nodes {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			// 单机探测预算用尽：保留前面已完成的节点探测结果并继续汇总主机状态，不再直接 return 丢弃。
			break
		}
		probedAny = true

		apiKey := ""
		if len(n.apiKey) > 0 && c.box != nil {
			plain, err := c.box.Open(n.apiKey)
			if err != nil {
				// 密钥解不开：这不是"探不通"，探测根本没开始。
				// 用 probeOK=false 会把它算成不稳定 —— 那不对，问题在面板侧
				// （加密密钥换了 / 数据坏了），节点可能好得很。
				// 所以这里算探测成功（我们明确地得出了结论），只是结论是"没法测"。
				writeCtx, stopWrite := detachedWriteContext(ctx, 2*time.Second)
				c.updateNodeStatus(writeCtx, n.id, "unknown", "api", "unauthorized", "API 密钥解密失败", true)
				stopWrite()
				lastErrKind, lastErrDetail = "unauthorized", "API 密钥解密失败"
				continue
			}
			apiKey = string(plain)
		}

		started := time.Now()
		// 直接把 api_url 交给探测函数 —— 它内部会归一成**完整端点**。
		//
		// ⚠️ 这里原本是「先 normalizeAPIBase 拿 base、再自己拼 /node/meta」，
		// 而那个拼法漏了 /api/v1 前缀，生产上表现为
		// 「端口通但 HTTP 404」。改成把原始 api_url 交下去统一处理，
		// 让"前缀是什么"这件事只有一个地方知道。
		result := probeStatusViaAPI(ctx, n.apiURL, apiKey, 10*time.Second)
		elapsed := int(time.Since(started).Milliseconds())
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}

		// 节点探测本身可能刚好耗满预算，使用独立 2 秒短上下文确保状态能落库。
		writeCtx, stopWrite := detachedWriteContext(ctx, 2*time.Second)
		switch {
		case result.OK:
			c.updateNodeStatus(writeCtx, n.id, "online", "api", "", "", true)
			// 顺带用上报的端口纠正库里的记录。
			//
			// 为什么值得做：节点的监听端口可能在节点侧被改过
			// （或迁移过），而库里的值还是旧的。那条记录会让
			// "端口探测"与"界面展示"都跟着错。写上新的值并记日志，
			// 让这种漂移可见而不是被静默吸收。
			c.syncReportedListenPort(writeCtx, n.id, n.name, result.ListenPort)
			if !serverOnline {
				serverOnline = true
				serverLatency = elapsed
			}
		case result.ErrorKind == "unauthorized":
			// 能收到 401 说明**网络是通的、节点是活的**，只是凭据不对。
			// 这种情况标 online 还是 offline？
			//
			// 选 online：状态的语义是"节点在不在跑"，而它显然在跑
			// （它应答了）。凭据问题记在 status_error 里，让界面能
			// 同时显示"在线（API 鉴权失败，请更新密钥）"——
			// 标成 offline 会让人跑去机房查一台其实好好的机器。
			//
			// probeOK=true：**探测本身成功了**（拿到了 HTTP 应答），
			// 只是内容说凭据不对。这不是"探不通"，不该累计不稳定计数 ——
			// 否则一个密钥过期但网络好得很的节点会被标成"不稳定"，
			// 把人引向错误的方向（去查线路而不是换钥匙）。
			c.updateNodeStatus(writeCtx, n.id, "online", "api", "unauthorized", result.Detail, true)
			if !serverOnline {
				serverOnline = true
				serverLatency = elapsed
			}
		case result.ErrorKind == "invalid":
			// 2026-11-06：没绑 API、地址 / 证书配置不对、返回的不是节点门户 —— 面板这边就测不了，
			// 不是「节点连不上」。原来落进下面的 offline 分支：累计失败、3 轮后判离线并触发 node_offline，
			// 运维被引去查一台好好的机器。记 unknown + 原因，不累计不稳定计数。
			c.updateNodeStatus(writeCtx, n.id, "unknown", "api", "invalid", result.Detail, true)
			lastErrKind, lastErrDetail = result.ErrorKind, result.Detail
		default:
			// 这里才是真的"没探通"（超时 / 连接被拒 / DNS 失败…）→ probeOK=false。
			c.updateNodeStatus(writeCtx, n.id, "offline", "api", result.ErrorKind, result.Detail, false)
			lastErrKind, lastErrDetail = result.ErrorKind, result.Detail
			anyUnreachable = true
		}
		stopWrite()
	}

	if !probedAny || errors.Is(ctx.Err(), context.Canceled) {
		return
	}

	// 主机状态：只要有一个节点在线，主机就算在线（那台机器在跑东西）。
	serverWriteCtx, stopServerWrite := detachedWriteContext(ctx, 2*time.Second)
	defer stopServerWrite()
	if serverOnline {
		// 2026-11-06：API 路径不写 latency_ms / net_latency_ms。前者是 SSH 握手耗时，后者是面板到主机的
		// 纯 TCP 建连耗时（由监控调度器每 15 分钟测一次）；原来这里每 30 秒用整个 HTTPS 请求耗时
		// （DNS + TCP + TLS + 处理，通常是 RTT 的 3-4 倍）覆盖它们，监控页的「网络延迟」口径随之来回切换。
		// 纯 API 主机（无 SSH 凭据）恢复在线时同步清空残留的 status_error；带 SSH 凭据降级时保留 SSH 失败提示。
		query := `UPDATE servers SET status='online', updated_at=now() WHERE id=$1 AND deleted_at IS NULL`
		if !t.hasCred {
			query = `UPDATE servers SET status='online', status_error='', updated_at=now() WHERE id=$1 AND deleted_at IS NULL`
		}
		if _, err := c.db.Exec(serverWriteCtx, query, t.id); err != nil {
			c.logger.Warn("collector: update server via api failed", "server", t.name, "error", err)
		}
		c.logger.Debug("collector: 状态经 API 探测完成（无 SSH 凭据，仅状态）",
			"server", t.name, "nodes", len(nodes), "api_latency_ms", serverLatency)
	} else if !anyUnreachable {
		// 没有一个节点是「探不通」，全是面板侧测不了（解密失败 / 未绑 API / 配置不对）：主机记 unknown 并写明原因。
		if _, err := c.db.Exec(serverWriteCtx,
			`UPDATE servers SET status='unknown', latency_ms=NULL, status_error=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`,
			t.id, "无法经节点 API 判断主机状态："+lastErrDetail); err != nil {
			c.logger.Warn("collector: mark unknown via api failed", "server", t.name, "error", err)
		}
		c.logger.Warn("collector: 节点 API 无法用于探测（不是连不上）",
			"server", t.name, "nodes", len(nodes), "kind", lastErrKind, "detail", lastErrDetail)
	} else {
		// 全部节点的 API 都不通。
		//
		// ⚠️ 这里**不写 latency_ms / net_latency_ms**：那是"网络延迟"，
		// 而 API 不通可能是节点服务没起来（网络其实是通的）。
		// 写一个数字进去会污染延迟统计。
		if _, err := c.db.Exec(serverWriteCtx,
			`UPDATE servers SET status='offline', latency_ms=NULL, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`,
			t.id); err != nil {
			c.logger.Warn("collector: mark offline via api failed", "server", t.name, "error", err)
		}
		c.logger.Info("collector: 所有节点 API 均不可达",
			"server", t.name, "nodes", len(nodes), "kind", lastErrKind, "detail", lastErrDetail)
	}
}

// unstableAfterFailures 是「连续失败多少轮 → 判定为不稳定」的阈值。
//
// 为什么是 3：连续三轮都没探通，就不是偶发抖动了。而再往后若持续失败，
// 状态会自然滑向 offline —— 那是 critical 级（已有 node_offline 规则接管）。
// 所以 warning 覆盖的是"偶尔失败、偶尔成功"这个区间，
// 它以前被静默吞掉（失败那轮只写 status_error、保留 status 不动）。
const unstableAfterFailures = 3

// hostStableAfterFailures 是「机器级 metrics 脚本连续失败多少轮 → 判定为 warning」
// 的阈值，与节点侧的 unstableAfterFailures 取同一个值（3）。
//
// 为什么需要它（2026-10-24 审计 P0-2）：原来 metricsScript 失败一次就把
// servers.status 置 warning，下一轮成功又置回 online。告警评估对每次
// warning↔online 都新开一条 server_warning、恢复时再 resolve 一条，
// 于是「一次 SSH 抖动」= 一对告警。
//
// 实测：美国-阿什本一台机器产生 229 条 server_warning（单小时最高 24 条），
// 全库 alert_events 里 reopened 382 次 vs triggered 64 次 —— 65% 的告警
// 来自这一台抖动的主机，真正的 critical（节点离线）被淹没。
//
// 取 3 而非 1 的道理与节点侧相同：采集周期 30 秒，连续 3 轮（90 秒）
// 都跑不通才值得让人知道；偶发一次失败保持原状态，不制造状态翻转，
// 也就不制造告警。
const hostStableAfterFailures = 3

// updateNodeStatus 写节点状态与探测来源。
//
// 单独一个函数是为了让"探测结果 → 数据库"这层映射只有一处，
// 避免三条分支（online/offline/unauthorized）各写一遍 UPDATE
// 而其中一条忘了写 status_source。
//
// probeOK 表示这一轮探测**是否成功拿到结果**（不是"节点在线"）：
//   - 成功 → consecutive_probe_failures 归零
//   - 失败 → 累加；未达 unstableAfterFailures 时置 warning（不稳定，不进健康闸门），
//     连续达到阈值才置 offline；已是 offline 的保持 offline
//
// ⚠️ 为什么必须显式传 probeOK 而不是从 status 推断：
// "offline" 既可能是"探通了、发现端口没听"（探测本身成功），
// 也可能是"压根没探通"（探测失败）。这两者对"不稳定"的含义正好相反 ——
// 前者是确定的结果，后者才是可疑的。用 status 推断会把它们混为一谈。
func (c *Collector) updateNodeStatus(ctx context.Context, nodeID, status, source, errKind, errDetail string, probeOK bool) {
	// 一次 UPDATE 里同时维护状态与连续失败计数，避免"改了状态忘了计数"。
	//
	// 计数规则写成 SQL 表达式而不是先读后写：并发探测（理论上）与
	// 读-改-写之间的竞态会让计数漂移，而表达式是原子的。
	//
	// warning 的覆盖条件是 `consecutive >= 阈值 AND status <> 'offline'`：
	// 已 offline 的节点不该被降级成 warning —— 那会把一个 critical 问题
	// 显示成较轻的等级，属于"报警降级"，比不报更危险。
	//
	// 2026-11-04：只写未删除的节点。一轮探测从读节点列表到写结果最长十几秒：这期间节点被删除
	// （deleteNode 刚把 status 置成 offline、抹掉 api_url），在途的探测结果照样写回 online 和新的
	// last_checked_at —— 已删节点又显示「在线」，统计口径与告警都被污染。
	var err error
	if probeOK {
		_, err = c.db.Exec(ctx, `
UPDATE hysteria_nodes
   SET status=$1, status_source=$2, status_error=$3,
       consecutive_probe_failures = 0,
       last_checked_at=now(), updated_at=now()
 WHERE id=$4 AND deleted_at IS NULL`, status, source, errKind, nodeID)
	} else {
		// 2026-11-03：原来第一条写的是 `WHEN $1 = 'offline'`（传入值）而唯一的调用方
		// 恰好总传 "offline" —— 一次 10 秒超时就直接判离线，warning 永远到不了，
		// 隔轮失败的节点一直挂着 node_offline、5 分钟后被健康闸门从订阅里拿掉。
		// 现在按「当前值」判：已离线保持离线；连续失败未达阈值记为不稳定；
		// 达到阈值（约 90 秒持续不通）才离线 —— 真挂的节点照样会离线。
		_, err = c.db.Exec(ctx, `
UPDATE hysteria_nodes
   SET status = CASE
           WHEN status = 'offline' THEN 'offline'
           WHEN $1 <> 'offline' THEN status
           WHEN consecutive_probe_failures + 1 >= $5 THEN 'offline'
           ELSE 'warning'
       END,
       status_source=$2, status_error=$3,
       consecutive_probe_failures = consecutive_probe_failures + 1,
       last_checked_at=now(), updated_at=now()
 WHERE id=$4 AND deleted_at IS NULL`, status, source, errKind, nodeID, unstableAfterFailures)
	}
	if err != nil {
		c.logger.Warn("collector: update node status failed", "node_id", nodeID, "error", err)
	} else if errDetail != "" && (!probeOK || errKind != "") && c.logger != nil {
		c.logger.Debug("collector: node probe detail", "node_id", nodeID, "status", status, "kind", errKind, "detail", errDetail)
	}
}

// probeNodes 通过 SSH 探测节点端口（有 SSH 凭据时走这条）。
func (c *Collector) probeNodes(ctx context.Context, client *ssh.Client, t target) {
	rows, err := c.db.Query(ctx, `SELECT id, listen_port FROM hysteria_nodes WHERE server_id=$1 AND deleted_at IS NULL`, t.id)
	if err != nil {
		c.logger.Warn("collector: query nodes failed", "error", err)
		return
	}
	var nodes []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.id, &n.port); err != nil {
			rows.Close()
			c.logger.Warn("collector: scan nodes failed; skipping node probe this round", "server", t.name, "error", err)
			return
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		c.logger.Warn("collector: read nodes failed; skipping node probe this round", "server", t.name, "error", err)
		return
	}
	rows.Close()

	for _, n := range nodes {
		stdout, stderr, code, err := sshclient.Run(ctx, client, portProbeCommand(n.port))
		listening, parseErr := parsePortProbe(stdout, code)
		if err == nil && parseErr != nil {
			// 2026-11-03：命令「跑完了」但没有给出可信结论（主机没有 ss、ss 不认 -H、输出不是数字）。
			// 原来管道的退出码来自 wc，缺 ss 时照样输出 0、退出 0 ⇒ 节点被判 offline，
			// 健康门随即把一台好节点踢出订阅。现在按「探测失败」处理，保留原状态。
			err = parseErr
			if strings.TrimSpace(stderr) == "" {
				stderr = parseErr.Error()
			}
		}
		if err != nil {
			// 「在主机上跑命令」这一步本身失败了：
			//   - 可能是 SSH 会话刚建好但立刻断了
			//   - 可能是 ss 命令本身不存在（旧内核）
			//   - 可能是上下文超时（采集器留的时间不够）
			//
			// ⚠️ 这种"采集器能看到主机但看不到端口"的情况**也值得记下来**，
			// 否则节点 status 卡在最后一次成功的值上（很可能是 online），
			// 但实际上当前已经拿不到新数据 —— 用户看到 online 会以为一切正常，
			// 而指标其实已经过期。
			//
			// 用 status_error 记录（**保留 status 不动**，避免误诊 offline），
			// 让前端能看到「节点最后一次探测失败于...」。
			//
			// ⚠️ 2026-10-02 改：这里原来是裸 SQL，绕过了 updateNodeStatus，
			// 于是 SSH 路径的失败**不会累计 consecutive_probe_failures** ——
			// 有 SSH 凭据的节点永远不会被判定为"不稳定"（而三台主力节点
			// 恰好都走 SSH）。现改为走统一函数，probeOK=false 让它计数。
			//
			// 为什么 probeOK=false：这是真的要探端口但没探成
			// （SSH 会话断了 / ss 命令不存在 / 超时），正是不稳定的信号。
			c.markNodeProbeFailed(ctx, n.id, "命令失败: "+sshclient.CleanCommandOutput(stderr))
			c.logger.Warn("collector: node probe failed", "node_id", n.id, "error", err)
			continue
		}
		status := "offline"
		if listening {
			status = "online"
		}
		// status_source 标成 ssh，与 API 路径区分开 —— 界面上据此说明
		// 「这个状态是怎么来的」，用户才知道为什么有的节点有 CPU 数字、
		// 有的只有状态。
		//
		// probeOK=true：探测**成功执行**了（拿到了 ss 输出）。
		// 注意 status 可能是 offline —— 那是"探通了、发现端口没听"，
		// 是确定的结果，不是"不稳定"。这正是不从 status 推断 probeOK 的原因。
		c.updateNodeStatus(ctx, n.id, status, "ssh", "", "", true)
	}
	c.collectUsage(ctx, client, t, nodes)
}

// portProbeCommand / parsePortProbe 见 sshclient：与运维任务的健康检查共用同一套判据（2026-11-04）。
func portProbeCommand(port int) string { return sshclient.PortProbeCommand(port) }

func parsePortProbe(stdout string, exitCode int) (bool, error) {
	return sshclient.ParsePortProbe(stdout, exitCode)
}

// markNodeProbeFailed 记录一次"探测没能完成"，并累计不稳定计数。
//
// 与 updateNodeStatus 分开是因为语义不同：
//   - updateNodeStatus(status, ...) → 我们**得出了结论**（在线/离线）
//   - markNodeProbeFailed         → 我们**没能得出结论**（命令失败/会话断了）
//
// 后者保留 status 不动（避免把"不知道"误诊成"离线"），
// 但要累计计数 —— 连续几轮都拿不到结论，就是"不稳定"。
func (c *Collector) markNodeProbeFailed(ctx context.Context, nodeID, detail string) {
	if _, err := c.db.Exec(ctx, `
UPDATE hysteria_nodes
   SET status = CASE
           WHEN status = 'offline' THEN 'offline'
           WHEN consecutive_probe_failures + 1 >= $3 THEN 'warning'
           ELSE status
       END,
       status_source='ssh',
       status_error=$1,
       consecutive_probe_failures = consecutive_probe_failures + 1,
       last_checked_at=now(), updated_at=now()
 WHERE id=$2 AND deleted_at IS NULL`, detail, nodeID, unstableAfterFailures); err != nil {
		c.logger.Warn("collector: mark probe-failed failed", "node_id", nodeID, "error", err)
	}
}

// usageScript is the read-only half of the traffic probe: the hysteria listen port
// plus the portal's cumulative used_bytes bookkeeping.
const usageScript = `
port=$(awk '/^listen:/{gsub(/[^0-9]/,"",$2); print $2; exit}' /etc/hysteria/config.yaml 2>/dev/null)
total=$(python3 -c 'import json;d=json.load(open("/etc/hysteria/portal.json"));print(sum(int(u.get("used_bytes",0) or 0) for u in d.get("users",{}).values()))' 2>/dev/null)
echo "LISTEN_PORT=$port"
echo "USED_TOTAL=$total"
`

// One round trip covers both traffic views we publish:
//   - portal.json's used_bytes is a *cumulative* counter, so it is stored as
//     source='portal_used' and never summed as if it were a rate.
//   - hysteria's /traffic is read without resetting per-user counters. The
//     absolute snapshot and legacy bootstrap feed a database transaction that
//     computes tx/rx deltas. Only attempted when the operator opted in.
func (c *Collector) collectUsage(ctx context.Context, client *ssh.Client, t target, nodes []nodeRow) {
	// 主机监控可以独立于节点登记存在。没有有效节点时，不读取门户用量，
	// 更不能调用会清零增量计数的 /traffic?clear=true。主机指标仍由上层采集。
	// 已登记节点的监听端口错配告警必须保留，不能把真正的数据丢弃静默掉。
	if len(nodes) == 0 {
		return
	}
	probe := ""
	if t.trafficURL != "" && t.trafficFile != "" {
		// Remote probe only reads counters and the legacy cursor for bootstrap.
		probe = fmt.Sprintf(trafficSnapshotScript, shellArgument(t.trafficFile), shellArgument(t.trafficURL))
		// 2026-11-04：流量探针失败（密钥文件读不到、非 root 建不了状态目录、接口不通）不能拖垮整条命令。
		// 原来它是最后一条命令：它一失败，整条以非零退出，collectUsage 直接 return、不记任何日志 ——
		// 已经正确算出的 USED_TOTAL 也一并丢掉，流量图永远空着，而日志里什么都没有。
		probe = "{ " + probe + "} || echo TRAFFIC_ERR=$?\n"
	}
	stdout, stderr, exit, err := sshclient.Run(ctx, client, usageScript+probe+"\n")
	if err != nil || exit != 0 {
		c.logger.Warn("collector: usage probe failed", "server", t.name, "exit", exit, "error", err,
			"stderr", strings.TrimSpace(sshclient.CleanCommandOutput(stderr)))
		return
	}
	values := parseKeyValues(stdout)
	listenPort := int(parseInt(values["LISTEN_PORT"]))
	used := parseInt(values["USED_TOTAL"])
	// A node that reports a listen port but has no matching hysteria_nodes row is a
	// silent black hole: both traffic views below are keyed off that row, so the
	// numbers would be dropped without a trace. Surface it, otherwise a node the
	// operator believes is monitored simply never records anything.
	if listenPort > 0 && !hasNodePort(nodes, listenPort) {
		c.logger.Warn("collector: node reports listen port with no matching node row, traffic dropped",
			"server", t.name, "listen_port", listenPort)
	}
	if used > 0 && listenPort > 0 {
		for _, n := range nodes {
			if n.port != listenPort {
				continue
			}
			if _, err := c.db.Exec(ctx, `INSERT INTO traffic_samples (node_id, sampled_at, rx_bytes, tx_bytes, source)
VALUES ($1, date_trunc('minute', now()), 0, $2, 'portal_used')
ON CONFLICT (node_id, sampled_at, source) DO UPDATE SET tx_bytes = EXCLUDED.tx_bytes`, n.id, used); err != nil {
				c.logger.Warn("collector: store usage failed", "node_id", n.id, "error", err)
			}
			break
		}
	}
	if code := values["TRAFFIC_ERR"]; code != "" {
		c.logger.Warn("collector: traffic stats probe failed; portal usage still recorded", "server", t.name, "exit", code,
			"stderr", strings.TrimSpace(sshclient.CleanCommandOutput(stderr)))
		return
	}
	if values["TRAFFIC"] == "" {
		return
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, listenPort, t.trafficURL, values["TRAFFIC"]); err != nil {
		c.logger.Warn("collector: store cumulative traffic failed", "server", t.name, "error", err)
	}

}

// hysteria returns {"<user>":{"tx":N,"rx":N},...} keyed by authenticated user.
func sumHysteriaTraffic(raw string) (rx int64, tx int64, err error) {
	var perUser map[string]struct {
		Tx uint64 `json:"tx"`
		Rx uint64 `json:"rx"`
	}
	if err = json.Unmarshal([]byte(raw), &perUser); err != nil {
		return 0, 0, err
	}
	for _, entry := range perUser {
		rx += int64(entry.Rx)
		tx += int64(entry.Tx)
	}
	return rx, tx, nil
}

// hasNodePort reports whether any registered hysteria node listens on port.
func hasNodePort(nodes []nodeRow, port int) bool {
	for _, n := range nodes {
		if n.port == port {
			return true
		}
	}
	return false
}

func parseKeyValues(out string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	return values
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// formatLoadAverageJSON 将 /proc/loadavg 前三段数值安全编码为合法的 JSON 数字数组，
// 防止多余空格、制表符或异常字符拼出非法 JSON 导致整条主机指标插入失败。
func formatLoadAverageJSON(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "[]"
	}
	nums := make([]float64, 0, len(fields))
	for _, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return "[]"
		}
		nums = append(nums, v)
	}
	encoded, err := json.Marshal(nums)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func parseInt(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// resourceStatus 按资源占用给主机定状态，带滞回：进入告警的门槛高于退出门槛，
// 避免指标在阈值附近抖动时状态来回切换（每切一次就是一条告警 + 一条恢复）。
func resourceStatus(prev string, cpuAvg, diskPercent float64) string {
	if cpuAvg >= 85 || diskPercent >= 90 {
		return "warning"
	}
	if prev == "warning" && (cpuAvg >= 75 || diskPercent >= 85) {
		return "warning"
	}
	return "online"
}

// syncReportedListenPort 用节点 API 上报的监听端口纠正库里的记录（只在确实不同时写，并记日志）。
//
// 2026-11-04：同主机已有另一台在役节点占着这个端口时不改 —— 部分唯一索引会拒绝，
// 原来每 30 秒重试一次、每次报一条「同步端口失败」，实际上永远改不成。
// 条件写进 UPDATE：不满足就是 0 行，记一条说明为什么没同步。已删除的节点同样不写。
func (c *Collector) syncReportedListenPort(ctx context.Context, nodeID, nodeName string, reported int) {
	if reported <= 0 || reported > 65535 {
		return
	}
	var currentPort int
	if err := c.db.QueryRow(ctx, `SELECT listen_port FROM hysteria_nodes WHERE id=$1 AND deleted_at IS NULL`, nodeID).Scan(&currentPort); err != nil || currentPort == reported {
		return
	}
	tag, err := c.db.Exec(ctx,
		`UPDATE hysteria_nodes SET listen_port=$1, updated_at=now()
		  WHERE id=$2 AND deleted_at IS NULL
		    AND NOT EXISTS (SELECT 1 FROM hysteria_nodes o
		                     WHERE o.server_id = hysteria_nodes.server_id AND o.id <> hysteria_nodes.id
		                       AND o.listen_port = $1 AND o.deleted_at IS NULL)`,
		reported, nodeID)
	switch {
	case err != nil:
		c.logger.Warn("collector: 同步端口失败", "node", nodeName, "error", err)
	case tag.RowsAffected() == 0:
		c.logger.Warn("collector: 节点上报的端口已被同主机另一台节点占用，未同步；请核对节点配置",
			"node", nodeName, "库中", currentPort, "节点上报", reported)
	default:
		c.logger.Warn("collector: 节点上报的监听端口与库中记录不一致，已同步",
			"node", nodeName, "库中", currentPort, "节点上报", reported)
	}
}

// Detached writes must bound both pool acquisition and statement execution.
func detachedWriteContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

const trafficSnapshotScript = `python3 - %s %s <<'METERPY'
import hashlib,json,pathlib,subprocess,sys,time,urllib.request
secret=pathlib.Path(sys.argv[1]).read_text().strip()
url=sys.argv[2].rstrip('/')+'/traffic'
epoch=subprocess.check_output(['systemctl','show','hysteria-server','--property=InvocationID','--value']).decode().strip()
p=pathlib.Path('/var/lib/hy2-ops-traffic')/(hashlib.sha256(url.encode()).hexdigest()+'.json')
# Read the old cursor BEFORE sampling; never create or advance it here.
try:
 old=json.loads(p.read_text()) if p.exists() else None
except Exception:
 old=None
req=urllib.request.Request(url,headers={'Authorization':secret})
with urllib.request.urlopen(req,timeout=5) as r: current=json.load(r)
print('TRAFFIC='+json.dumps({'epoch':epoch,'counters':current,'observed_ns':time.time_ns(),'bootstrap':old}))
METERPY
`

func shellArgument(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type trafficCounter struct {
	Tx int64 `json:"tx"`
	Rx int64 `json:"rx"`
}
type trafficCursor struct {
	Epoch    string                    `json:"epoch"`
	Counters map[string]trafficCounter `json:"counters"`
}
type trafficSnapshot struct {
	trafficCursor
	ObservedNS int64          `json:"observed_ns"`
	Bootstrap  *trafficCursor `json:"bootstrap"`
}

// Cursor locking, bucket addition and cursor advancement share one commit. Replayed
// or older observations cannot add twice, even when the previous commit reply was lost.
func (c *Collector) storeTrafficSnapshot(ctx context.Context, nodes []nodeRow, port int, sourceURL, raw string) error {
	nodeID := ""
	for _, n := range nodes {
		if port > 0 && n.port == port {
			nodeID = n.id
			break
		}
	}
	if nodeID == "" {
		return nil
	} // no matching node: do not initialize/advance a cursor
	var sample trafficSnapshot
	if err := json.Unmarshal([]byte(raw), &sample); err != nil {
		return err
	}
	if sample.Epoch == "" || sample.ObservedNS <= 0 || sample.Counters == nil {
		return errors.New("incomplete traffic snapshot")
	}
	if err := validateTrafficCounters(sample.Counters); err != nil {
		return err
	}
	if sample.Bootstrap != nil {
		if sample.Bootstrap.Epoch == "" || sample.Bootstrap.Counters == nil {
			return errors.New("invalid legacy traffic cursor")
		}
		if err := validateTrafficCounters(sample.Bootstrap.Counters); err != nil {
			return err
		}
	}
	digest := sha256.Sum256([]byte(strings.TrimRight(sourceURL, "/") + "/traffic"))
	sourceKey := fmt.Sprintf("%x", digest)
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollbackCtx, cancel := detachedWriteContext(ctx, time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	// ON CONFLICT waits for concurrent initializers, then FOR UPDATE serializes
	// all readers against the same durable baseline.
	baseline := sample.trafficCursor
	if sample.Bootstrap != nil {
		baseline = *sample.Bootstrap
	}
	initial, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO collector_traffic_cursors(node_id,source_key,cursor,observed_ns)
 VALUES($1::uuid,$2,$3::jsonb,0) ON CONFLICT DO NOTHING`, nodeID, sourceKey, string(initial)); err != nil {
		return err
	}
	var stored []byte
	var observed int64
	if err = tx.QueryRow(ctx, `SELECT cursor,observed_ns FROM collector_traffic_cursors WHERE node_id=$1::uuid AND source_key=$2 FOR UPDATE`, nodeID, sourceKey).Scan(&stored, &observed); err != nil {
		return err
	}
	if sample.ObservedNS <= observed {
		return nil
	}
	var old trafficCursor
	if err = json.Unmarshal(stored, &old); err != nil {
		return err
	}
	rx, txBytes, next, err := trafficDifference(old, sample.trafficCursor)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO traffic_samples(node_id,sampled_at,rx_bytes,tx_bytes,source)
 VALUES($1::uuid,date_trunc('minute',now()),$2,$3,'hysteria_traffic')
 ON CONFLICT(node_id,sampled_at,source) DO UPDATE SET rx_bytes=traffic_samples.rx_bytes+EXCLUDED.rx_bytes,tx_bytes=traffic_samples.tx_bytes+EXCLUDED.tx_bytes`, nodeID, rx, txBytes); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE collector_traffic_cursors SET cursor=$3::jsonb,observed_ns=$4,updated_at=now() WHERE node_id=$1::uuid AND source_key=$2`, nodeID, sourceKey, string(encoded), sample.ObservedNS); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateTrafficCounters(counters map[string]trafficCounter) error {
	for _, v := range counters {
		if v.Tx < 0 || v.Rx < 0 {
			return errors.New("negative traffic counter")
		}
	}
	return nil
}

func trafficDifference(old, current trafficCursor) (rx, tx int64, next trafficCursor, err error) {
	next = trafficCursor{Epoch: current.Epoch, Counters: map[string]trafficCounter{}}
	if old.Epoch == current.Epoch {
		for uid, v := range old.Counters {
			next.Counters[uid] = v
		}
	}
	for uid, v := range current.Counters {
		delta := v
		if prior, ok := old.Counters[uid]; old.Epoch == current.Epoch && ok {
			// Per-direction reset: a drop in one counter must not discard the
			// other direction's valid increment in the same epoch.
			if v.Tx < prior.Tx {
				delta.Tx = 0
			} else {
				delta.Tx -= prior.Tx
			}
			if v.Rx < prior.Rx {
				delta.Rx = 0
			} else {
				delta.Rx -= prior.Rx
			}
		}
		if delta.Rx > math.MaxInt64-rx || delta.Tx > math.MaxInt64-tx {
			return 0, 0, next, errors.New("traffic counter total overflow")
		}
		rx += delta.Rx
		tx += delta.Tx
		next.Counters[uid] = v
	}
	return
}
