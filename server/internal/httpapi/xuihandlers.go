package httpapi

// xuihandlers.go — 3X-UI 面板的 HTTP 端点。
//
// 路由（server.go 注册）：
//
//	GET    /api/xui/panels                                  列表（含缓存状态）
//	POST   /api/xui/panels                                  添加（实时探测后入库）
//	POST   /api/xui/panels/preview                          预览（只探测不入库）
//	DELETE /api/xui/panels/{id}                             删除（需确认头）
//	POST   /api/xui/panels/{id}/refresh                     重新探测写回状态
//	GET    /api/xui/panels/{id}/inbounds                    inbound + 客户端 + 在线标记
//	POST   /api/xui/panels/{id}/clients                     添加客户端（addClient）
//	DELETE /api/xui/panels/{id}/inbounds/{inboundID}/clients/{clientId}   删客户端
//	POST   /api/xui/panels/{id}/inbounds/{inboundID}/clients/{email}/reset 重置流量
//
// 约定与 portal 侧一致：读走 readLimiter、写走 writeLimiter；
// 危险操作（删面板、删客户端）需要 X-Confirm-Dangerous: yes；
// 审计只记「做了什么」，绝不记凭据。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"nodeops/server/internal/secrets"

	"github.com/jackc/pgx/v5"
)

// ---- 列表与登记 ----

// GET /api/xui/panels
func (s *Server) xuiPanels(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT id, name, base_url, username, last_status, last_error, last_checked_at, inbounds_count, clients_count, online_count, created_at, updated_at, region, provider, name_source, region_source, provider_source FROM xui_panels ORDER BY created_at ASC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, baseURL, username, lastStatus, lastError string
		var region, provider, nameSource, regionSource, providerSource string
		var inbounds, clients, online int
		var checkedAt *time.Time
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &name, &baseURL, &username, &lastStatus, &lastError, &checkedAt, &inbounds, &clients, &online, &createdAt, &updatedAt,
			&region, &provider, &nameSource, &regionSource, &providerSource); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{
			"id": id, "name": name, "base_url": baseURL, "username": username,
			"last_status": lastStatus, "last_error": lastError, "last_checked_at": checkedAt,
			"inbounds_count": inbounds, "clients_count": clients, "online_count": online,
			"created_at": createdAt, "updated_at": updatedAt,
			// 地区与商家：自动识别填入，前端展示 + 允许手工纠正。
			// 来源标记一并返回，让前端能标出「这是自动认出来的」，
			// 用户改过之后就不会再被自动覆盖。
			"region": region, "provider": provider,
			"name_source": nameSource, "region_source": regionSource, "provider_source": providerSource,
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// xuiPanelInput 添加/预览共用入参。
type xuiPanelInput struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// validate 检查必填项，返回归一后的 base_url。
//
// Name **不是必填**：留空时由调用方按面板地址里的公网 IP 自动识别地区与商家
// 来命名（见 xuiCreatePanel）。识别也失败时退回用 base_url 当名字 ——
// 一个用地址当名字的面板至少还能被认出来，比拒绝添加有用得多。
func (in *xuiPanelInput) validate() (string, error) {
	if strings.TrimSpace(in.Username) == "" {
		return "", errors.New("请填写 3X-UI 用户名")
	}
	if in.Password == "" {
		return "", errors.New("请填写 3X-UI 密码")
	}
	return normalizeXUIBaseURL(in.BaseURL)
}

// xuiProbeResult 是一次探测的结果，带上面板代际标记 —— 调用侧要靠它决定
// 客户端标识用 clientId（旧版）还是 email（新版）。
type xuiProbeResult struct {
	Inbounds []xuiInboundSummary
	Onlines  []string
	// OnlinesOK：在线列表确实取到了。2026-11-06：取不到时原来当成「0 人在线」并以 ok 写库，
	// 覆盖上次成功的在线数，界面上所有客户端都显示离线。
	OnlinesOK bool
	Modern    bool
}

// probeXUIPanel 是 probeXUIPanelWith 的兼容包装，只关心 inbounds/onlines 的调用点用它。
func probeXUIPanel(ctx context.Context, cryptoBox *secrets.Box, baseURL, username, password string) (inbounds []xuiInboundSummary, onlineEmails []string, err error) {
	result, err := probeXUIPanelWith(ctx, cryptoBox, baseURL, username, password)
	if err != nil {
		return nil, nil, err
	}
	return result.Inbounds, result.Onlines, nil
}

// probeXUIPanelWith 用给定凭据真实登录并拉一次 inbounds + onlines。
// 这是「添加面板」的验证方式 —— 3X-UI 没有独立的轻量身份校验端点，
// list 是最轻的真实读。
func probeXUIPanelWith(ctx context.Context, cryptoBox *secrets.Box, baseURL, username, password string) (*xuiProbeResult, error) {
	session, err := xuiSession(ctx, cryptoBox, baseURL, username, password)
	if err != nil {
		return nil, err
	}
	listEnv, err := session.get(ctx, xuiAPIPrefix(baseURL), "/inbounds/list")
	if err != nil {
		return nil, err
	}
	var listRaw []map[string]any
	if len(listEnv.Obj) > 0 && string(listEnv.Obj) != "null" {
		if err := json.Unmarshal(listEnv.Obj, &listRaw); err != nil {
			return nil, errors.New("3X-UI inbound 列表无法解析")
		}
	}
	inbounds := make([]xuiInboundSummary, 0, len(listRaw))
	for _, item := range listRaw {
		inbounds = append(inbounds, normalizeXUIInbound(item))
	}
	onlines, onlinesOK := fetchXUIOnlines(ctx, session, baseURL)
	return &xuiProbeResult{
		Inbounds:  inbounds,
		Onlines:   onlines,
		OnlinesOK: onlinesOK,
		Modern:    session.modern,
	}, nil
}

// fetchXUIOnlines 取在线 email 列表。新旧版路径不同，任一成功即返回。
// 拿不到不算失败 —— 有的部署会关闭在线统计，主数据是 inbounds。
func fetchXUIOnlines(ctx context.Context, session *xuiSessionClient, baseURL string) ([]string, bool) {
	paths := []string{"/inbounds/onlines"}
	if session.modern {
		// 新版把在线查询挪到了 clients 命名空间。
		paths = []string{"/clients/onlines", "/inbounds/onlines"}
	}
	for _, path := range paths {
		envelope, err := session.get(ctx, xuiAPIPrefix(baseURL), path)
		if err != nil {
			continue
		}
		// 接口成功、obj 为空 / null：确实没人在线。
		if len(envelope.Obj) == 0 || string(envelope.Obj) == "null" {
			return []string{}, true
		}
		var raw []string
		if json.Unmarshal(envelope.Obj, &raw) == nil {
			return raw, true
		}
	}
	return []string{}, false
}

// POST /api/xui/panels/preview — 只探测不入库，供添加对话框确认连通性。
func (s *Server) xuiPreviewPanel(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	var input xuiPanelInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	baseURL, err := input.validate()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	inbounds, online, err := probeXUIPanel(r.Context(), s.box, baseURL, strings.TrimSpace(input.Username), input.Password)
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	clients := 0
	for _, inbound := range inbounds {
		clients += len(inbound.Clients)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":       baseURL,
		"inbounds_count": len(inbounds),
		"clients_count":  clients,
		"online_count":   len(online),
	})
}

// POST /api/xui/panels — 探测成功后入库。凭据信封加密，任何接口不回显。
func (s *Server) xuiCreatePanel(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input xuiPanelInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	baseURL, err := input.validate()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	username := strings.TrimSpace(input.Username)
	inbounds, online, err := probeXUIPanel(r.Context(), s.box, baseURL, username, input.Password)
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	ciphertext, err := s.box.Seal([]byte(input.Password))
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	clients := 0
	for _, inbound := range inbounds {
		clients += len(inbound.Clients)
	}

	// 自动识别地区与商家（与其他新增入口共用同一套规则，见 geoapply.go）。
	//
	// 输入取自 base_url 的**主机名**，且只有它是裸公网 IP 时才查 ——
	// 域名形态（panel.example.com）不查，理由见 hostFromURL 的注释：
	// 反解域名得到的是 CDN 边缘节点，不是这台机器真实所在，比不识别更误导。
	//
	// current 全部为空（这是新建行），于是识别成功就会填 name/region/provider；
	// 失败则三样都保持原样，即"名字用用户填的、地区商家留空"。
	geoResult := applyGeo(geoCurrent{
		Name: strings.TrimSpace(input.Name),
		// 2026-11-03：只有用户没填名字时名字才可被识别结果替换。原来写死 true，
		// 运维手填的「HK-自建」会被改成「日本-东京」并记为 auto，违反 geoapply 的规则 2。
		NameAuto:     strings.TrimSpace(input.Name) == "",
		RegionAuto:   true,
		ProviderAuto: true,
	}, s.resolveGeo(r.Context(), hostFromURL(baseURL)))

	name := strings.TrimSpace(input.Name)
	if geoResult.Applied && strings.TrimSpace(geoResult.Name) != "" {
		name = strings.TrimSpace(geoResult.Name)
	}
	// 名字最终还是空（用户没填 + 识别不出来）：用地址兜底。
	// 一个用地址当名字的面板还能被认出来；空名字在列表里就是一行空白，
	// 连点进去的欲望都没有。
	if name == "" {
		name = baseURL
		// 兜底值不是识别结果，标成 manual，让以后有机会被识别更新。
		geoResult.NameAuto = false
	}

	var id string
	err = s.db.QueryRow(r.Context(), `INSERT INTO xui_panels
(name, base_url, username, password, last_status, last_checked_at, inbounds_count, clients_count, online_count,
 region, provider, name_source, region_source, provider_source)
VALUES ($1, $2, $3, $4, 'ok', now(), $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
		name, baseURL, username, ciphertext, len(inbounds), clients, len(online),
		geoResult.Region, geoResult.Provider,
		autoSource(geoResult.NameAuto), autoSource(geoResult.RegionAuto), autoSource(geoResult.ProviderAuto)).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "这个 3X-UI 地址已经登记过"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_xui_panel", "xui_panel", id, map[string]any{
		"name": name, "base_url": baseURL, "inbounds_count": len(inbounds),
		"geo_region": geoResult.Region, "geo_provider": geoResult.Provider, "geo_source": geoResult.Source,
	})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// DELETE /api/xui/panels/{id}
func (s *Server) xuiDeletePanel(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除 3X-UI 面板需要 X-Confirm-Dangerous: yes"})
		return
	}
	panelID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "3X-UI 面板不存在"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `DELETE FROM xui_panels WHERE id=$1::uuid`, panelID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "3X-UI 面板不存在"})
		return
	}
	s.audit(r.Context(), r, "delete_xui_panel", "xui_panel", panelID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// POST /api/xui/panels/{id}/refresh — 重新探测并写回缓存状态（列表页「刷新」）。
func (s *Server) xuiRefreshPanel(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadXUITarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	probe, err := probeXUIPanelWith(r.Context(), s.box, target.BaseURL, target.Username, target.Password)
	if err != nil {
		s.recordXUIStatus(r.Context(), target.ID, "error", err.Error(), 0, 0, 0)
		s.writeXUITargetError(w, err)
		return
	}
	clients := 0
	for _, inbound := range probe.Inbounds {
		clients += len(inbound.Clients)
	}
	onlineCount := len(probe.Onlines)
	resp := map[string]any{
		"status":         "ok",
		"inbounds_count": len(probe.Inbounds),
		"clients_count":  clients,
		"online_count":   onlineCount,
	}
	if !probe.OnlinesOK {
		// 在线列表没取到：保留库里上次的在线数（-1），回包如实说明，不报「0 人在线」。
		onlineCount = -1
		resp["online_count"] = nil
		resp["onlines_error"] = "在线列表未取到（面板接口失败或已关闭在线统计）"
	}
	s.recordXUIStatus(r.Context(), target.ID, "ok", "", len(probe.Inbounds), clients, onlineCount)
	writeJSON(w, http.StatusOK, resp)
}

// ---- 运行时操作（每次重新登录，不缓存会话）----

// GET /api/xui/panels/{id}/inbounds — 实时拉取 inbound + 客户端 + 在线标记。
func (s *Server) xuiInbounds(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadXUITarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	probe, err := probeXUIPanelWith(r.Context(), s.box, target.BaseURL, target.Username, target.Password)
	if err != nil {
		s.recordXUIStatus(r.Context(), target.ID, "error", err.Error(), 0, 0, 0)
		s.writeXUITargetError(w, err)
		return
	}
	online := probe.Onlines
	onlineSet := make(map[string]bool, len(online))
	for _, email := range online {
		onlineSet[email] = true
	}
	// 2026-11-06：client_id 在旧版面板上就是客户端的 UUID / 密码（xuiClientIdentity），
	// 只有管理员需要它（删除客户端是 requireAdmin）；其他角色一律留空。
	isAdmin := claims.Role == "admin"
	clients := make([]xuiClientStatus, 0)
	for _, inbound := range probe.Inbounds {
		for _, client := range inbound.Clients {
			email, clientID := xuiClientIdentity(client, inbound.Protocol)
			// 新版按 email 定位客户端（del/reset 都以 email 为路径段），所以
			// 直接把 client_id 暴露成 email，前端无需分版本。
			if probe.Modern {
				clientID = email
			}
			if !isAdmin {
				clientID = ""
			}
			status := xuiClientStatus{
				Email:       email,
				ClientID:    clientID,
				InboundID:   inbound.ID,
				InboundName: inbound.Remark,
				Protocol:    inbound.Protocol,
				Online:      onlineSet[email],
				ExpiryTime:  xuiAsInt64(client["expiryTime"]),
				TotalBytes:  xuiAsInt64(client["totalGB"]),
				// 流量已由 normalizeXUIInbound 从两代结构里统一提到 client 上。
				UpBytes:   xuiAsInt64(client["up"]),
				DownBytes: xuiAsInt64(client["down"]),
			}
			clients = append(clients, status)
		}
	}
	onlineCount := len(online)
	resp := map[string]any{
		"panel_id":   target.ID,
		"panel_name": target.Name,
		"inbounds":   redactXUIInbounds(probe.Inbounds),
		"clients":    clients,
		"onlines":    online,
	}
	if !probe.OnlinesOK {
		onlineCount = -1
		resp["onlines_error"] = "在线列表未取到（面板接口失败或已关闭在线统计），在线状态未知"
	}
	s.recordXUIStatus(r.Context(), target.ID, "ok", "", len(probe.Inbounds), len(clients), onlineCount)
	writeJSON(w, http.StatusOK, resp)
}

// xuiAddClientInput 添加客户端入参。
type xuiAddClientInput struct {
	InboundID  int    `json:"inbound_id"`
	Email      string `json:"email"`
	LimitTotal int64  `json:"limit_total_bytes"` // 0 = 不限量
	ExpireDays int    `json:"expire_days"`       // 0 = 永不过期
}

// xuiEmailPattern 卡 email 形态：它会被拼进 3X-UI 的 settings JSON 与路径段。
// 只允许真实 email 常用的字符，从根上杜绝注入面 —— 与 portalAction 里
// warpDomainPattern 的白名单思路一致。
var xuiEmailPattern = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,64}$`)

// xuiPathSegmentPattern 路径里的 clientId / email 段。
var xuiPathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,128}$`)

// xuiInboundIDPattern 入站 ID：3X-UI 的入站 ID 是正整数（2026-11-04）。原来与 clientId 共用上面那条，
// 允许「.」：`..` 会原样拼进旧版面板的 /panel/api/inbounds/{id}/… 路径，把请求导向别的接口。
var xuiInboundIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// randomXUISubID 生成分订阅 ID。3X-UI 用它做 /sub/ 订阅路径段，8-16 位小写
// hex 是各版本的共同形态。
func randomXUISubID() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// xuiClientPayload 按协议构造一个最小合法的 client。
//
// 为什么由服务端构造而不是前端拼整包：addClient 的 settings 字段是
// **整份 inbound settings 的 JSON 字符串**，前端拼错一个字段就可能把整个
// inbound 的 settings 覆盖坏。而 3X-UI 对 addClient 实际只取 clients 数组里
// 的新成员（其余 settings 字段在 addClient 路径上不会被合并写回 —— 以官方
// web/service/inbound.go 的 addClient 实现为准：它只把新 client append 进
// 既有 clients）。所以这里送「一个最小 client」是各版本里最稳的形状。
func xuiClientPayload(input xuiAddClientInput) map[string]any {
	// tgId 必须是整数，不能是字符串。
	//
	// 3X-UI 3.8.5 的新版 clients/add 走的是 clientPayloadWithHwid 结构体，
	// 它的 tgId 字段是 int64。早期（以及前端表单）习惯把它写成 "" 表示
	// 「没有绑定 Telegram」，但在新版会被 json.Unmarshal 直接拒绝：
	//   json: cannot unmarshal string into Go struct field
	//   clientPayloadWithHwid.tgId of type int64
	// 面板会把这句原文透给我们，表现为开户 502。改用 0 表示未绑定。
	client := map[string]any{
		"email":      input.Email,
		"enable":     true,
		"limitIp":    0,
		"totalGB":    input.LimitTotal,
		"expiryTime": 0,
		"tgId":       0,
		"subId":      randomXUISubID(),
		"reset":      0,
	}
	if input.ExpireDays > 0 {
		client["expiryTime"] = time.Now().UnixMilli() + int64(input.ExpireDays)*86_400_000
	}
	return client
}

// POST /api/xui/panels/{id}/clients
func (s *Server) xuiAddClient(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadXUITarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	var input xuiAddClientInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	input.Email = strings.TrimSpace(input.Email)
	if input.InboundID < 1 || !xuiEmailPattern.MatchString(input.Email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "inbound_id 需为正整数；email 只允许字母数字与 ._@+-，长度 1-64"})
		return
	}
	if input.LimitTotal < 0 || input.ExpireDays < 0 || input.ExpireDays > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit_total_bytes 需 >= 0；expire_days 取 0-3650（0 为永不过期）"})
		return
	}
	session, err := xuiSession(r.Context(), s.box, target.BaseURL, target.Username, target.Password)
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	if err := addXUIClient(r.Context(), session, target.BaseURL, input); err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	s.audit(r.Context(), r, "xui_add_client", "xui_panel", target.ID, map[string]any{
		"inbound_id": input.InboundID, "email": input.Email,
		"limit_total_bytes": input.LimitTotal, "expire_days": input.ExpireDays,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

// addXUIClient 按面板代际选开户方式。
//
// 新版走 /panel/api/clients/add，body 是 JSON 且 inbound 字段是**数组**
// （{"inboundIds":[1],"client":{...}}）—— 实测少写数组会报
// "at least one inbound is required"。
// 旧版走 /panel/api/inbounds/addClient，form 提交 id + settings（整份 settings
// 的 JSON 字符串，clients 里只放新成员；3X-UI 只 append 新 client，不会覆盖其余字段）。
func addXUIClient(ctx context.Context, session *xuiSessionClient, baseURL string, input xuiAddClientInput) error {
	client := xuiClientPayload(input)
	if session.modern {
		_, err := session.postJSON(ctx, xuiAPIPrefix(baseURL), "/clients/add", map[string]any{
			"inboundIds": []int{input.InboundID},
			"client":     client,
		})
		return err
	}
	// 2026-11-03：旧版 addClient 不会补协议默认值（新版 clients/add 会，见 3X-UI
	// client_crud.go 的 fillProtocolDefaults），vless / vmess 缺 id、trojan 缺 password
	// 直接报「empty client ID」。按 inbound 的协议补上；shadowsocks 用 email 定位、不需要。
	protocol, err := xuiInboundProtocol(ctx, session, baseURL, input.InboundID)
	if err != nil {
		return err
	}
	if err := fillLegacyXUIClientIdentity(client, protocol); err != nil {
		return err
	}
	settings, err := json.Marshal(map[string]any{"clients": []any{client}, "decryption": "none"})
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("id", fmt.Sprintf("%d", input.InboundID))
	form.Set("settings", string(settings))
	_, err = session.post(ctx, xuiAPIPrefix(baseURL), "/inbounds/addClient", form)
	return err
}

// xuiInboundProtocol 读一个 inbound 的协议（旧版开户前用来决定要补哪个身份字段）。
func xuiInboundProtocol(ctx context.Context, session *xuiSessionClient, baseURL string, inboundID int) (string, error) {
	env, err := session.get(ctx, xuiAPIPrefix(baseURL), fmt.Sprintf("/inbounds/get/%d", inboundID))
	if err != nil {
		return "", err
	}
	var inbound struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(env.Obj, &inbound); err != nil || inbound.Protocol == "" {
		return "", errors.New("读取 3X-UI inbound 协议失败")
	}
	return strings.ToLower(inbound.Protocol), nil
}

// fillLegacyXUIClientIdentity 按协议补上旧版面板要求的客户端身份字段。
func fillLegacyXUIClientIdentity(client map[string]any, protocol string) error {
	switch protocol {
	case "vless", "vmess":
		id, err := randomUUIDv4()
		if err != nil {
			return err
		}
		client["id"] = id
	case "trojan":
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		client["password"] = hex.EncodeToString(raw)
	case "shadowsocks":
		// 按 email 定位；密码由面板按加密方式生成。
	default:
		return fmt.Errorf("暂不支持在旧版 3X-UI 上为 %s 协议开户", protocol)
	}
	return nil
}

// randomUUIDv4 生成一个随机 UUID（RFC 4122 v4）。
func randomUUIDv4() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// DELETE /api/xui/panels/{id}/inbounds/{inboundID}/clients/{clientId}
func (s *Server) xuiDeleteClient(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除客户端需要 X-Confirm-Dangerous: yes"})
		return
	}
	inboundID := r.PathValue("inboundID")
	clientID := r.PathValue("clientId")
	if !xuiInboundIDPattern.MatchString(inboundID) || !xuiPathSegmentPattern.MatchString(clientID) || clientID == "." || clientID == ".." {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "路径参数不合法"})
		return
	}
	target, err := s.loadXUITarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	session, err := xuiSession(r.Context(), s.box, target.BaseURL, target.Username, target.Password)
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	if err := deleteXUIClient(r.Context(), session, target.BaseURL, inboundID, clientID); err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	s.audit(r.Context(), r, "xui_delete_client", "xui_panel", target.ID, map[string]any{
		"inbound_id": inboundID, "client_id": clientID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// deleteXUIClient 按面板代际选删户方式。
//
// 新版是 POST /panel/api/clients/del/{email} —— 按 **email** 定位，且不再需要
// inboundID（客户端在全局命名空间里按 email 唯一）。前端传来的 clientId 在
// 新版下就是 email（见 xuiClientIdentity 与前端抽屉的构造）。
// 旧版是 POST /panel/api/inbounds/{id}/delClient/{clientId}，clientId 按协议取
// id/password/email。
func deleteXUIClient(ctx context.Context, session *xuiSessionClient, baseURL, inboundID, clientID string) error {
	if session.modern {
		_, err := session.postJSON(ctx, xuiAPIPrefix(baseURL), "/clients/del/"+url.PathEscape(clientID), nil)
		return err
	}
	_, err := session.post(ctx, xuiAPIPrefix(baseURL), "/inbounds/"+inboundID+"/delClient/"+url.PathEscape(clientID), nil)
	return err
}

// POST /api/xui/panels/{id}/inbounds/{inboundID}/clients/{email}/reset
func (s *Server) xuiResetClientTraffic(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	inboundID := r.PathValue("inboundID")
	email := r.PathValue("email")
	if !xuiInboundIDPattern.MatchString(inboundID) || !xuiEmailPattern.MatchString(email) || email == "." || email == ".." {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "路径参数不合法"})
		return
	}
	target, err := s.loadXUITarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	session, err := xuiSession(r.Context(), s.box, target.BaseURL, target.Username, target.Password)
	if err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	if err := resetXUIClientTraffic(r.Context(), session, target.BaseURL, inboundID, email); err != nil {
		s.writeXUITargetError(w, err)
		return
	}
	s.audit(r.Context(), r, "xui_reset_client_traffic", "xui_panel", target.ID, map[string]any{
		"inbound_id": inboundID, "email": email,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// resetXUIClientTraffic 按面板代际选重置方式。两代都按 email 走路径段，
// 只是前缀不同：新版 /panel/api/clients/resetTraffic/{email}，
// 旧版 /panel/api/inbounds/{id}/resetClientTraffic/{email}。
func resetXUIClientTraffic(ctx context.Context, session *xuiSessionClient, baseURL, inboundID, email string) error {
	escaped := url.PathEscape(email)
	if session.modern {
		_, err := session.postJSON(ctx, xuiAPIPrefix(baseURL), "/clients/resetTraffic/"+escaped, nil)
		return err
	}
	_, err := session.post(ctx, xuiAPIPrefix(baseURL), "/inbounds/"+inboundID+"/resetClientTraffic/"+escaped, nil)
	return err
}

// pgx import 守卫：loadXUITarget（xuipanel.go）用 pgx.ErrNoRows，这里显式引用
// 以免调整文件拆分时 import 失配。
var _ = pgx.ErrNoRows

// xuiClientPublicFields 是 inbounds[].clients 里可以返回给页面的字段。
var xuiClientPublicFields = []string{"email", "enable", "up", "down", "totalGB", "expiryTime", "lastOnline", "limitIp", "comment"}

// redactXUIInbounds 去掉入站客户端里的凭据（2026-11-06）。
//
// 原来把 settings.clients 原样回给前端：vless/vmess 的 id（UUID）、trojan/ss 的 password、subId、flow
// 全在里面，而这个接口只要求登录 —— 只读账号就能拿到所有客户的连接凭据，
// 有了 subId 还能直接拉 3X-UI 的订阅。页面只用到客户端个数与流量字段。
func redactXUIInbounds(in []xuiInboundSummary) []xuiInboundSummary {
	out := make([]xuiInboundSummary, len(in))
	for i, inbound := range in {
		out[i] = inbound
		out[i].Clients = make([]xuiClientSnapshot, 0, len(inbound.Clients))
		for _, client := range inbound.Clients {
			safe := make(xuiClientSnapshot, len(xuiClientPublicFields))
			for _, k := range xuiClientPublicFields {
				if v, ok := client[k]; ok {
					safe[k] = v
				}
			}
			out[i].Clients = append(out[i].Clients, safe)
		}
	}
	return out
}
