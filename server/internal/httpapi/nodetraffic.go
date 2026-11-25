package httpapi

// nodetraffic.go — 节点整机流量计量、月度限额与内核版本（2026-10-14）。
//
// 为什么不复用已有的用量数据：
//   - traffic_samples 里的 hysteria_traffic / portal_used 只来自 SSH 采集，
//     只接了 API 的节点完全没有；
//   - portal_used 是 Hysteria 用户的配额记账，只在重新鉴权时前进，
//     且不含 Reality / GOST 等其它协议 —— 和 VPS 商家计费的数字对不上。
// 这里读节点 portal 的 GET /api/v1/node/traffic（默认路由网卡的累计计数器），
// 这才是商家计费的那个数。
//
// 口径：上传 + 下载双向合计；每月 traffic_reset_day 号北京时间 0 点重置。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// 北京时间。容器镜像里不一定带 tzdata，用固定时区避免 LoadLocation 失败。
var trafficCycleZone = time.FixedZone("CST", 8*3600)

const (
	nodeTrafficInterval  = time.Minute
	nodeVersionInterval  = 30 * time.Minute
	nodeTrafficCallLimit = 15 * time.Second
)

// trafficCycleStart 返回 now 所在计费周期的起点：最近一个「resetDay 号 0 点」。
// resetDay 超过当月天数时取当月最后一天（31 号重置在 2 月就是 28/29 号）。
func trafficCycleStart(now time.Time, resetDay int) time.Time {
	if resetDay < 1 {
		resetDay = 1
	}
	local := now.In(trafficCycleZone)
	start := cycleAnchor(local.Year(), local.Month(), resetDay)
	if local.Before(start) {
		prev := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, trafficCycleZone).AddDate(0, -1, 0)
		start = cycleAnchor(prev.Year(), prev.Month(), resetDay)
	}
	return start
}

func cycleAnchor(year int, month time.Month, day int) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, trafficCycleZone).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, 0, 0, 0, 0, trafficCycleZone)
}

// trafficDelta 计算两次采样之间的增量。
//
// 计数器是开机以来的累计值：同一次开机且没有回退时取差值；
// 重启（boot_id 变了）或计数器回退时，新计数器本身就是重启后的增量。
// rx / tx 分开判断：只有一个回退时，另一个仍按差值算，不会把它开机以来的量再算一遍。
// boot_id 读不到（空串）时按同一次开机处理，靠回退识别重启 —— 否则每分钟都会把
// 开机以来的全部流量重复计入，几分钟就把限额刷满。
// 没有上一次采样（刚开始计量）时只建立基线，不计入 —— 否则会把开机以来
// 的全部历史流量一次性算进本周期。
// prevKey / key 是 trafficBootKey 的结果；计量网卡变了（例如默认路由换了网卡）时
// 两个计数器不是同一个东西，只重建基线、不计入。
func trafficDelta(prevRx, prevTx *int64, prevKey string, rx, tx int64, key string) (int64, int64) {
	if prevRx == nil || prevTx == nil {
		return 0, 0
	}
	prevBoot, prevIfaces := splitTrafficBootKey(prevKey)
	boot, ifaces := splitTrafficBootKey(key)
	if prevIfaces != "" && ifaces != "" && prevIfaces != ifaces {
		return 0, 0
	}
	sameBoot := boot == prevBoot || boot == "" || prevBoot == ""
	delta := func(prev, cur int64) int64 {
		if sameBoot && cur >= prev {
			return cur - prev
		}
		return cur
	}
	return delta(*prevRx, rx), delta(*prevTx, tx)
}

// trafficBootKey 把 boot_id 和参与计量的网卡拼成一个值存进 traffic_boot_id，
// 不用为网卡集合单独加列。旧数据只有 boot_id，没有网卡部分，视为「网卡未知」。
func trafficBootKey(boot string, ifaces []string) string {
	if len(ifaces) == 0 {
		return boot
	}
	sorted := append([]string(nil), ifaces...)
	sort.Strings(sorted)
	return boot + "#" + strings.Join(sorted, ",")
}

func splitTrafficBootKey(key string) (boot, ifaces string) {
	boot, ifaces, _ = strings.Cut(key, "#")
	return boot, ifaces
}

type nodeTrafficReading struct {
	RxBytes   int64    `json:"rx_bytes"`
	TxBytes   int64    `json:"tx_bytes"`
	BootID    string   `json:"boot_id"`
	Suspended bool     `json:"suspended"`
	Ifaces    []string `json:"interfaces"`
}

// StartNodeTrafficMeter 定时采集节点流量、执行限额，并低频刷新内核版本。
func (s *Server) StartNodeTrafficMeter(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(nodeTrafficInterval)
		defer ticker.Stop()
		var lastVersion time.Time
		var versionBusy atomic.Bool
		for {
			func() {
				defer func() {
					if r := recover(); r != nil && s.logger != nil {
						s.logger.Error("node traffic: 流量计量主循环发生异常", "panic", r, "stack", string(debug.Stack()))
					}
				}()
				s.meterNodeTraffic(ctx)
			}()
			// 2026-11-04：版本刷新放到独立 goroutine（同一时刻只跑一轮）。原来与计量串在同一个循环里：
			// 十来台连不上的节点各等 15 秒，这一轮计量就被推迟两三分钟 —— 超额节点晚这么久才停。
			if time.Since(lastVersion) >= nodeVersionInterval && versionBusy.CompareAndSwap(false, true) {
				lastVersion = time.Now()
				go func() {
					defer versionBusy.Store(false)
					defer func() {
						if r := recover(); r != nil && s.logger != nil {
							s.logger.Error("node traffic: 版本刷新协程发生异常", "panic", r, "stack", string(debug.Stack()))
						}
					}()
					s.refreshNodeVersions(ctx)
				}()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

type trafficNode struct {
	id, name   string
	limit      *int64
	resetDay   int
	cycleStart *time.Time
	cycleBytes int64
	prevRx     *int64
	prevTx     *int64
	prevBoot   string
	suspended  bool
}

const trafficNodeColumns = `id, name, traffic_limit_bytes, traffic_reset_day, traffic_cycle_start, traffic_cycle_bytes,
       traffic_counter_rx, traffic_counter_tx, COALESCE(traffic_boot_id,''), traffic_suspended`

func scanTrafficNode(row interface{ Scan(...any) error }, n *trafficNode) error {
	return row.Scan(&n.id, &n.name, &n.limit, &n.resetDay, &n.cycleStart, &n.cycleBytes,
		&n.prevRx, &n.prevTx, &n.prevBoot, &n.suspended)
}

// trafficMeterLocks 同一节点同时只跑一次计量：改限额触发的即时计量和每分钟的
// 循环会撞在一起，两边都基于旧基线算增量，traffic_samples 会重复计入那一段，
// 循环里的旧快照还可能把刚调高限额的节点再停一次。
var trafficMeterLocks sync.Map

// trafficMeterRequestWait 是 HTTP 请求等计量锁的上限。
var trafficMeterRequestWait = 10 * time.Second

// lockTrafficMeterForRequest 在请求里取某节点的计量锁：最多等 trafficMeterRequestWait，并随请求取消。
// 取不到时已回 409，调用方直接返回。
//
// 2026-11-06：原来直接 sync.Mutex.Lock()。每分钟的计量持锁期间要调门户（15 秒）、
// 停用 / 恢复节点（45 秒）：超额或不可达的节点上，改限额、换绑、解绑的请求会卡住近一分钟，
// 客户端断开后也一直等 —— 而「调高限额立即恢复」最需要的正是被停的那台。
// lockTrafficMeterCtx 与 lockTrafficMeterForRequest 相同，供后台任务（节点迁移）用：等到 ctx 结束为止。
func lockTrafficMeterCtx(ctx context.Context, nodeID string) (func(), error) {
	if norm, ok := canonicalUUID(nodeID); ok {
		nodeID = norm
	}
	v, _ := trafficMeterLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	return mu.Unlock, nil
}

// trafficBaselineResetSQL 换机器时一并清掉的计量基线（开机以来的网卡计数器与 boot_id 属于旧机器）。
const trafficBaselineResetSQL = "traffic_counter_rx=NULL, traffic_counter_tx=NULL, traffic_boot_id=NULL"

func lockTrafficMeterForRequest(w http.ResponseWriter, r *http.Request, nodeID string) (func(), bool) {
	if norm, ok := canonicalUUID(nodeID); ok {
		nodeID = norm
	}
	v, _ := trafficMeterLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	deadline := time.NewTimer(trafficMeterRequestWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !mu.TryLock() {
		select {
		case <-r.Context().Done():
			writeJSON(w, statusClientClosedRequest, map[string]string{"error": "client closed request"})
			return nil, false
		case <-deadline.C:
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点正在计量流量或切换停用状态，请稍后重试"})
			return nil, false
		case <-tick.C:
		}
	}
	return mu.Unlock, true
}

func (s *Server) meterNodeTraffic(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT id FROM hysteria_nodes WHERE deleted_at IS NULL AND api_key IS NOT NULL`)
	if err != nil {
		s.logger.Warn("node traffic: list nodes failed", "error", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.logger.Warn("node traffic: node inventory is incomplete; skipping round", "error", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("node traffic: node inventory ended incomplete; skipping round", "error", err)
		return
	}
	// 2026-11-04：有界并发计量。原来逐台串行，每台最多等 15 秒（停机调用另有 45 秒）：
	// 几台不可达的节点就让后面健康节点的超额判断晚好几分钟。同一节点由 trafficMeterLocks 串行，不会重复计入。
	sem := make(chan struct{}, nodeTrafficConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil && s.logger != nil {
					s.logger.Error("node traffic: 单节点计量协程发生异常", "node_id", id, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			s.meterNodeByID(ctx, id)
		}(id)
	}
	wg.Wait()
}

// nodeTrafficConcurrency 计量并发上限：与开户一致，避免同时对太多节点发请求。
const nodeTrafficConcurrency = provisioningConcurrency

// meterNodeByID 加锁后读最新的节点行再计量，保证基线和限额都是当前值。
func (s *Server) meterNodeByID(ctx context.Context, id string) {
	mu, _ := trafficMeterLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	var n trafficNode
	err := scanTrafficNode(s.db.QueryRow(ctx, `SELECT `+trafficNodeColumns+`
FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL AND api_key IS NOT NULL`, id), &n)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("node traffic: load node failed", "node_id", id, "error", err)
		}
		return
	}
	s.meterOneNode(ctx, n, time.Now())
}

func (s *Server) meterOneNode(ctx context.Context, n trafficNode, now time.Time) {
	callCtx, cancel := context.WithTimeout(ctx, nodeTrafficCallLimit)
	defer cancel()
	target, err := s.loadPortalTarget(callCtx, n.id)
	if err != nil {
		// 2026-11-03：原来直接 return，不写 traffic_error ⇒ 有月限额的节点悄悄停止计量、
		// 超额也不会被停用，而界面看起来一切正常（采不到必须让人看见）。
		s.logger.Warn("node traffic: load portal target failed", "node_id", n.id, "error", err)
		s.setTrafficError(ctx, n.id, "无法计量流量："+err.Error())
		return
	}
	env, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodGet, "/node/traffic", nil)
	if err != nil {
		reason := "读取节点流量失败: " + err.Error()
		if strings.Contains(strings.ToLower(err.Error()), "endpoint not found") {
			reason = "节点门户版本过低，不支持整机流量计量；请在节点页「批量更新门户」到 2.7 及以上"
		}
		s.setTrafficError(ctx, n.id, reason)
		return
	}
	var reading nodeTrafficReading
	if err := json.Unmarshal(env.Data, &reading); err != nil {
		s.setTrafficError(ctx, n.id, "节点流量返回格式无法解析")
		return
	}
	// 2026-11-12：计数为 0（或负数）的读数不当作有效样本，本轮不计量、不动基线。
	//
	// 门户在读不到默认路由网卡时（网络服务重启、DHCP 重绑的短暂窗口）不报错，而是回 ok 加 rx/tx 都是 0、
	// 网卡列表为空。原来这样的读数被当成正常样本：trafficDelta 按「计数回退」算出增量 0，基线随即被写成 0；
	// 一分钟后正常读数回来，开机以来的全部流量被一次计进本周期 —— 有月限额的节点当场超额，代理服务被面板停掉。
	// 能应答这次请求的节点，默认路由网卡的累计计数不可能是 0，所以 0 只能是「没读到」。
	// 不按网卡列表是否为空来判断：旧版门户不带该字段。
	if reading.RxBytes < 0 || reading.TxBytes < 0 || (reading.RxBytes == 0 && reading.TxBytes == 0) {
		s.setTrafficError(ctx, n.id, "节点返回的网卡计数为 0 或无效（可能暂时读不到默认路由网卡），本轮不计量")
		return
	}

	cs := trafficCycleStart(now, n.resetDay)
	cycleBytes := n.cycleBytes
	if n.cycleStart == nil || !n.cycleStart.Equal(cs) {
		cycleBytes = 0 // 新周期
	}
	bootKey := trafficBootKey(reading.BootID, reading.Ifaces)
	dRx, dTx := trafficDelta(n.prevRx, n.prevTx, n.prevBoot, reading.RxBytes, reading.TxBytes, bootKey)
	cycleBytes += dRx + dTx

	if _, err := s.db.Exec(ctx, `
UPDATE hysteria_nodes SET traffic_cycle_start=$2, traffic_cycle_bytes=$3,
       traffic_counter_rx=$4, traffic_counter_tx=$5, traffic_boot_id=$6,
       traffic_sampled_at=$7, traffic_error=''
WHERE id=$1::uuid`, n.id, cs, cycleBytes, reading.RxBytes, reading.TxBytes, bootKey, now); err != nil {
		s.logger.Warn("node traffic: persist failed", "node", n.name, "error", err)
		return
	}
	if dRx+dTx > 0 {
		if _, err := s.db.Exec(ctx, `
INSERT INTO traffic_samples (node_id, sampled_at, rx_bytes, tx_bytes, source)
VALUES ($1::uuid, $2, $3, $4, 'node_iface') ON CONFLICT DO NOTHING`, n.id, now, dRx, dTx); err != nil {
			s.logger.Warn("node traffic: sample insert failed", "node", n.name, "error", err)
		}
	}

	over := n.limit != nil && cycleBytes >= *n.limit
	switch {
	case over && (!n.suspended || !reading.Suspended):
		// 也覆盖「库里记着已停用，节点侧却在跑」（例如有人手动拉起了服务）。
		s.setNodeSuspension(ctx, n, target, true, cycleBytes)
	case !over && n.suspended:
		// 新周期、调高限额或取消限额后恢复。2026-11-06：只看**面板自己的**标记。
		// 原来条件里还有 `|| reading.Suspended`：运维在门户里手动停了 hy2（排查、变更、封禁），
		// 面板 60 秒内就给它 POST suspended:false 拉起来，还写一条 node_traffic_resume 审计 ——
		// 面板只应恢复它自己停掉的服务。
		s.setNodeSuspension(ctx, n, target, false, cycleBytes)
	case !over && !n.suspended && reading.Suspended:
		// 节点侧停着、面板没停过：只记录，不动手（是谁停的要人去查）。
		s.setTrafficError(ctx, n.id, "节点上的代理服务处于停用状态，但不是面板停用的（可能是手工操作）；面板不会自动恢复它")
	}
}

func (s *Server) setNodeSuspension(ctx context.Context, n trafficNode, target *portalTarget, suspend bool, cycleBytes int64) {
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	_, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodPost, "/node/suspend",
		map[string]any{"suspended": suspend})
	action := "恢复"
	if suspend {
		action = "停用"
	}
	if err != nil {
		s.logger.Error("node traffic: 超额"+action+"失败，下一轮重试", "node", n.name, "error", err)
		s.setTrafficError(ctx, n.id, fmt.Sprintf("流量超额%s节点服务失败: %s", action, err.Error()))
		return
	}
	if suspend {
		_, err = s.db.Exec(ctx, `UPDATE hysteria_nodes SET traffic_suspended=TRUE,
			traffic_suspended_at=COALESCE(traffic_suspended_at, now()) WHERE id=$1::uuid`, n.id)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE hysteria_nodes SET traffic_suspended=FALSE,
			traffic_suspended_at=NULL WHERE id=$1::uuid`, n.id)
	}
	if err != nil {
		s.logger.Error("node traffic: 写回停用状态失败", "node", n.name, "error", err)
		return
	}
	// 节点超流量停服或恢复后立即失效健康闸门与订阅缓存，防止继续向客户下发已停服节点。
	invalidateHealthGateCache()
	InvalidateSubscriptionCache()
	limit := int64(0)
	if n.limit != nil {
		limit = *n.limit
	}
	s.logger.Warn("node traffic: 节点服务已"+action, "node", n.name,
		"cycle_bytes", cycleBytes, "limit_bytes", limit)
	s.auditSystem(ctx, "node_traffic_"+map[bool]string{true: "suspend", false: "resume"}[suspend], n.id,
		map[string]any{"cycle_bytes": cycleBytes, "limit_bytes": limit})
}

func (s *Server) setTrafficError(ctx context.Context, nodeID, reason string) {
	if _, err := s.db.Exec(ctx, `UPDATE hysteria_nodes SET traffic_error=$2 WHERE id=$1::uuid AND traffic_error IS DISTINCT FROM $2`, nodeID, reason); err != nil {
		s.logger.Warn("node traffic: record error failed", "node_id", nodeID, "error", err)
	}
}

// auditSystem 记录没有操作者的系统动作（超额停用/恢复），方便事后追溯
// 「节点为什么停了」。user_id 为空即系统动作。
func (s *Server) auditSystem(ctx context.Context, action, nodeID string, details map[string]any) {
	encoded, _ := json.Marshal(details)
	if _, err := s.db.Exec(ctx, `
INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details)
VALUES (NULL, $1, 'hysteria_node', $2::uuid, $3::jsonb)`, action, nodeID, string(encoded)); err != nil {
		s.logger.Warn("audit(system) failed", "action", action, "error", err)
	}
}

// refreshNodeVersions 读每个节点的 Hysteria 内核版本，写进 hysteria_nodes.version。
// 版本只在网页会话通道（check-version）上有，所以只处理绑了 basic 凭据的节点。
func (s *Server) refreshNodeVersions(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT id, name FROM hysteria_nodes
		WHERE deleted_at IS NULL AND portal_basic_user IS NOT NULL AND portal_basic_password IS NOT NULL`)
	if err != nil {
		s.logger.Warn("node version: list failed", "error", err)
		return
	}
	type row struct{ id, name string }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			s.logger.Warn("node version: row scan failed", "error", err)
			return
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("node version: row iteration failed", "error", err)
		return
	}
	rows.Close()
	for _, r := range list {
		version, err := s.fetchNodeCoreVersion(ctx, r.id)
		if err != nil {
			s.logger.Info("node version: read failed", "node", r.name, "error", err)
			continue
		}
		if _, err := s.db.Exec(ctx, `UPDATE hysteria_nodes SET version=$2, version_checked_at=now() WHERE id=$1::uuid`, r.id, version); err != nil {
			s.logger.Warn("node version: persist failed", "node", r.name, "error", err)
		}
	}
}

func (s *Server) fetchNodeCoreVersion(ctx context.Context, nodeID string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, nodeTrafficCallLimit)
	defer cancel()
	target, err := s.loadPortalWebTarget(callCtx, nodeID)
	if err != nil {
		return "", err
	}
	env, err := portalWebCall(callCtx, target, http.MethodGet, "check-version", nil)
	if err != nil {
		return "", err
	}
	var body struct {
		CoreCurrent string `json:"core_current"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		return "", err
	}
	v := strings.TrimSpace(body.CoreCurrent)
	if v == "" || v == "未知" {
		return "", errors.New("节点未返回内核版本")
	}
	return v, nil
}

// updateNodeTrafficLimit 设置节点月度流量限额与重置日。
// limit_gb 为 null 或 0 表示不限额。改完立即对这个节点跑一次计量，
// 调高或取消限额时被停的服务马上恢复，不用等下一分钟。
func (s *Server) updateNodeTrafficLimit(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		LimitGB  *float64 `json:"limit_gb"`
		ResetDay int      `json:"reset_day"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if input.ResetDay < 1 || input.ResetDay > 31 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "重置日需为 1-31"})
		return
	}
	// 2026-11-04：limit_gb 只接受「null / 0 = 不限」与正数。原来负数被当成「不限」：
	// 写错一个符号就把超额停用中的节点悄悄放开；极小的正数（如 1e-10）截断成 0 字节，
	// 撞上 CHECK (traffic_limit_bytes > 0) 报 500。路径 id 先校验，免得畸形 id 在全局锁表里各占一项。
	var limit *int64
	if input.LimitGB != nil && *input.LimitGB < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "限额不能为负数（不限请传 0 或 null）"})
		return
	}
	if input.LimitGB != nil && *input.LimitGB > 0 {
		if *input.LimitGB > 1e6 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "限额过大"})
			return
		}
		v := int64(*input.LimitGB * (1 << 30))
		if v < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "限额过小"})
			return
		}
		limit = &v
	}
	nodeID := r.PathValue("id")
	if !uuidPattern.MatchString(nodeID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "节点 ID 格式不正确"})
		return
	}
	unlock, ok := lockTrafficMeterForRequest(w, r, nodeID)
	if !ok {
		return
	}
	defer unlock()
	var oldResetDay int
	err := s.db.QueryRow(r.Context(), `SELECT traffic_reset_day FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&oldResetDay)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	now := time.Now()
	// 只迁移仍属于当前周期的起点；离线很久的旧周期应由计量器正常清零。
	tag, err := s.db.Exec(r.Context(), `UPDATE hysteria_nodes SET traffic_limit_bytes=$2, traffic_reset_day=$3,
		traffic_cycle_start = CASE WHEN traffic_reset_day <> $3 AND traffic_cycle_start = $5
		                           THEN $4 ELSE traffic_cycle_start END,
		updated_at=now() WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID, limit, input.ResetDay,
		trafficCycleStart(now, input.ResetDay), trafficCycleStart(now, oldResetDay))
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	s.audit(r.Context(), r, "set_node_traffic_limit", "hysteria_node", nodeID,
		map[string]any{"limit_bytes": limit, "reset_day": input.ResetDay})
	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.meterNodeByID(ctx, id)
	}(nodeID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "limit_bytes": limit, "reset_day": input.ResetDay})
}
