package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"nodeops/server/internal/sshclient"
)

// 被墙检测（2026-10-15）。
//
// 面板部署在海外时，只能看到「海外能不能连上节点」；被墙是「国内连不上」，
// 从海外永远测不出来。公开的多地拨测服务（check-host 等）在大陆没有节点。
// 所以需要一台**在中国大陆的探测点**：任意一台你能 SSH 的国内机器（便宜的国内 VPS、
// 家里的软路由 / 树莓派都行），在 VPS 主机页登记，再在这里选它当探测点。
//
// 判据：从探测点对每个节点的 IP 做 TCP 建连（节点门户的 HTTPS 端口，真实的 TCP
// 监听）+ ICMP ping。两样都不通、而面板从海外看节点在线 → 疑似被墙（IP 级封锁）。
// 只测 IP 级封锁；按协议/端口的精准封锁（UDP 被 QoS）不在这个判据里。

const cnProbeSettingKey = "cn_probe_server_id"

type blockCheckItem struct {
	NodeID    string     `json:"node_id"`
	NodeName  string     `json:"node_name"`
	NodeState string     `json:"node_status"`
	TargetIP  string     `json:"target_ip"`
	TCPPort   *int       `json:"tcp_port"`
	TCPOK     *bool      `json:"tcp_ok"`
	PingOK    *bool      `json:"ping_ok"`
	Blocked   *bool      `json:"blocked"`
	Detail    string     `json:"detail"`
	CheckedAt *time.Time `json:"checked_at"`
}

var blockCheckMu sync.Mutex

func (s *Server) cnProbeServerID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, cnProbeSettingKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(id), nil
}

// GET /api/block-checks
func (s *Server) listBlockChecks(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	probe := map[string]any(nil)
	probeMissing := false
	id, err := s.cnProbeServerID(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if id != "" {
		var name, host string
		err := s.db.QueryRow(r.Context(), `SELECT name, host FROM servers WHERE id=$1::uuid AND deleted_at IS NULL`, id).Scan(&name, &host)
		// 2026-11-06：探测点主机被删时照常回 200（probe=null、probe_missing=true）。原来回 409，
		// 面板读不到数据就整块不渲染 —— 重新选探测点的下拉框正好在这块里，界面上没有任何出口。
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			probeMissing = true
		case err != nil:
			s.databaseError(w, r, err)
			return
		default:
			probe = map[string]any{"server_id": id, "name": name, "host": host}
		}
	}
	rows, err := s.db.Query(r.Context(), `
SELECT n.id::text, n.name, n.status, COALESCE(b.target_ip,''), b.tcp_port, b.tcp_ok, b.ping_ok, b.blocked, COALESCE(b.detail,''), b.checked_at
FROM hysteria_nodes n LEFT JOIN node_block_checks b ON b.node_id = n.id
WHERE n.deleted_at IS NULL ORDER BY n.name`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	items := make([]blockCheckItem, 0, 16)
	for rows.Next() {
		var it blockCheckItem
		if err := rows.Scan(&it.NodeID, &it.NodeName, &it.NodeState, &it.TargetIP, &it.TCPPort, &it.TCPOK, &it.PingOK, &it.Blocked, &it.Detail, &it.CheckedAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"probe": probe, "probe_missing": probeMissing, "items": items})
}

// PUT /api/settings/cn-probe  {"server_id": "..."}；空串 = 取消探测点。
func (s *Server) setCNProbe(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		ServerID string `json:"server_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	in.ServerID = strings.TrimSpace(in.ServerID)
	// 2026-11-04：ID 格式不对是 400；查询失败是 500。同时规范为小写标准 UUID，
	// 确保 blockedNodeConditionsSQL 中的 ps.id::text = ss.value 精确匹配。
	if in.ServerID != "" {
		norm, ok := canonicalUUID(in.ServerID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机 ID 格式不正确"})
			return
		}
		in.ServerID = norm
	}
	if in.ServerID != "" {
		var hasCred bool
		var nodes int
		err := s.db.QueryRow(r.Context(), `
SELECT EXISTS (SELECT 1 FROM server_credentials c WHERE c.server_id = s.id),
       (SELECT count(*) FROM hysteria_nodes n WHERE n.server_id = s.id AND n.deleted_at IS NULL)
FROM servers s WHERE s.id=$1::uuid AND s.deleted_at IS NULL`, in.ServerID).Scan(&hasCred, &nodes)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
			return
		}
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if !hasCred {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "探测点需要 SSH 凭据"})
			return
		}
	}
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '被墙检测的国内探测点（主机 id）')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, cnProbeSettingKey, in.ServerID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "set_cn_probe", "system_setting", cnProbeSettingKey, map[string]any{"server_id": in.ServerID})
	writeJSON(w, http.StatusOK, map[string]string{"server_id": in.ServerID})
}

// POST /api/block-checks/run
func (s *Server) runBlockChecksNow(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	n, err := s.runBlockChecks(ctx)
	if err != nil {
		// 2026-11-06：按错误类型回状态码。原来一律 502 + 原文：读配置、读节点的数据库错误
		// （带 pgx 原文）被说成「探测点网关错误」，「没设探测点」「上一轮还在跑」也是 502。
		var probeErr blockCheckProbeError
		var persistErr blockCheckPersistError
		switch {
		case errors.Is(err, errNoCNProbe), errors.Is(err, errBlockCheckRunning):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.As(err, &persistErr):
			// 探测跑完了但结论没落库：可重试，不能说成「已检测」。
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		case errors.As(err, &probeErr):
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			s.databaseError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), r, "run_block_checks", "hysteria_node", "", map[string]any{"nodes": n})
	writeJSON(w, http.StatusOK, map[string]any{"checked": n})
}

// blockCheckStartDelay：启动后多久跑第一轮。与监控调度器同样等 90 秒，
// 让面板先稳定下来；原来要等满 30 分钟才有第一轮，重启后这段时间被墙状态全靠旧数据。
var blockCheckStartDelay = 90 * time.Second

// StartBlockChecker 启动 90 秒后先跑一轮，之后每 30 分钟检测一次（配置了探测点才跑）。
func (s *Server) StartBlockChecker(ctx context.Context) {
	go func() {
		round := func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("block check panic", "panic", r)
				}
			}()
			runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if _, err := s.runBlockChecks(runCtx); err != nil && !errors.Is(err, errNoCNProbe) {
				s.logger.Warn("block check failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(blockCheckStartDelay):
		}
		round()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				round()
			}
		}
	}()
}

var errBlockCheckRunning = errors.New("上一轮检测还在进行")

// blockCheckProbeError 是探测点本身（SSH、远端命令、坏的 portal 地址）的失败 —— 只有这类回 502。
type blockCheckProbeError struct{ msg string }

func (e blockCheckProbeError) Error() string { return e.msg }

var errNoCNProbe = errors.New("还没有设置国内探测点：在「被墙检测」里选一台位于中国大陆、已配 SSH 凭据的主机")

type blockTarget struct {
	nodeID string
	ip     string
	port   int
}

func (s *Server) runBlockChecks(ctx context.Context) (int, error) {
	probeID, err := s.cnProbeServerID(ctx)
	if err != nil {
		return 0, fmt.Errorf("读取国内探测点配置失败：%w", err)
	}
	if probeID == "" {
		return 0, errNoCNProbe
	}
	if !blockCheckMu.TryLock() {
		return 0, errBlockCheckRunning
	}
	defer blockCheckMu.Unlock()

	rows, err := s.db.Query(ctx, `
SELECT n.id::text, s.host, COALESCE(n.api_url,'')
FROM hysteria_nodes n JOIN servers s ON s.id = n.server_id
WHERE n.deleted_at IS NULL AND n.server_id <> $1::uuid`, probeID)
	if err != nil {
		return 0, err
	}
	var targets []blockTarget
	var unprobeable []blockTarget
	for rows.Next() {
		var t blockTarget
		var apiURL string
		if err := rows.Scan(&t.nodeID, &t.ip, &apiURL); err != nil {
			rows.Close()
			return 0, err
		}
		// 2026-11-03：主机地址不是 IP（登记成了域名）时原来直接跳过、什么都不写，
		// 页面上这台节点永远停在上一次的结论（或「尚未检测」），看不出是没测。
		// 不在这里替它解析域名：国内探测点要测的是这台机器的 IP，本地解析结果可能被污染。
		if net.ParseIP(t.ip) == nil {
			unprobeable = append(unprobeable, t)
			continue
		}
		t.port = 443
		// 2026-11-03：解绑节点 API 会把在役节点的 api_url 置 NULL（COALESCE 成 ''）。
		// 原来空串也走下面的「非法 URL」分支，整轮检测直接失败 —— 只因为一台节点没绑 API，
		// 所有节点都不再测，而已判被墙的节点因「数据过期保持原判」被永久挡在订阅外。
		// 没有 portal 地址就按默认 443 探测；填了但格式不对仍然整轮报错（那是坏数据）。
		if strings.TrimSpace(apiURL) == "" {
			targets = append(targets, t)
			continue
		}
		u, err := url.Parse(apiURL)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			rows.Close()
			return 0, blockCheckProbeError{fmt.Sprintf("节点 %s 的节点 API 地址格式不对，无法确定检测端口", shortID(t.nodeID))}
		}
		if u.Port() != "" {
			port, parseErr := strconv.Atoi(u.Port())
			if parseErr != nil || port < 1 || port > 65535 {
				rows.Close()
				return 0, blockCheckProbeError{fmt.Sprintf("节点 %s 的节点 API 端口不合法，无法确定检测端口", shortID(t.nodeID))}
			}
			t.port = port
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	unprobeablePersistFailed := 0
	for _, t := range unprobeable {
		if _, err := s.db.Exec(ctx, `
INSERT INTO node_block_checks (node_id, probe_server_id, target_ip, tcp_port, tcp_ok, ping_ok, blocked, detail, checked_at)
VALUES ($1::uuid, $2::uuid, $3, NULL, NULL, NULL, NULL, $4, now())
ON CONFLICT (node_id) DO UPDATE SET probe_server_id=EXCLUDED.probe_server_id, target_ip=EXCLUDED.target_ip,
  tcp_port=NULL, tcp_ok=NULL, ping_ok=NULL, blocked=NULL, detail=EXCLUDED.detail, checked_at=now()`,
			t.nodeID, probeID, t.ip, "主机地址不是 IP，无法从国内探测点检测；请在主机页把地址改成公网 IP"); err != nil {
			s.logger.Warn("block check: persist skip reason failed", "node", t.nodeID, "error", err)
			unprobeablePersistFailed++
		}
	}
	if len(targets) == 0 {
		if unprobeablePersistFailed > 0 {
			return 0, blockCheckPersistError{unprobeablePersistFailed}
		}
		return 0, nil
	}

	client, _, err := s.dialServer(ctx, probeID)
	if err != nil {
		return 0, blockCheckProbeError{"连接国内探测点失败：" + err.Error()}
	}
	defer client.Close()

	// 所有目标并发探测，单个 TCP 4 秒、ping 2 包，整体约 10 秒。
	// IP 与端口都来自面板自己的库并已校验为合法 IP / 整数，仍使用 shellQuote 安全转义脚本整体。
	var script strings.Builder
	script.WriteString("probe(){ t=no; timeout 4 bash -c \"</dev/tcp/$2/$3\" 2>/dev/null && t=yes; p=no; ping -c 2 -W 2 -q \"$2\" >/dev/null 2>&1 && p=yes; echo \"$1 $t $p\"; }\n")
	for _, t := range targets {
		fmt.Fprintf(&script, "probe %s %s %d &\n", t.nodeID, t.ip, t.port)
	}
	script.WriteString("wait\n")
	out, stderr, code, err := sshclient.Run(ctx, client, "bash -c "+shellQuote(script.String()))
	if err != nil || code != 0 {
		return 0, blockCheckProbeError{"探测点执行失败：" + firstNonEmpty(strings.TrimSpace(stderr), fmt.Sprint(err))}
	}
	results := map[string][2]bool{}
	for _, line := range strings.Split(sshclient.CleanCommandOutput(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 {
			results[f[0]] = [2]bool{f[1] == "yes", f[2] == "yes"}
		}
	}
	targetPersistFailed := 0
	for _, t := range targets {
		r, ok := results[t.nodeID]
		var tcpOK, pingOK, blocked *bool
		detail := "探测点没有返回结果"
		if ok {
			tcpOK, pingOK = &r[0], &r[1]
			b := !r[0] && !r[1]
			blocked = &b
			switch {
			case b:
				detail = "国内 TCP 与 ping 都不通，疑似 IP 被封"
			case !r[0]:
				detail = "ping 通但 TCP 不通，可能是端口被封或防火墙"
			case !r[1]:
				detail = "TCP 通（ping 被禁用很常见，不影响使用）"
			default:
				detail = "国内可达"
			}
		}
		if _, err := s.db.Exec(ctx, `
INSERT INTO node_block_checks (node_id, probe_server_id, target_ip, tcp_port, tcp_ok, ping_ok, blocked, detail, checked_at)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (node_id) DO UPDATE SET probe_server_id=EXCLUDED.probe_server_id, target_ip=EXCLUDED.target_ip,
  tcp_port=EXCLUDED.tcp_port, tcp_ok=EXCLUDED.tcp_ok, ping_ok=EXCLUDED.ping_ok, blocked=EXCLUDED.blocked,
  detail=EXCLUDED.detail, checked_at=now()`, t.nodeID, probeID, t.ip, t.port, tcpOK, pingOK, blocked, detail); err != nil {
			// 写不进去代表该节点结论未落地（页面与告警仍停在旧数据）。
			s.logger.Warn("block check: persist failed", "node", t.nodeID, "error", err)
			targetPersistFailed++
		}
	}
	totalPersistFailed := unprobeablePersistFailed + targetPersistFailed
	if totalPersistFailed > 0 {
		// 仅扣除 targets 自身的落库失败数，防止 unprobeable 落库失败把已检测数量扣成负数。
		return len(targets) - targetPersistFailed, blockCheckPersistError{totalPersistFailed}
	}
	return len(targets), nil
}

// blockCheckPersistError：探测跑完了，但结论没能写库（数据库问题），不能算「已检测」。
type blockCheckPersistError struct{ failed int }

func (e blockCheckPersistError) Error() string {
	return fmt.Sprintf("有 %d 台节点的检测结果未能写入数据库，页面与告警仍是旧数据，请稍后重试", e.failed)
}

// blockedNodeConditions：最近 1 小时内判定被墙、且面板从海外看节点仍在线的节点。
// 节点本身离线时不报「被墙」—— 那是另一种故障，已有 node_offline 告警。
//
// 数据过期时保持原判（2026-10-23）：检测每 30 分钟一轮，只要探测点连不上、
// 面板重启后还没跑、或者某轮探测返回 NULL，原来就会因「1 小时内没有 blocked 记录」
// 把被墙告警自动恢复，节点随之回到客户订阅 —— 实际上它依旧被墙，只是没测。
// 现在：没有新鲜结论（1 小时内没测过，或最近一次结果为 NULL）且已有未恢复的
// node_blocked 告警时维持告警，直到新鲜探测明确证明恢复。无数据不能证明已恢复。
func (s *Server) blockedNodeConditions(ctx context.Context) ([]alertCondition, error) {
	rows, err := s.db.Query(ctx, blockedNodeConditionsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertCondition
	for rows.Next() {
		var sid, nid string
		if err := rows.Scan(&sid, &nid); err != nil {
			return nil, err
		}
		out = append(out, alertCondition{alertKey{"node_blocked", sid, nid}, "critical"})
	}
	return out, rows.Err()
}

const blockedNodeConditionsSQL = `
SELECT n.server_id::text, n.id::text FROM hysteria_nodes n
LEFT JOIN node_block_checks b ON b.node_id = n.id
WHERE n.deleted_at IS NULL AND (
    (n.status = 'online' AND b.blocked AND b.checked_at > now() - interval '1 hour')
    OR ((b.blocked IS NULL OR b.checked_at IS NULL OR b.checked_at <= now() - interval '1 hour')
        AND EXISTS (SELECT 1 FROM alerts a WHERE a.node_id = n.id AND a.type = 'node_blocked' AND a.status <> 'resolved')
        -- 2026-11-06：「数据过期就维持原判」只在探测点仍在时成立。取消探测点或删掉探测点主机后，
        -- 再也不会有新数据：原来这条分支让已判被墙的节点永久挂着告警、永久被挡在买家订阅外。
        AND EXISTS (SELECT 1 FROM system_settings ss JOIN servers ps ON ps.id::text = ss.value AND ps.deleted_at IS NULL
                    WHERE ss.key = 'cn_probe_server_id')))`
