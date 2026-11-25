package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"nodeops/server/internal/sshclient"
)

// 本文件（2026-10-16）：
//   1. 故障节点自动从订阅下线
//   9. SSH 主机指纹变化后的确认与重新登记

// ---------------------------------------------------------------- 1. 自动下线

const autoHideSettingKey = "auto_hide_unhealthy_nodes"

// autoHideAfter：告警持续多久才下线。太短会因为一次探测抖动就把节点从客户订阅里拿掉。
const autoHideAfter = 5 * time.Minute

// autoHideServerAfter：主机级告警（整机失联 / 采集停滞）持续多久才把它的节点下线。
//
// 为什么主机级要单独一档（2026-10-24 加）：主机级故障与节点级故障的确定性不同。
// 节点端口不通可能是探测抖动（也可能真挂了），需要 5 分钟的观察；
// 而**整机 SSH 完全连不上**几乎只有两种结局：机器宕了，或已退租 ——
// 它不会在 3 分钟内自己恢复。
//
// 为什么原来漏了主机级（2026-10-23 美国-阿什本实测）：
// queryUnhealthyNodes 原来只查 node_offline / node_blocked，
// 且 JOIN 时带 `n.deleted_at IS NULL` —— 节点已软删时，
// 连「主机还活着吗」这件事都无人问津。整机宕机 4 小时无人知晓。
const autoHideServerAfter = 3 * time.Minute

type hiddenNode struct {
	NodeID string    `json:"node_id"`
	Name   string    `json:"name"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

var healthGateCache struct {
	sync.Mutex
	at         time.Time
	nodes      map[string]hiddenNode
	loading    chan struct{}
	generation uint64
}

// 缓存自动下线开关，减少订阅渲染查询；setAutoHide 保存后主动失效。
var autoHideCache struct {
	sync.Mutex
	at         time.Time
	enabled    bool
	valid      bool // 是否有可回退的值；失效只清时间戳，保留快照
	generation uint64
}

const autoHideCacheTTL = 30 * time.Second

// 与 healthGateQuery 一样，允许测试注入实际查询，不依赖数据库。
var autoHideQuery = (*Server).queryAutoHideEnabled

func (s *Server) queryAutoHideEnabled(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, autoHideSettingKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // 未配置时默认开启；查询故障不能当作未配置
	}
	return strings.TrimSpace(v) != "0", err
}

func (s *Server) autoHideEnabled(ctx context.Context) bool {
	autoHideCache.Lock()
	if autoHideCache.valid && time.Since(autoHideCache.at) < autoHideCacheTTL {
		v := autoHideCache.enabled
		autoHideCache.Unlock()
		return v
	}
	generation := autoHideCache.generation
	autoHideCache.Unlock()

	// 共享缓存的刷新不随某个买家断开而取消，但仍受独立的短超时约束。
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthGateQueryTimeout)
	defer cancel()
	enabled, err := autoHideQuery(s, qctx)

	autoHideCache.Lock()
	if err != nil {
		enabled = true // 从未读到值时才回退默认开启
		if autoHideCache.valid {
			enabled = autoHideCache.enabled // 包括并发刷新刚读到的已知值
		}
	}
	if autoHideCache.generation == generation {
		// 失败也刷新时间戳，避免数据库故障时每个请求都等一次超时。
		autoHideCache.at, autoHideCache.enabled, autoHideCache.valid = time.Now(), enabled, true
	}
	autoHideCache.Unlock()
	return enabled
}

// invalidateAutoHideCache：管理员改了开关后立刻生效，不必等 TTL。
func invalidateAutoHideCache() {
	autoHideCache.Lock()
	autoHideCache.at = time.Time{}
	autoHideCache.generation++ // 失效前启动的读取不得回填旧值
	autoHideCache.Unlock()
}

// autoHideRecoverHold：离线或被墙告警恢复后，节点还要再保持下线这么久才回到订阅。
// 原来告警一恢复就立刻放回 —— 端口抖动（恢复 30 秒又挂）会让客户订阅里的节点反复出现、消失。
const autoHideRecoverHold = 10 * time.Minute

// healthGateQueryTimeout：健康闸门查询的上限。订阅拉取在等它，宁可这一轮用旧缓存也不挂住请求。
const healthGateQueryTimeout = 3 * time.Second

// healthGateQuery 是真正查库的那一步；是变量以便测试注入（不依赖数据库）。
var healthGateQuery = (*Server).queryUnhealthyNodes

// unhealthyNodes 返回当前应从订阅中隐藏的节点：
// node_offline（节点端口不通）或 node_blocked（国内连不上）的告警持续超过 5 分钟、且尚未恢复；
// 这两类告警恢复后还要再观察 autoHideRecoverHold 才放回。
// 结果缓存 30 秒：订阅拉取很频繁，不必每次都查告警表。
//
// 锁只护缓存本身，查库在锁外做（2026-10-23）：原来整段查询都持锁、且用请求的 ctx ——
// 一个慢查询会让所有订阅拉取排队，而某个客户端断开（ctx 被取消）又会让这一轮查询失败、
// 把空结果写进缓存，30 秒内所有故障节点都被放回订阅。现在查询用独立的短超时 ctx，
// 失败时沿用上一份缓存。
func (s *Server) unhealthyNodes(ctx context.Context) map[string]hiddenNode {
	healthGateCache.Lock()
	if time.Since(healthGateCache.at) < 30*time.Second && healthGateCache.nodes != nil {
		nodes := healthGateCache.nodes
		healthGateCache.Unlock()
		return nodes
	}
	prev := healthGateCache.nodes
	if loading := healthGateCache.loading; loading != nil {
		healthGateCache.Unlock()
		if prev != nil {
			return prev // 刷新期间直接使用旧快照，不排队等待数据库
		}
		select {
		case <-ctx.Done():
			return map[string]hiddenNode{}
		case <-loading:
			return s.unhealthyNodes(ctx)
		}
	}
	loading := make(chan struct{})
	generation := healthGateCache.generation
	healthGateCache.loading = loading
	healthGateCache.Unlock()
	defer func() {
		healthGateCache.Lock()
		healthGateCache.loading = nil
		close(loading)
		healthGateCache.Unlock()
	}()

	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthGateQueryTimeout)
	defer cancel()
	out, err := healthGateQuery(s, qctx)
	if err != nil {
		s.logger.Warn("health gate: query failed", "error", err)
		if prev != nil {
			out = prev // 沿用上一份结果：查询失败不该让已下线的故障节点突然回到订阅
		} else {
			out = map[string]hiddenNode{} // 从没成功过：不下线任何节点（宁可多发也不误删）
		}
	}
	healthGateCache.Lock()
	// 失败也刷新时间戳：否则数据库不可用时每次订阅拉取都会再等一次超时。
	if healthGateCache.generation == generation {
		healthGateCache.at, healthGateCache.nodes = time.Now(), out
	}
	healthGateCache.Unlock()
	return out
}

func (s *Server) queryUnhealthyNodes(ctx context.Context) (map[string]hiddenNode, error) {
	out := map[string]hiddenNode{}
	if s.db == nil {
		return out, nil
	}
	// 第二个分支是「恢复观察期」：故障已恢复但不足 autoHideRecoverHold，
	// 且当初确实持续够 autoHideAfter（真被下线过）—— 只抖了一下的从没下线过，也就无需观察。
	//
	// ⚠️ 这里有两个容易漏的点（2026-10-24 补）：
	//
	// 1. **主机级告警也要纳入**。原来只查 node_offline / node_blocked，
	//    而「整机失联」（server_offline）这类告警同样意味着
	//    这台主机上的节点全都连不上 —— 却完全不影响订阅输出。
	//    2026-10-23 美国-阿什本实测：主机宕机数小时，
	//    订阅里仍把它当作可用节点下发给客户。
	//
	// 2. **JOIN 条件不能把主机级告警过滤掉**。主机级告警的 node_id 为空，
	//    它要通过 server_id 关联到该主机下的**未删除**节点。
	//    原来 JOIN hysteria_nodes n ON n.id = a.node_id AND n.deleted_at IS NULL
	//    对主机级告警根本匹配不上（a.node_id 是 NULL），
	//    所以主机级告警必须走 server_id 这一支。
	rows, err := s.db.Query(ctx, `
WITH bad AS (
  -- 节点级：节点自己的端口不通 / 疑似被墙
  SELECT a.node_id::text AS node_id, n.name AS node_name, a.type AS kind,
         a.first_seen_at, a.status, a.resolved_at, $1::float AS grace
  FROM alerts a
  JOIN hysteria_nodes n ON n.id = a.node_id AND n.deleted_at IS NULL
  WHERE a.type IN ('node_offline', 'node_blocked')

  UNION ALL

  -- 主机级：整机失联 / 采集停滞 —— 这台主机上的所有在役节点都不可用
  SELECT n.id::text AS node_id, n.name AS node_name, a.type AS kind,
         a.first_seen_at, a.status, a.resolved_at, $3::float AS grace
  FROM alerts a
  JOIN hysteria_nodes n ON n.server_id = a.server_id AND n.deleted_at IS NULL
  WHERE a.type IN ('server_offline', 'server_stale')
    AND a.node_id IS NULL

  UNION ALL

  -- 2026-11-06：流量超额被停用的节点（代理服务停了，门户还在应答）。只接 API 的节点探测只看门户，
  -- 一直是 online、不会有 node_offline：买家在周期重置前一直拿到一个连不上的节点。不需要宽限期。
  SELECT n.id::text AS node_id, n.name AS node_name, 'traffic_suspended' AS kind,
         COALESCE(n.traffic_suspended_at, now() - interval '1 day') AS first_seen_at,
         'open' AS status, NULL::timestamptz AS resolved_at, 0::float AS grace
  FROM hysteria_nodes n
  WHERE n.traffic_suspended AND n.deleted_at IS NULL
)
SELECT node_id, node_name, kind, MIN(first_seen_at), bool_or(status <> 'resolved')
FROM bad
WHERE first_seen_at < now() - make_interval(secs => grace)
  AND (status <> 'resolved'
       OR (resolved_at > now() - make_interval(secs => $2)
           AND resolved_at - first_seen_at >= make_interval(secs => grace)))
GROUP BY node_id, node_name, kind`,
		autoHideAfter.Seconds(), autoHideRecoverHold.Seconds(), autoHideServerAfter.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h hiddenNode
		var kind string
		var active bool
		if err := rows.Scan(&h.NodeID, &h.Name, &kind, &h.Since, &active); err != nil {
			return nil, err
		}
		reason := map[string]string{
			"node_offline":      "节点端口不通",
			"node_blocked":      "国内探测疑似被墙",
			"server_offline":    "所在主机整机不可达",
			"server_stale":      "所在主机采集停滞",
			"traffic_suspended": "流量超额，节点服务已停用",
		}[kind]
		if reason == "" {
			reason = kind
		}
		if !active {
			reason += "（已恢复，观察中）"
		}
		if prev, ok := out[h.NodeID]; ok {
			reason = prev.Reason + "、" + reason
			if prev.Since.Before(h.Since) {
				h.Since = prev.Since
			}
		}
		h.Reason = reason
		out[h.NodeID] = h
	}
	return out, rows.Err()
}

// filterHealthyNodes 在 subscriptionNodesFor 里调用。
// 安全阀：如果过滤后一个节点都不剩，就原样返回 —— 订阅里没有节点比有一个坏节点更糟，
// 而且「全部节点同时故障」更可能是探测侧（面板网络）出了问题。
func (s *Server) filterHealthyNodes(ctx context.Context, nodes []subscriptionNode) []subscriptionNode {
	if len(nodes) == 0 || !s.autoHideEnabled(ctx) {
		return nodes
	}
	bad := s.unhealthyNodes(ctx)
	if len(bad) == 0 {
		return nodes
	}
	kept := make([]subscriptionNode, 0, len(nodes))
	for _, n := range nodes {
		if _, hide := bad[n.ID]; !hide {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		return nodes
	}
	return kept
}

// invalidateHealthGateCache 立即失效故障节点下线缓存，确保节点删除、迁移、超流量停用或恢复即刻反映到订阅输出。
func invalidateHealthGateCache() {
	healthGateCache.Lock()
	healthGateCache.at = time.Time{}
	healthGateCache.generation++
	healthGateCache.Unlock()
}

// GET /api/nodes/auto-hide
func (s *Server) getAutoHide(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		claims, _ := claimsFrom(r.Context())
		if !s.enforceReadLimit(w, claims.Subject) {
			return
		}
	}
	enabled := s.autoHideEnabled(r.Context())
	hidden := []hiddenNode{}
	if enabled {
		for _, h := range s.unhealthyNodes(r.Context()) {
			hidden = append(hidden, h)
		}
	}
	// after_minutes 报**节点级**门槛（原来只有这一档）。
	// 主机级门槛单独报出来 —— 2026-10-24 起主机级是 3 分钟、节点级是 5 分钟，
	// 只报一个数会让前端/运维以为所有故障都等 5 分钟。
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":              enabled,
		"after_minutes":        int(autoHideAfter.Minutes()),
		"server_after_minutes": int(autoHideServerAfter.Minutes()),
		"hidden":               hidden,
	})
}

// PUT /api/nodes/auto-hide  {"enabled": true}
func (s *Server) setAutoHide(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	v := "1"
	if !in.Enabled {
		v = "0"
	}
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '故障节点自动从订阅下线（1 开 / 0 关）')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, autoHideSettingKey, v); err != nil {
		s.databaseError(w, r, err)
		return
	}
	invalidateHealthGateCache()
	invalidateAutoHideCache()
	s.audit(r.Context(), r, "set_auto_hide_nodes", "system_setting", autoHideSettingKey, map[string]any{"enabled": in.Enabled})
	s.getAutoHide(w, r)
}

// ---------------------------------------------------------------- 9. 指纹变化

// fingerprintMismatch 从 SSH 错误里取出新旧指纹（不是指纹问题返回 nil）。
func fingerprintMismatch(err error) *sshclient.HostKeyMismatchError {
	var m *sshclient.HostKeyMismatchError
	if errors.As(err, &m) {
		return m
	}
	return nil
}

// POST /api/servers/{id}/fingerprint/accept  {"expected_old": "...", "new": "..."}
//
// 只接受操作者在页面上**亲眼看到**的那个新指纹，并且只在库里仍是旧指纹时才替换
// （防止两个人同时操作，或指纹在确认期间又变了）。永不自动接受。
func (s *Server) acceptServerFingerprint(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		ExpectedOld string `json:"expected_old"`
		New         string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少新旧指纹"})
		return
	}
	in.ExpectedOld = strings.TrimSpace(in.ExpectedOld)
	in.New = strings.TrimSpace(in.New)
	if in.ExpectedOld == "" || !strings.HasPrefix(in.New, "SHA256:") || len(in.New) <= len("SHA256:") ||
		strings.ContainsAny(in.New, " \t\r\n") || controlChars.MatchString(in.New) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新旧指纹格式不正确"})
		return
	}
	id := r.PathValue("id")
	tag, err := s.db.Exec(r.Context(), `UPDATE servers SET host_fingerprint=$3, updated_at=now() WHERE id=$1::uuid AND host_fingerprint=$2 AND deleted_at IS NULL`, id, in.ExpectedOld, in.New)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "登记的指纹已经变化，请重新测试连接后再确认"})
		return
	}
	s.audit(r.Context(), r, "accept_server_fingerprint", "server", id, map[string]any{"old": in.ExpectedOld, "new": in.New})
	writeJSON(w, http.StatusOK, map[string]string{"fingerprint": in.New})
}
