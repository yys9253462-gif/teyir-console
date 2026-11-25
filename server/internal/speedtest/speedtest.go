// Package speedtest — 大陆三网测速。
//
// 为什么测速在**节点本机**执行，而不是在面板上
// --------------------------------------------
// 面板部署在东京，它 ping 大陆靶点测的是「东京 ↔ 大陆」。而用户关心的是
// 「我家（电信/联通/移动）↔ 这台节点」的体验 —— 这两件事毫无关系。
// 一台美国节点可能回国线路极好（CN2 GIA）而东京到它绕路，反之亦然。
//
// 所以正确做法是：面板 SSH 到节点 → 在**节点本机**跑探针脚本 →
// 结果回传。这样测出来的才是这台节点自己的回国线路质量，
// 也就是"这台机器好不好用"的核心指标。
//
// 探针为什么是一个 shell 片段而不是一个要上传的脚本文件
// ------------------------------------------------------
// 上传文件需要知道节点的目录布局、要有写权限、还要处理清理。
// 而这个探针只有几十行、无依赖（只用 ping / awk / date），
// 直接通过 SSH 执行一段命令即可 —— 项目里 healthCheck 等已经这么做了。
// 少一个"分发与清理"的环节，就少一类故障。
//
// 依赖探测是必须的：**不是每台节点都装了 ping**（实测一些精简镜像没有），
// 而且不同发行版的 ping 输出格式不一样（BusyBox 与 iputils 差异很大）。
// 脚本先自检依赖，缺失就明确报出来，而不是让解析层拿到空输出后
// 报一个含混的错。
package speedtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nodeops/server/internal/secrets"
	"nodeops/server/internal/sshclient"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Target 是一个测速靶点。
type Target struct {
	Key     string
	Name    string
	Host    string
	Carrier string
}

// Result 是对一个靶点的一次测量。
//
// 所有数值字段用指针或 *float64 之外的"可空"表示（这里用 float64 + Has 布尔，
// 更直观）：**必须能区分"测到 0ms"与"没测到"**。
// 用 0 兼职表示"没测到"会让界面显示"0 ms"，那是假数据。
type Result struct {
	TargetKey string
	Name      string
	Host      string
	Carrier   string

	ICMPAvgMs    *float64
	ICMPLossPct  *float64
	TCPConnectMs *float64
	DownloadMbps *float64
	UploadMbps   *float64

	// ProbeOK 表示这个名字的探针**整体**跑成功了。
	ProbeOK    bool
	ProbeError string
}

// Prober 负责在一台节点上跑测速。
type Prober struct {
	logger *slog.Logger
	db     *pgxpool.Pool
	box    *secrets.Box
	// Timeout 是单台节点整轮测速的**兜底上限**。
	//
	// 它必须随靶点数量增长：每个靶点 10 个包 ≈ 11 秒（见 perTargetBudget），
	// 靶点 6 个一批并行（理由见 probeScript 注释），预算仍按串行给，留足余量。
	//
	// ⚠️ 这里踩过一次：Timeout 写死 90 秒，按 **12 个**靶点的 72 秒算的。
	// 迁移 022 把靶点扩到 18 个后，最坏耗时变成 108 秒 > 90 秒 ——
	// 最坏情况下**每一轮都超时**，写不进任何数据，
	// 页面上的表现是"三网数据待测"且怎么点测速都不出数。
	// 静态值在靶点可配的系统里必然过期：改表加一个靶点就可能踩到。
	// 现在改成按实际靶点数动态计算，留 50% 余量。
	//
	// 硬上限 10 分钟：后台批量任务的 context 也是 10 分钟，
	// 再大也没意义（而且真超时了应该由日志里的 probe_error 暴露出来，
	// 而不是靠一个静默掐断的上限）。
	Timeout time.Duration
}

func NewProber(logger *slog.Logger, db *pgxpool.Pool, box *secrets.Box) *Prober {
	return &Prober{logger: logger, db: db, box: box}
}

// perTargetBudget 是单个靶点的最坏耗时预算（秒）。
//
// 探针里是 `ping -c 10 -W 2`：10 个包、间隔 1 秒，最后一个包再等 2 秒，
// 单靶点约 11 秒。2026-11-01 由 3 包改 10 包：3 包时丢包率只有
// 0/33/67/100% 四档，一个包丢失就跳 33%，读数太粗。
//
// 靶点在脚本里按 6 个一组并行，所以整轮实际耗时约
// ceil(靶点数/6) × 11 秒；这里仍按「全部串行」给预算，宁可超时宽松。
const perTargetBudget = 12

// probeTimeout 按靶点数量算整轮超时。
//
// 抽成独立函数是为了能单测"靶点变多时超时跟着涨"这个性质 ——
// 上面注释里记的那个 bug（12 个靶点的静态 90 秒）正是缺这个测试的后果。
// 用例写在 speedtest_test.go 的 TestProbeTimeoutScalesWithTargets。
func probeTimeout(targetCount int) time.Duration {
	if targetCount <= 0 {
		// 没有靶点时不会走到这里（调用前已判空），但给个下限，
		// 免得负数或零在 time.AfterFunc 里变成"立即超时"的陷阱。
		return 30 * time.Second
	}
	// 余量 50%：脚本自身的启动、SSH 通道往返、解析耗时都算在里面。
	budget := time.Duration(float64(targetCount*perTargetBudget) * 1.5 * float64(time.Second))
	// 上限 10 分钟，与后台批量任务的 context 超时对齐 ——
	// 超过它的部分等不到，只会白占一个 SSH 会话。
	const hardCap = 10 * time.Minute
	if budget > hardCap {
		return hardCap
	}
	return budget
}

// probeScript 是下发到节点执行的探针。
//
// 设计要点（每一条都是为了应对实测会遇到的情况）：
//
//  1. **先探测依赖**。没有 ping 就直接报 NO_PING 退出 —— 让上层知道
//     "这台机器测不了"而不是"靶点全不通"。这两者对运维的含义完全不同。
//
//  2. **小批并行，每批 6 个**（2026-11-01 前是全串行）。改 10 包后串行
//     要 18×11≈200 秒/台，4 路并发下一轮 15 台节点超出 10 分钟上限。
//     每个 ping 每秒只发一个 84 字节的包，6 个并行对带宽可以忽略，
//     不会互相推高延迟；portal 通道早就是 8 路并行。
//     输出先写临时文件，全部结束后按原顺序打印，解析层不受影响。
//
//  3. **解析按 iputils 与 BusyBox 两种格式**。iputils 输出
//     `rtt min/avg/max/mdev = 1.2/3.4/5.6/0.7 ms`，BusyBox 输出
//     `round-trip min/avg/max = 1.2/3.4/5.6 ms`。只认一种会导致
//     部分节点永远"未测得"，而原因看不出来。
//
//  4. **丢包率从统计行取**，不从 ping 退出码推断（退出码在不同实现里
//     含义有差异，且丢包但收到 1 个包时退出码可能仍是 0）。
//
//  5. 输出用 `KEY=VALUE` 的行格式，解析层按前缀取，不猜位置。
//     这样以后加指标不需要改解析结构。
//
// 参数用 %s 注入 host 与 key —— **注入前必须校验**（见 validTargetHost），
// 因为这段字符串会被送进远程 shell 执行。
const probeScript = `
set -u
if ! command -v ping >/dev/null 2>&1; then
  echo "PROBE_ERROR=NO_PING"
  exit 0
fi

echo "PROBE_OK=1"

tmp=$(mktemp -d 2>/dev/null || echo /tmp/nodeops-probe.$$)
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

# 10 个包（间隔 1 秒）、最后一个包最多等 2 秒；每批 6 个靶点并行。
i=0
for entry in %s; do
  # key 只含字母数字与 -_（validTargetKey），在第一个冒号处切：IPv6 地址本身含冒号，
  # 原来的 ${entry##*:} 会把 2001:db8::1 切成 1（2026-11-03）。
  host=${entry#*:}
  ping -c 10 -W 2 "$host" >"$tmp/$i" 2>/dev/null &
  i=$((i+1))
  if [ $((i %% 6)) -eq 0 ]; then wait; fi
done
wait

i=0
for entry in %s; do
  key=${entry%%%%:*}
  raw=$(cat "$tmp/$i" 2>/dev/null)
  i=$((i+1))

  loss=$(echo "$raw" | grep -oE '[0-9]+([.][0-9]+)?%% packet loss' | head -n1 | grep -oE '^[0-9]+([.][0-9]+)?')
  avg=$(echo "$raw" | grep -oE '= [0-9.]+/[0-9.]+/[0-9.]+' | head -n1 | awk -F'[ /=]+' '{print $3}')

  echo "TARGET=$key"
  echo "LOSS=${loss:-}"
  echo "AVG=${avg:-}"
done
`

// validTargetHost 校验靶点地址，防止 shell 注入。
//
// 这段值会被拼进远程执行的 shell 命令里。靶点来自数据库（虽然是我们预置的），
// 但用户以后可以在界面上加靶点 —— 那时它就是用户输入。所以必须在
// 拼接前校验，而不是信任来源。
//
// 只接受 IPv4/IPv6 字面量或合法主机名，且不允许任何 shell 元字符。
func validTargetHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	if trimmed != host || trimmed == "" || len(trimmed) > 253 {
		return false
	}
	// 明确拒绝一切可能被 shell 解释的字符。
	if strings.ContainsAny(trimmed, " \t\n\r;|&$`(){}[]<>'\"\\!*?~#") {
		return false
	}
	// 只允许字母、数字、点、冒号、连字符（覆盖 IPv4/IPv6/域名）。
	for _, r := range trimmed {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == ':' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// BuildProbeCommand 拼出下发给节点的完整命令。
//
// 单独导出是为了能在测试里断言"注入防护生效了" —— 这是安全相关的一段，
// 必须可测。
func BuildProbeCommand(targets []Target) (string, error) {
	if len(targets) == 0 {
		return "", fmt.Errorf("没有可用的测速靶点")
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		if !validTargetHost(t.Host) {
			return "", fmt.Errorf("靶点地址不合法: %q", t.Host)
		}
		if !validTargetKey(t.Key) {
			return "", fmt.Errorf("靶点标识不合法: %q", t.Key)
		}
		parts = append(parts, t.Key+":"+t.Host)
	}
	list := strings.Join(parts, " ")
	return fmt.Sprintf(probeScript, list, list), nil
}

// validTargetKey 靶点标识同样会被拼进命令，需要校验。
func validTargetKey(key string) bool {
	trimmed := strings.TrimSpace(key)
	if trimmed != key || trimmed == "" || len(trimmed) > 64 {
		return false
	}
	for _, r := range trimmed {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 输出解析
// ---------------------------------------------------------------------------

// rttAvgPattern 匹配两种常见 ping 实现的平均值。
//
//	iputils:  rtt min/avg/max/mdev = 1.234/5.678/9.012/1.234 ms
//	BusyBox:  round-trip min/avg/max = 1.234/5.678/9.012 ms
//
// 共同点是 `= a/b/c` 且 avg 是第二项，所以只锚定这个形状，不锚定前缀名。
var (
	rttAvgPattern  = regexp.MustCompile(`=\s*([0-9.]+)/([0-9.]+)/[0-9.]+`)
	probeErrorLine = regexp.MustCompile(`^PROBE_ERROR=(.*)$`)
)

// ParseProbeOutput 把探针的 stdout 解析成结果列表。
//
// 纯函数、无 IO，方便逐种格式单测 —— 这是整个功能里最容易被
// 不同 ping 实现打脸的地方。
func ParseProbeOutput(raw string, targets []Target) []Result {
	byKey := make(map[string]*Result, len(targets))
	order := make([]string, 0, len(targets))
	for _, t := range targets {
		r := &Result{
			TargetKey: t.Key, Name: t.Name, Host: t.Host, Carrier: t.Carrier,
		}
		byKey[t.Key] = r
		order = append(order, t.Key)
	}

	var probeErr string
	var sawProbeOK bool
	var current *Result

	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if match := probeErrorLine.FindStringSubmatch(line); match != nil {
			probeErr = match[1]
			continue
		}
		if line == "PROBE_OK=1" {
			sawProbeOK = true
			continue
		}
		if strings.HasPrefix(line, "TARGET=") {
			current = byKey[strings.TrimPrefix(line, "TARGET=")]
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, "LOSS=") {
			value := strings.TrimPrefix(line, "LOSS=")
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				// Only valid statistics count as a successful target; never turn an
				// out-of-range loss into an apparently legitimate 100% loss.
				if parsed >= 0 && parsed <= 100 {
					current.ICMPLossPct = clampPtr(parsed, 0, 100)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "AVG=") {
			value := strings.TrimPrefix(line, "AVG=")
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				if parsed >= 0 && parsed <= 60000 {
					current.ICMPAvgMs = clampPtr(parsed, 0, 60000)
				}
			}
			continue
		}
	}

	results := make([]Result, 0, len(order))
	for _, key := range order {
		r := byKey[key]
		switch {
		case probeErr != "":
			r.ProbeOK = false
			r.ProbeError = probeErr
		case !sawProbeOK:
			// 既没有 PROBE_OK 也没有明确的 PROBE_ERROR —— 说明输出完全
			// 不符合预期（可能 shell 不兼容、或命令被截断）。
			// 这种情况以前会被当成"所有靶点都没测到"，看不出根因。
			r.ProbeOK = false
			r.ProbeError = "探针输出不符合预期"
		case r.ICMPLossPct == nil || (*r.ICMPLossPct < 100 && r.ICMPAvgMs == nil):
			r.ProbeError = "靶点未返回有效统计"
		default:
			r.ProbeOK = true
		}
		results = append(results, *r)
	}
	return results
}

func clampPtr(value, low, high float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	if value < low {
		value = low
	}
	if value > high {
		value = high
	}
	return &value
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// RunOnServer 在一台主机上执行一轮测速，并把结果写库。
//
// 返回每个节点的结果条数，用于日志与调用方判断。
func (p *Prober) RunOnServer(ctx context.Context, serverID string, nodeIDs []string) (int, error) {
	targets, err := p.loadTargets(ctx)
	if err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, nil // 没有启用的靶点，静默跳过（用户可能全禁用了）
	}
	command, err := BuildProbeCommand(targets)
	if err != nil {
		return 0, err
	}

	// 超时按**本轮实际靶点数**算，不用构造时的静态值（理由见 probeTimeout）。
	timeout := probeTimeout(len(targets))
	if p.Timeout > 0 {
		// 测试可以显式覆盖；生产不设，由 probeTimeout 按靶点数定。
		timeout = p.Timeout
	}

	// 取主机的 SSH 凭据 —— 复用 collector 的取法（最新一条凭据）。
	var host, user, fingerprint string
	var port int
	var authType string
	var ciphertext []byte
	err = p.db.QueryRow(ctx, `
SELECT s.host, s.ssh_port, s.ssh_user, COALESCE(s.host_fingerprint, ''),
       COALESCE(sc.auth_type, ''), sc.ciphertext
FROM servers s
LEFT JOIN LATERAL (
    SELECT auth_type, ciphertext FROM server_credentials x
    WHERE x.server_id = s.id ORDER BY x.created_at DESC LIMIT 1
) sc ON true
WHERE s.id = $1::uuid AND s.deleted_at IS NULL`, serverID).Scan(&host, &port, &user, &fingerprint, &authType, &ciphertext)
	if err != nil {
		return 0, fmt.Errorf("读取主机凭据失败: %w", err)
	}
	if len(ciphertext) == 0 || p.box == nil {
		return 0, fmt.Errorf("该主机没有 SSH 凭据，无法测速")
	}
	// 2026-11-03：主机指纹还没登记时不连。Dial 在 pinned 为空时接受任何主机密钥，
	// 而这里随后会把保存的密码 / 私钥发给应答方；指纹由采集器首轮登记，等它即可。
	if strings.TrimSpace(fingerprint) == "" {
		return 0, fmt.Errorf("主机指纹尚未登记（等采集器首轮连接后再测速）")
	}
	plain, err := p.box.Open(ciphertext)
	if err != nil {
		return 0, fmt.Errorf("解密凭据失败: %w", err)
	}
	if authType == "" {
		authType = "ssh_key"
	}
	cred := sshclient.Credential{Type: authType, Secret: plain}

	client, _, err := sshclient.Dial(ctx, host, port, user, cred, fingerprint)
	if err != nil {
		return 0, fmt.Errorf("SSH 连接失败: %w", err)
	}
	defer client.Close()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, exit, err := sshclient.Run(runCtx, client, command)
	if err != nil {
		return 0, fmt.Errorf("执行探针失败: %w", err)
	}
	if exit != 0 {
		// 探针脚本刻意用 exit 0 表达"跑完了但有情况"（如 NO_PING），
		// 所以非零退出说明是更底层的问题（shell 不支持、命令被拒）。
		return 0, fmt.Errorf("探针退出码 %d: %s", exit, truncate(strings.TrimSpace(stderr), 200))
	}

	results := ParseProbeOutput(stdout, targets)

	// 本轮所有行共用一个 round_id。
	//
	// 为什么在应用侧生成而不是靠数据库的 now()：
	// 「一轮」是**执行语义**，不是时间语义。用时间戳去逼近它，
	// 就需要一个"多近才算同一轮"的魔法数 —— 而这个数必然出错：
	// 调大了把相邻两轮混成一轮（实测踩过：间隔 24 秒的两次测速
	// 被当成一轮，同一靶点在界面上出现两次、平均值也算错），
	// 调小了又会把同一轮的行拆开（轮内各行的时间戳本来就差几百毫秒）。
	//
	// 显式生成一个 id 就彻底消除了这个问题：同轮共享、跨轮不同，
	// 读取时 `WHERE round_id = (最新那轮的 id)` 精确且无歧义。
	roundID, err := newUUID()
	if err != nil {
		return 0, fmt.Errorf("生成测速轮次标识失败: %w", err)
	}

	// 把结果写到**每个节点**上。
	//
	// 为什么按节点而非按主机存：一台主机上可能跑多个 hysteria 节点，
	// 它们监听不同端口、可能走不同出口（有的配了 warp 分流）。
	// 但同一台机器的三网质量本质相同，所以这里对每个节点都写一份 ——
	// 好处是"按节点看"和"按主机看"两个视角都能直接查，不用做归属推断。
	//
	// 同一轮里所有节点共用同一个 roundID：这样"第 N 次全量测速"也能
	// 按 round_id 聚合（比如以后想做"全网三网趋势"）。
	written, err := p.storeResults(ctx, serverID, nodeIDs, results, roundID)
	if err != nil {
		return written, err
	}
	if err := usableProbeResults(results); err != nil {
		return written, err
	}
	if written > 0 {
		p.logger.Info("speedtest: 完成", "server", serverID, "nodes", len(nodeIDs),
			"targets", len(results), "rows", written, "round", roundID)
	}
	return written, nil
}

// storeResults 把一轮结果落库，返回写入行数。
//
// 为什么抽出来而不是让 RunOnServer 与 RunOnPortalAndStore 各写一份：
// 两条通道的**唯一**区别是"结果从哪来"（SSH 探针 vs 节点 portal），
// 落库格式、round_id 语义、幂等策略完全一样。
// 抄第二份的代价是：以后改列（比如加个 tcp_connect_ms 的真实填充）
// 很容易只改一处，然后两条通道的数据形态悄悄分叉 ——
// 而这种分叉只有查库才看得出来，界面上反而"都挺正常"。
func (p *Prober) storeResults(ctx context.Context, serverID string, nodeIDs []string, results []Result, roundID string) (int, error) {
	written := 0
	var firstErr error
	failed := 0
	for _, nodeID := range nodeIDs {
		for _, r := range results {
			// 2026-11-04：按真正插入的行数计数。ON CONFLICT DO NOTHING 跳过的行（同一秒内同一节点
			// 同一靶点已有一条，例如手动测速与定时轮次撞在同一秒）原来也算「已写入」，
			// 于是 RunOnPortalAndStore 的「全部冲突」判断永远不会成立、日志里的行数也偏大。
			tag, err := p.db.Exec(ctx, `
INSERT INTO node_speedtests
    (node_id, server_id, target_key, target_name, target_host, carrier,
     icmp_avg_ms, icmp_loss_pct, tcp_connect_ms, download_mbps, upload_mbps,
     probe_ok, probe_error, round_id)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, NULL, NULL, NULL, $9, $10, $11::uuid)
ON CONFLICT DO NOTHING`,
				nodeID, serverID, r.TargetKey, r.Name, r.Host, r.Carrier,
				r.ICMPAvgMs, r.ICMPLossPct, r.ProbeOK, r.ProbeError, roundID)
			if err != nil {
				p.logger.Warn("speedtest: 写入结果失败",
					"node", nodeID, "target", r.TargetKey, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				failed++
				continue
			}
			written += int(tag.RowsAffected())
		}
	}
	// Any failed insert makes the round incomplete. Preserve the actual count,
	// including when some earlier inserts committed or were skipped as conflicts.
	if failed > 0 {
		return written, fmt.Errorf("测速结果部分或全部写入失败（已写入 %d 行，失败 %d 行）: %w", written, failed, firstErr)
	}
	return written, nil
}

// RunOnPortalAndStore 是 portal 通道的"测速 + 落库"一步到位版本。
//
// 为什么不把落库交给调用方：round_id 的生成与"同轮共享"的语义是
// 采集器内部的事（见 RunOnServer 里那段注释），暴露成两步会让调用方
// 有机会生成不一致的 round_id —— 那正是项目里踩过的坑
// （间隔 24 秒的两轮被时间窗口混成一轮）。
func (p *Prober) RunOnPortalAndStore(ctx context.Context, nodeID, serverID, baseURL, apiKey string) error {
	results, err := p.RunOnPortal(ctx, nodeID, baseURL, apiKey)
	if err != nil {
		return err
	}
	roundID, err := newUUID()
	if err != nil {
		return fmt.Errorf("生成测速轮次标识失败: %w", err)
	}
	written, err := p.storeResults(ctx, serverID, []string{nodeID}, results, roundID)
	if err != nil {
		return err
	}
	if written == 0 {
		return errors.New("结果未写入数据库（全部冲突或写入失败）")
	}
	p.logger.Info("speedtest: portal 通道完成", "node", nodeID, "server", serverID,
		"targets", len(results), "rows", written, "round", roundID)
	return nil
}

// loadTargets 读出启用的靶点。
func (p *Prober) loadTargets(ctx context.Context) ([]Target, error) {
	rows, err := p.db.Query(ctx, `
SELECT target_key, name, host, carrier
FROM speedtest_targets WHERE enabled = true ORDER BY sort_order, target_key`)
	if err != nil {
		return nil, fmt.Errorf("读取测速靶点失败: %w", err)
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		// 2026-11-03：扫描失败原来直接 continue —— 那个靶点从本轮静默消失，
		// 界面上「这一组没数据」与「没测」分不开。与 rows.Err() 一样整体报错。
		if err := rows.Scan(&t.Key, &t.Name, &t.Host, &t.Carrier); err != nil {
			return nil, fmt.Errorf("读取测速靶点失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LoadTargets 供 API 层读取靶点清单（同一份查询逻辑，避免两处不一致）。
func (p *Prober) LoadTargets(ctx context.Context) ([]Target, error) {
	return p.loadTargets(ctx)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// newUUID 生成一个 RFC 4122 v4 UUID。
//
// 为什么自己写 20 行而不是引 github.com/google/uuid：
// 项目此前从没在 Go 侧生成过 UUID（全都用数据库的 gen_random_uuid()），
// 而这里需要"一次探针执行生成一个、之后所有行共用"——那个值必须在
// 应用侧产生（见调用处注释）。为了这一个用途引入一个第三方依赖
// 不划算：多一个需要跟进安全公告的包，换来的只是省下 20 行标准库代码。
//
// 正确性要点（v4 的规定动作，写漏了会生成不合法的 UUID）：
//   - 第 7 字节高 4 位设为 0100（版本号 4）
//   - 第 9 字节高 2 位设为 10（variant，RFC 4122）
func newUUID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // 版本 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// netDialer 是 DialOnly 用的小包装，让超时可配且不污染全局。
type netDialer struct{ timeout time.Duration }

func (d *netDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: d.timeout}).DialContext(ctx, network, address)
}

// DialOnly 测量到目标地址的**纯 TCP 建连耗时**，不含 SSH 握手。
//
// 为什么需要它：servers.latency_ms 是 SSH 握手耗时，被当成"网络延迟"
// 展示会误导（SSH 端口被封的节点显示无延迟，但它的代理可能好得很）。
// 这里单独测一次 TCP 建连，给"节点监控"页提供语义正确的网络延迟。
func DialOnly(ctx context.Context, host string, port int, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	started := time.Now()
	conn, err := (&netDialer{timeout: timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return int(time.Since(started).Milliseconds()), nil
}

func usableProbeResults(results []Result) error {
	for _, r := range results {
		if r.ProbeOK {
			return nil
		}
	}
	return errors.New("本轮测速没有可用靶点统计")
}
