package httpapi

// ippurity.go — 节点 IP 纯净度接口（2026-10-15）。
//
// 三个端点：
//   GET  /api/nodes/{id}/ip-purity          读缓存（不联网，快）
//   POST /api/nodes/{id}/ip-purity/refresh  重新检测（可带 force_api=1 走深度检测）
//   POST /api/ip-purity/refresh-all         批量检测所有可检测节点（管理员）
//
// 设计要点
// --------
//  1. **读缓存不联网**：节点列表页会对每个节点拉一次纯净度，如果每次都打
//     第三方 API，一次刷屏几十个节点就把免费配额刷爆。所以 GET 只读库，
//     检测是显式动作（refresh）。这与项目"读操作宽、写操作严"的分桶一致：
//     GET 归 readLimiter，refresh 归 writeLimiter。
//
//  2. **检测失败不清空旧结果**：接口失败时只更新 error 列，checked_at 与
//     判定字段保持上次成功的值。这样界面能同时说"上次成功是 3 天前"和
//     "上次尝试失败因为限速"，而不是把一次抖动变成"这台机器没有数据"。
//
//  3. **检测对象是节点自报的 public_ip，退到 servers.host**：与 geo 同源，
//     都来自节点本机网卡读数，比用户手填更可信。
//
//  4. 被检测 IP 存在库里（ip_purity 的 ip 列），IP 变了界面据此提示重测。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"nodeops/server/internal/ippurity"

	"github.com/jackc/pgx/v5"
)

// purityCheckTimeout 限制单次第三方查询。geo 是 8s，这里给 10s ——
// ipapi.is 偶尔慢，而这是显式动作（用户点了按钮在等），可以多等一会。
const purityCheckTimeout = 12 * time.Second

// nodeIPForPurity 取一个节点该被检测的 IP。
//
// 优先级：servers.host 若是公网 IP 字面量就用它（最稳，不依赖节点在线）；
// 否则留空由调用方提示"该节点没有可检测的公网 IP"。
// 不解析域名 —— 与 geo 同理，把域名反解成 IP 得到的是 CDN 边缘节点，
// 不是这台机器真实所在，比不检测更误导。
func (s *Server) nodeIPForPurity(ctx context.Context, nodeID string) (string, error) {
	var host string
	err := s.db.QueryRow(ctx, `
SELECT s.host
FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id
WHERE n.id = $1::uuid AND n.deleted_at IS NULL`, nodeID).Scan(&host)
	if err != nil {
		return "", err
	}
	host = strings.TrimSpace(host)
	if ippurity.IsPublicIP(host) {
		return host, nil
	}
	return "", nil
}

// ipPurityRow 是数据库里的一行纯净度记录。
type ipPurityRow struct {
	IP             string          `json:"ip"`
	ConnectionType string          `json:"connection_type"`
	DualISP        *bool           `json:"is_dual_isp"`
	FraudScore     *int            `json:"fraud_score"`
	Proxy          *bool           `json:"is_proxy"`
	VPN            *bool           `json:"is_vpn"`
	Abuser         *bool           `json:"is_abuser"`
	ASN            string          `json:"asn"`
	ASNOrg         string          `json:"asn_org"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	Source         string          `json:"source"`
	Error          string          `json:"error,omitempty"`
	CheckedAt      *time.Time      `json:"checked_at"`
	// 原生 IP：RegCountry 注册国（RDAP）、GeoCountry 所在国；Native nil=未判定。
	// ScoreSource：评分来源 ipqs（单 IP 欺诈分）/ ipapi（ASN 滥用比例）/ 空。
	ScoreSource string `json:"score_source"`
	RegCountry  string `json:"reg_country"`
	RegRegistry string `json:"reg_registry"`
	GeoCountry  string `json:"geo_country"`
	Native      *bool  `json:"is_native"`
	// Stale 表示"存下来的 IP 与当前节点 IP 不一致"，界面据此提示重测。
	Stale bool `json:"stale"`
}

// nodeIPPurity 读一个节点的纯净度缓存（不联网）。
func (s *Server) nodeIPPurity(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")

	// 节点是否存在（含 deleted_at 过滤），顺带拿出当前 IP 做 stale 判定。
	currentIP, err := s.nodeIPForPurity(r.Context(), nodeID)
	if err != nil {
		// 查不到节点：区分"真没有"与"库错误"。
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}

	row, found, err := s.loadIPPurity(r.Context(), nodeID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if !found {
		// 尚未检测过：明确区分"从未检测"与"检测了没结果"。
		writeJSON(w, http.StatusOK, map[string]any{
			"checked": false,
			"ip":      currentIP,
			"reason":  map[bool]string{true: "", false: "该节点没有可检测的公网 IP"}[currentIP != ""],
		})
		return
	}
	// IP 变更或当前 IP 无法确定时不算 stale（无法确定就不该吓唬用户）。
	row.Stale = row.IP != "" && currentIP != "" && row.IP != currentIP
	writeJSON(w, http.StatusOK, map[string]any{"checked": true, "purity": row})
}

// refreshNodeIPPurity 对一个节点重新检测。
func (s *Server) refreshNodeIPPurity(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	if !uuidPattern.MatchString(nodeID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	// force_api=1 时即使离线库已判出连接类型也走 API 拿评分（界面「深度检测」）。
	forceAPI := r.URL.Query().Get("force_api") == "1"

	row, err := s.checkAndStorePurity(r.Context(), nodeID, forceAPI)
	if err != nil {
		// 2026-11-03：原来一律 502 + 原文，「节点不存在」也是 502、数据库错误原样外泄。
		// 只有检测本身失败（上游/配置）才算 502，且那类错误已是面向操作者的文案。
		switch {
		case errors.Is(err, errPurityNodeNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case row.IP == "" && !errors.Is(err, errPurityDisabled):
			s.databaseError(w, r, err)
		default:
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return
	}
	s.audit(r.Context(), r, "refresh_node_ip_purity", "hysteria_node", nodeID,
		map[string]any{"ip": row.IP, "source": row.Source, "score": row.FraudScore})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "purity": row})
}

// refreshAllIPPurity 批量检测所有能拿到公网 IP 的节点（管理员）。
//
// 返回一个任务式的摘要：成功几个、跳过几个（没 IP）、失败几个。
// 不做异步 —— 节点数有上限（几十台），且每台一次第三方查询，串行跑完
// 在免费配额内也够快；异步反而要再引入任务表与轮询。
func (s *Server) refreshAllIPPurity(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	// 2026-11-04：同一时刻只跑一轮批量检测。每台节点都要调一次第三方接口（消耗付费配额）：
	// 页面按钮只防同一个标签页，另一个标签页、Telegram 或重发的请求会再起一整轮，配额翻倍消耗。
	if !refreshAllPurityRunning.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "批量检测正在进行，完成后再试"})
		return
	}
	defer refreshAllPurityRunning.Store(false)
	rows, err := s.db.Query(r.Context(), `
SELECT n.id
FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id
WHERE n.deleted_at IS NULL`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	rows.Close()

	ok, skipped, failed := 0, 0, 0
	var errorsOut []string
	for _, id := range ids {
		if r.Context().Err() != nil {
			break
		}
		row, err := s.checkAndStorePurity(r.Context(), id, false)
		switch {
		case err == nil && row.IP == "":
			skipped++
		case err == nil:
			ok++
		default:
			failed++
			errorsOut = append(errorsOut, id+": "+err.Error())
			// 单个失败继续下一个 —— 一台机器的问题不该中断整批。
		}
	}
	s.audit(r.Context(), r, "refresh_all_ip_purity", "hysteria_node", "",
		map[string]any{"ok": ok, "skipped": skipped, "failed": failed})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "skipped": skipped, "failed": failed, "errors": errorsOut,
	})
}

// refreshAllPurityRunning 标记批量纯净度检测是否正在进行。
var refreshAllPurityRunning atomic.Bool

// listIPPurity 一次性返回**所有在役节点**的纯净度快照（不联网，只读库）。
//
// 专用「IP 纯净度」页面用这个端点：一次拿全量，前端渲染成一张表，
// 用户一眼扫完所有节点。与单节点 GET 一样走 readLimiter，绝不触发第三方查询 ——
// 检测是显式动作（refreshNodeIPPurity / refreshAllIPPurity）。
//
// 每行都带上节点名、当前应检测 IP、以及 stale 标记：
//   - 从未检测过 → checked=false，reason 说明原因（如"无可检测公网 IP"）
//   - 检测过     → checked=true，purity 是最近一次**成功**的结果
//
// 这样前端不用为每个节点再补一次请求。
func (s *Server) listIPPurity(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}

	rows, err := s.db.Query(r.Context(), `
SELECT n.id, n.name, s.name, s.host,
       COALESCE(p.ip,''), COALESCE(p.connection_type,'unknown'),
       p.is_dual_isp, p.fraud_score, p.is_proxy, p.is_vpn, p.is_abuser,
       COALESCE(p.asn,''), COALESCE(p.asn_org,''),
       COALESCE(p.raw, '{}'::jsonb), COALESCE(p.source,''), COALESCE(p.error,''),
       p.checked_at, (p.node_id IS NOT NULL) AS has_row,
       COALESCE(p.reg_country,''), COALESCE(p.reg_registry,''), COALESCE(p.geo_country,''), p.is_native, COALESCE(p.score_source,'')
FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id
LEFT JOIN node_ip_purity p ON p.node_id = n.id
WHERE n.deleted_at IS NULL
ORDER BY s.name, n.name`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	type item struct {
		NodeID     string       `json:"node_id"`
		NodeName   string       `json:"node_name"`
		ServerName string       `json:"server_name"`
		Host       string       `json:"host"`
		Checked    bool         `json:"checked"`
		Reason     string       `json:"reason,omitempty"`
		Purity     *ipPurityRow `json:"purity,omitempty"`
	}

	out := make([]item, 0, 32)
	for rows.Next() {
		var (
			it        item
			currentIP string
			hostRaw   string
			row       ipPurityRow
			raw       []byte
			hasRow    bool
		)
		if err := rows.Scan(
			&it.NodeID, &it.NodeName, &it.ServerName, &hostRaw,
			&row.IP, &row.ConnectionType,
			&row.DualISP, &row.FraudScore, &row.Proxy, &row.VPN, &row.Abuser,
			&row.ASN, &row.ASNOrg,
			&raw, &row.Source, &row.Error,
			&row.CheckedAt, &hasRow,
			&row.RegCountry, &row.RegRegistry, &row.GeoCountry, &row.Native, &row.ScoreSource,
		); err != nil {
			s.databaseError(w, r, err)
			return
		}
		row.Raw = raw
		// 该检测哪个 IP：servers.host 若是公网字面量就用它（与 nodeIPForPurity 同源）。
		if ippurity.IsPublicIP(strings.TrimSpace(hostRaw)) {
			currentIP = strings.TrimSpace(hostRaw)
		}
		it.Host = currentIP

		if !hasRow {
			it.Checked = false
			if currentIP == "" {
				it.Reason = "该节点没有可检测的公网 IP"
			} else {
				it.Reason = "尚未检测"
			}
		} else {
			it.Checked = true
			row.Stale = row.IP != "" && currentIP != "" && row.IP != currentIP
			it.Purity = &row
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

var (
	errPurityNodeNotFound = errors.New("节点不存在")
	errPurityDisabled     = errors.New("IP 纯净度检测未启用（未配置离线库与 API 密钥）")
)

// checkAndStorePurity 是"检测 + 落库"的唯一实现，被单节点与批量两处共用。
//
// 落库策略（关键）：**失败的检测不覆盖上一条成功结果**。失败只写 error 列；
// 成功才更新判定字段与 checked_at。这样"上次成功的数据"与"这次失败的记录"
// 能同时存在，界面可以诚实表达两件事。
func (s *Server) checkAndStorePurity(ctx context.Context, nodeID string, forceAPI bool) (ipPurityRow, error) {
	ip, err := s.nodeIPForPurity(ctx, nodeID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ipPurityRow{}, errPurityNodeNotFound
		}
		return ipPurityRow{}, err
	}
	if ip == "" {
		// 没有可检测 IP 不是错误，是"跳过"。返回空 IP 让调用方计为 skipped。
		return ipPurityRow{}, nil
	}
	if s.purity == nil {
		return ipPurityRow{}, errPurityDisabled
	}

	callCtx, cancel := context.WithTimeout(ctx, purityCheckTimeout)
	defer cancel()
	verdict, lookupErr := s.purity.Lookup(callCtx, ip, forceAPI)
	if lookupErr != nil {
		// 失败：只记 error，不动旧结果。
		s.recordPurityError(ctx, nodeID, ip, lookupErr.Error())
		return ipPurityRow{IP: ip, Error: lookupErr.Error()}, lookupErr
	}

	raw, _ := json.Marshal(verdict.Raw)
	// ON CONFLICT 覆盖判定字段与 checked_at、清空 error；node_id 是主键。
	// 用 db.Exec 而非返回行，然后再 load 一次保证返回的是库里的真值
	// （含 DB 默认值），避免"接口说的"和"库里的"不一致。
	_, err = s.db.Exec(ctx, upsertIPPuritySQL,
		nodeID, verdict.IP, verdict.ConnectionType, verdict.DualISP, verdict.FraudScore,
		verdict.Proxy, verdict.VPN, verdict.Abuser, verdict.ASN, verdict.ASNOrg,
		string(raw), verdict.Source, verdict.ScoreSource, verdict.Partial)
	if err != nil {
		return ipPurityRow{}, err
	}
	s.checkAndStoreNative(ctx, nodeID, verdict.IP)
	row, _, err := s.loadIPPurity(ctx, nodeID)
	if err != nil {
		return ipPurityRow{}, err
	}
	return row, nil
}

// recordPurityError 记一次失败：只写 error 与 updated_at，保留 checked_at 与判定。
// 若该节点还没有任何记录，则插一行（checked_at 为 NULL，表示"从未成功过"）。
func (s *Server) recordPurityError(ctx context.Context, nodeID, ip, reason string) {
	if _, err := s.db.Exec(ctx, `
INSERT INTO node_ip_purity (node_id, ip, error, updated_at)
VALUES ($1::uuid, $2, $3, now())
ON CONFLICT (node_id) DO UPDATE SET error = EXCLUDED.error, updated_at = now()`,
		nodeID, ip, reason); err != nil {
		s.logger.Warn("ip purity: record error failed", "node_id", nodeID, "error", err)
	}
}

// loadIPPurity 读一行。found=false 表示从未检测过。
func (s *Server) loadIPPurity(ctx context.Context, nodeID string) (ipPurityRow, bool, error) {
	var row ipPurityRow
	var raw []byte
	err := s.db.QueryRow(ctx, `
SELECT COALESCE(ip,''), COALESCE(connection_type,'unknown'), is_dual_isp, fraud_score,
       is_proxy, is_vpn, is_abuser, COALESCE(asn,''), COALESCE(asn_org,''),
       COALESCE(raw, '{}'::jsonb), COALESCE(source,''), COALESCE(error,''), checked_at,
       reg_country, reg_registry, geo_country, is_native, score_source
FROM node_ip_purity WHERE node_id = $1::uuid`, nodeID).Scan(
		&row.IP, &row.ConnectionType, &row.DualISP, &row.FraudScore,
		&row.Proxy, &row.VPN, &row.Abuser, &row.ASN, &row.ASNOrg,
		&raw, &row.Source, &row.Error, &row.CheckedAt,
		&row.RegCountry, &row.RegRegistry, &row.GeoCountry, &row.Native, &row.ScoreSource)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ipPurityRow{}, false, nil
		}
		return ipPurityRow{}, false, err
	}
	row.Raw = raw
	return row, true, nil
}

// checkAndStoreNative 判定原生 IP 并落库（尽力而为）。
//
// 注册国走 RDAP、所在国走面板的 geo 解析（与节点命名同源，不消耗 ipapi.is 配额）。
// 任一侧查询失败只让 is_native 留空 —— 不让纯净度检测因此整体失败，
// 也不覆盖上次查到的那一侧的值。
func (s *Server) checkAndStoreNative(ctx context.Context, nodeID, ip string) {
	if ip == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	reg, registry, regErr := ippurity.RegistrationCountry(callCtx, ip)
	if regErr != nil {
		s.logger.Warn("ip native: rdap lookup failed", "ip", ip, "error", regErr)
	}
	reg = strings.ToUpper(strings.TrimSpace(reg))
	registry = strings.TrimSpace(registry)
	geoCountry := ""
	if s.geo != nil {
		if info, err := s.geo.Lookup(callCtx, ip); err == nil {
			geoCountry = strings.ToUpper(strings.TrimSpace(info.Country))
		} else {
			s.logger.Warn("ip native: geo lookup failed", "ip", ip, "error", err)
		}
	}
	// 单条原子更新同时写入注册国、所在国与原生判定，减少一次数据库往返并消除两步更新间的中间态。
	if _, err := s.db.Exec(ctx, `
UPDATE node_ip_purity SET
    reg_country  = CASE WHEN $2 <> '' THEN $2 ELSE reg_country END,
    reg_registry = CASE WHEN $2 <> '' THEN $3 ELSE reg_registry END,
    geo_country  = CASE WHEN $4 <> '' THEN $4 ELSE geo_country END,
    is_native    = CASE
        WHEN (CASE WHEN $2 <> '' THEN $2 ELSE reg_country END) <> ''
         AND (CASE WHEN $4 <> '' THEN $4 ELSE geo_country END) <> ''
        THEN (CASE WHEN $2 <> '' THEN $2 ELSE reg_country END) = (CASE WHEN $4 <> '' THEN $4 ELSE geo_country END)
        ELSE NULL END,
    updated_at = now()
WHERE node_id = $1::uuid`, nodeID, reg, registry, geoCountry); err != nil {
		s.logger.Warn("ip native: persist failed", "node", nodeID, "error", err)
	}
}

// BackfillIPNative 在启动时为「已检测过纯净度、但还没有原生判定」的节点补查
// 一次原生 IP（2026-10-15 上线时存量数据都没有）。只用 RDAP 与面板 geo，
// 不消耗 ipapi.is 配额。逐台串行、每台间隔 2 秒，对 RDAP 足够礼貌。
func (s *Server) BackfillIPNative(ctx context.Context) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil && s.logger != nil {
				s.logger.Error("ip native backfill panic", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		rows, err := s.db.Query(ctx, `
SELECT p.node_id::text, p.ip FROM node_ip_purity p
JOIN hysteria_nodes n ON n.id = p.node_id AND n.deleted_at IS NULL
WHERE p.checked_at IS NOT NULL AND p.ip <> '' AND p.is_native IS NULL`)
		if err != nil {
			s.logger.Warn("ip native backfill: query failed", "error", err)
			return
		}
		type pending struct{ id, ip string }
		var list []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.ip); err != nil {
				rows.Close()
				s.logger.Warn("ip native backfill: row scan failed", "error", err)
				return
			}
			list = append(list, p)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			s.logger.Warn("ip native backfill: row iteration failed", "error", err)
			return
		}
		rows.Close()
		for i, p := range list {
			if i > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			s.checkAndStoreNative(ctx, p.id, p.ip)
		}
		if len(list) > 0 {
			s.logger.Info("ip native backfill: done", "count", len(list))
		}
	}()
}

// upsertIPPuritySQL 写入一次成功的纯净度检测结果（$1 node_id … $13 score_source，$14 partial 原因）。
//
// 2026-11-06：$14 非空（公网 API 或单 IP 评分失败、用的是降级结果）且 IP 没变、库里已有评分时，
// 评分类字段（评分、风险标记、双 ISP、评分来源）保留上一次的，error 写原因。原来一律覆盖：
// ipapi 返回 429 时评分被写成 NULL，IPQS 额度用尽时单 IP 分被 ASN 级分数顶掉，定时巡检还报「评分 85 → 0」。
const upsertIPPuritySQL = `
INSERT INTO node_ip_purity (
    node_id, ip, connection_type, is_dual_isp, fraud_score,
    is_proxy, is_vpn, is_abuser, asn, asn_org, raw, source, score_source, error, checked_at, updated_at
) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13, $14, now(), now())
ON CONFLICT (node_id) DO UPDATE SET
    -- 2026-11-04：IP 变了，旧 IP 的注册国 / 所在国与原生判定一起作废。原来只覆盖 ip：
    -- checkAndStoreNative 对查不到的一侧保留旧值，于是新 IP 的原生判定由旧 IP 的国家算出来。
    reg_country  = CASE WHEN node_ip_purity.ip IS DISTINCT FROM EXCLUDED.ip THEN '' ELSE node_ip_purity.reg_country END,
    reg_registry = CASE WHEN node_ip_purity.ip IS DISTINCT FROM EXCLUDED.ip THEN '' ELSE node_ip_purity.reg_registry END,
    geo_country  = CASE WHEN node_ip_purity.ip IS DISTINCT FROM EXCLUDED.ip THEN '' ELSE node_ip_purity.geo_country END,
    is_native    = CASE WHEN node_ip_purity.ip IS DISTINCT FROM EXCLUDED.ip THEN NULL ELSE node_ip_purity.is_native END,
    ip = EXCLUDED.ip,
    connection_type = EXCLUDED.connection_type,
    is_dual_isp = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.is_dual_isp ELSE EXCLUDED.is_dual_isp END,
    fraud_score = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.fraud_score ELSE EXCLUDED.fraud_score END,
    is_proxy = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.is_proxy ELSE EXCLUDED.is_proxy END,
    is_vpn = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.is_vpn ELSE EXCLUDED.is_vpn END,
    is_abuser = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.is_abuser ELSE EXCLUDED.is_abuser END,
    asn = EXCLUDED.asn,
    asn_org = EXCLUDED.asn_org,
    raw = EXCLUDED.raw,
    source = EXCLUDED.source,
    score_source = CASE WHEN $14 <> '' AND node_ip_purity.ip IS NOT DISTINCT FROM EXCLUDED.ip AND node_ip_purity.fraud_score IS NOT NULL
        THEN node_ip_purity.score_source ELSE EXCLUDED.score_source END,
    error = $14,
    checked_at = now(),
    updated_at = now()`
