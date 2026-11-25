package httpapi

// xuipanel.go — 3X-UI 面板对接。
//
// 3X-UI 是一个广泛使用的 Xray 管理面板，与本项目管理的 hysteria2 节点是
// 两套独立体系。运维者往往两边都有：hy2 节点在本面板管，3X-UI 面板散落在
// 各处要开各自的网页。本文件把 3X-UI 的 API 纳入本控制台，让「添加一个
// 3X-UI 面板」之后可以直接在这里看 inbound / 客户端 / 流量 / 在线数，
// 并执行开户、删户、重置流量这几类高频操作。
//
// 与 hysteria 节点对接（nodeapi.go）共用的安全姿态：
//
//   1. 凭据信封加密。3X-UI 没有 API token，鉴权完全靠登录会话 cookie，
//      所以 username/password 就是对方的管理员凭据 —— 与 hysteria 节点的
//      api_key 同级，同样 AES-256-GCM 落库、任何接口不回显。
//   2. SSRF 防护复用 portalClient 的拨号器（拒绝内网/链路本地地址），
//      防止把 API 服务端变成探测内网的跳板。
//   3. 响应体截断上限。inbounds/list 在面板很大时能到几百 KB，给 1 MiB。
//
// 3X-UI API 契约 —— 分成两代，实现同时兼容（2026-10-01 在真实面板上逐条实测）。
//
// 旧版（Vue 前端，约 v2.x）：
//
//   POST /login   form 表单 username/password → {"success":true,...}
//                 + Set-Cookie（cookie 名 "session"）
//   GET  /panel/api/inbounds/list        → obj: inbound 数组，settings 是 JSON 字符串
//   GET  /panel/api/inbounds/onlines     → obj: 在线 email 数组
//   POST /panel/api/inbounds/addClient   form: id + settings(JSON 字符串)
//   POST /panel/api/inbounds/{id}/delClient/{clientId}
//   POST /panel/api/inbounds/{id}/resetClientTraffic/{email}
//
// 新版（React 前端，v3.0.2+ / 实测 3.8.5）—— 三处关键差异，缺一不可：
//
//   1. 登录多一步 CSRF。GET /csrf-token 取 obj 作为令牌，登录时必须带
//      X-CSRF-Token 头，否则一律 403（响应体为空，极易误判成「凭据错误」）。
//      cookie 名是 "3x-ui"。
//   2. inbound 列表里 settings 是 **对象**（不是字符串），且客户端流量不再挂在
//      settings.clients[].clientStats 上，而是同级的顶格 clientStats 数组 ——
//      按 email 对齐。
//   3. 客户端增删改迁到 /panel/api/clients/*，且开户 body 是
//      {"inboundIds":[<id>...], "client":{...}}（注意是数组，不是单个 id）。
//      del / resetTraffic 按 **email** 走路径段（旧版 delClient 按 clientId）。
//
// 所有响应统一 {"success":bool,"msg":string,"obj":...} 信封。
//
// 探测新旧的判据：GET /csrf-token 返回 200 且有 obj → 新版；否则 → 旧版。
// 这个判据不依赖版本号字符串（各 fork 版本号混乱），只看行为。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"nodeops/server/internal/secrets"

	"github.com/jackc/pgx/v5"
)

// xuiRequest 描述一次 3X-UI HTTP 调用。form 与 json 二选一（都为 nil 即无 body）。
type xuiRequest struct {
	method string
	url    string
	form   url.Values
	json   []byte
	header map[string]string
	cookie *http.Cookie
}

// xuiMaxRedirects：3X-UI 登录 302 到首页、反代补斜杠之类最多两三跳，再多就是环。
const xuiMaxRedirects = 3

// xuiCheckRedirect 只跟随**同 scheme + 同 host（含端口）**的重定向，最多 3 跳。
// 跨主机一律拒绝：带着会话 cookie / CSRF 头跳到别的主机就是把面板凭据送出去；
// 降级到 http 同理。目的地 IP 仍由 portalClient 的拨号器逐次校验（拒私网）。
func xuiCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > xuiMaxRedirects {
		return fmt.Errorf("3X-UI 重定向超过 %d 次", xuiMaxRedirects)
	}
	origin := via[0].URL
	if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) {
		return errors.New("3X-UI 重定向到了其他主机或协议，已拒绝")
	}
	return nil
}

// xuiHTTPClient 在 base 的基础上派生 3X-UI 专用客户端（2026-10-23）。
//
// 原来直接用 portalClient：它的 CheckRedirect 拒绝一切重定向（与上面注释说的
// 「单独放开」相反），且 10s 的 Client.Timeout 会盖过 xuiCall 里 15s 的请求截止时间，
// 大面板 inbounds/list 偶尔就被掐断。派生客户端共用同一个 Transport
// （同一套 TLS 配置与拒私网拨号器），只换重定向策略、去掉 Client.Timeout，由每次请求的 ctx 管时限。
func xuiHTTPClient(base *http.Client) *http.Client {
	c := *base
	c.Timeout = 0
	c.CheckRedirect = xuiCheckRedirect
	return &c
}

// xuiCall 走 xuiHTTPClient 派生的客户端（拒私网、同源重定向、单次 15s）。
func xuiCall(ctx context.Context, client *http.Client, req xuiRequest) ([]byte, int, []*http.Cookie, error) {
	var body io.Reader
	contentType := ""
	switch {
	case req.json != nil:
		body = strings.NewReader(string(req.json))
		contentType = "application/json"
	case req.form != nil:
		body = strings.NewReader(req.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, req.method, req.url, body)
	if err != nil {
		return nil, 0, nil, err
	}
	httpReq.Header.Set("User-Agent", "hy2-ops-console")
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	for key, value := range req.header {
		httpReq.Header.Set(key, value)
	}
	if req.cookie != nil {
		httpReq.AddCookie(req.cookie)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("连接 3X-UI 面板失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, nil, errors.New("读取 3X-UI 响应失败")
	}
	return raw, resp.StatusCode, resp.Cookies(), nil
}

// xuiEnvelope 是 3X-UI 所有 API 响应的统一外壳。
type xuiEnvelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

// parseXUIResponse 校验状态码与信封。msg 为空时给默认文案，避免把空串当错误抛给操作者。
func parseXUIResponse(raw []byte, statusCode int) (*xuiEnvelope, error) {
	if statusCode == http.StatusNotFound {
		return nil, errors.New("3X-UI 返回 404：确认地址是否多了路径（如反代子路径），API 需直达面板根路径")
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return nil, errors.New("3X-UI 返回 401/403：会话已失效或凭据被拒绝，请重新保存面板凭据")
	}
	var parsed xuiEnvelope
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if statusCode != http.StatusOK {
			return nil, fmt.Errorf("3X-UI 返回 %d，且响应不是合法 JSON", statusCode)
		}
		return nil, errors.New("3X-UI 响应不是合法 JSON：确认这是 3X-UI 面板而不是别的服务")
	}
	if !parsed.Success {
		message := parsed.Msg
		if message == "" {
			message = "未知错误"
		}
		return nil, errors.New("3X-UI 报错: " + message)
	}
	return &parsed, nil
}

// xuiLogin 用用户名密码换取会话 cookie，并识别面板是新版还是旧版。
//
// 新版（React）的登录必须先取 CSRF 令牌并回填到 X-CSRF-Token 头，否则无论
// 凭据对错都是 403 + 空 body —— 这个空 body 会让人误以为是「密码错了」，
// 所以这里把它显式翻译成可执行的提示。
func xuiLogin(ctx context.Context, client *http.Client, baseURL, username, password string) (*xuiSessionClient, error) {
	csrf, modern, bootstrapCookie, err := xuiFetchCSRF(ctx, client, baseURL)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	request := xuiRequest{method: http.MethodPost, url: baseURL + "/login", form: form}
	if modern {
		request.header = map[string]string{"X-CSRF-Token": csrf, "Accept": "application/json"}
		// ⚠️ 关键：CSRF 令牌与 /csrf-token 下发的会话 cookie 是**绑定**的
		// （2026-10-01 在 3X-UI 3.8.5 上实测）。如果登录时不带上这个 cookie，
		// 即便 X-CSRF-Token 头完全正确也会得到 403 + 空 body，且看起来就像
		// 「密码错误」。所以必须把引导 cookie 一起送出去。
		request.cookie = bootstrapCookie
	}
	raw, statusCode, cookies, err := xuiCall(ctx, client, request)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusForbidden && len(raw) == 0 {
		return nil, errors.New("3X-UI 返回 403 且无响应体：新版面板要求 CSRF 令牌与配套会话 cookie，若都已带则是凭据被拒或面板开启了 IP 限制")
	}
	if _, err := parseXUIResponse(raw, statusCode); err != nil {
		return nil, err
	}
	session := &xuiSessionClient{client: client, csrf: csrf, modern: modern}
	// 登录成功后优先用响应里的新 cookie；个别版本不回下发，则沿用引导 cookie。
	for _, cookie := range cookies {
		if cookie.Name == "3x-ui" || cookie.Name == "session" {
			session.cookie = cookie
			break
		}
	}
	if session.cookie == nil {
		session.cookie = bootstrapCookie
	}
	if session.cookie == nil {
		return nil, errors.New("3X-UI 登录成功但没有下发会话 cookie，确认面板版本是否过旧")
	}
	return session, nil
}

// xuiFetchCSRF 探测面板是否为新版，并对新版取回 CSRF 令牌与配套的引导 cookie。
//
// 判据是行为而不是版本号：新版才有 GET /csrf-token（旧版对它返回 404 或返回
// 一段 HTML）。各 fork 的版本号字符串混乱，看行为最可靠。
//
// 注意引导 cookie：/csrf-token 会在响应里 Set-Cookie，且这个 cookie 是登录时
// 校验 CSRF 的必要条件。丢掉它会让登录必然 403 —— 这是实测踩过的坑。
func xuiFetchCSRF(ctx context.Context, client *http.Client, baseURL string) (token string, modern bool, cookie *http.Cookie, err error) {
	raw, statusCode, cookies, err := xuiCall(ctx, client, xuiRequest{method: http.MethodGet, url: baseURL + "/csrf-token"})
	if err != nil {
		return "", false, nil, err
	}
	if statusCode != http.StatusOK {
		return "", false, nil, nil
	}
	var envelope xuiEnvelope
	if json.Unmarshal(raw, &envelope) != nil || !envelope.Success {
		return "", false, nil, nil
	}
	var value string
	if json.Unmarshal(envelope.Obj, &value) != nil || value == "" {
		return "", false, nil, nil
	}
	for _, candidate := range cookies {
		if candidate.Name == "3x-ui" || candidate.Name == "session" {
			return value, true, candidate, nil
		}
	}
	return value, true, nil, nil
}

// xuiSession 是一次「登录 + 带 cookie 调 API」的封装。
// 每次操作重新登录 —— 3X-UI 的会话 cookie 有效期跟着面板设置走（默认约 1 小时），
// 服务端不持久化 cookie，重登一次的代价远小于维护一份会过期的缓存。
func xuiSession(ctx context.Context, cryptoBox *secrets.Box, baseURL, username, password string) (*xuiSessionClient, error) {
	if cryptoBox == nil {
		return nil, errors.New("服务端未配置凭据加密密钥")
	}
	return newXUISession(ctx, xuiHTTPClient(portalClient), baseURL, username, password)
}

// newXUISession 与 xuiSession 相同，但允许注入 http.Client。
//
// 存在的唯一理由是让「协议正确性」可被测试：portalClient 的拨号器会拒绝
// 一切非公网地址（生产必需的 SSRF 防护），而测试用的 httptest 服务器必然
// 监听在 127.0.0.1 —— 若不能换 client，整条协议链路就只能靠真机验证，
// 而真机验证在 CI 里跑不了。
func newXUISession(ctx context.Context, client *http.Client, baseURL, username, password string) (*xuiSessionClient, error) {
	return xuiLogin(ctx, client, baseURL, username, password)
}

type xuiSessionClient struct {
	cookie *http.Cookie
	// client 是本次会话使用的 HTTP 客户端。生产路径恒为 xuiHTTPClient(portalClient)。
	client *http.Client
	// csrf 是新版面板的 CSRF 令牌，旧版为空。所有写操作都要回填它。
	csrf string
	// modern 标记面板是新版（React / /panel/api/clients/* 那一代）。
	modern bool
}

func (c *xuiSessionClient) http() *http.Client {
	if c.client != nil {
		return c.client
	}
	return xuiHTTPClient(portalClient)
}

// writeHeader 给写操作补上 CSRF 头（仅新版需要）。
func (c *xuiSessionClient) writeHeader() map[string]string {
	if c.modern && c.csrf != "" {
		return map[string]string{"X-CSRF-Token": c.csrf, "Accept": "application/json"}
	}
	return nil
}

func (c *xuiSessionClient) get(ctx context.Context, baseURL, path string) (*xuiEnvelope, error) {
	raw, statusCode, _, err := xuiCall(ctx, c.http(), xuiRequest{
		method: http.MethodGet, url: baseURL + path, cookie: c.cookie,
	})
	if err != nil {
		return nil, err
	}
	return parseXUIResponse(raw, statusCode)
}

func (c *xuiSessionClient) post(ctx context.Context, baseURL, path string, payload url.Values) (*xuiEnvelope, error) {
	raw, statusCode, _, err := xuiCall(ctx, c.http(), xuiRequest{
		method: http.MethodPost, url: baseURL + path, form: payload,
		header: c.writeHeader(), cookie: c.cookie,
	})
	if err != nil {
		return nil, err
	}
	return parseXUIResponse(raw, statusCode)
}

// postJSON 走 JSON body 的写操作（新版 /panel/api/clients/* 用这个）。
func (c *xuiSessionClient) postJSON(ctx context.Context, baseURL, path string, payload any) (*xuiEnvelope, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, statusCode, _, err := xuiCall(ctx, c.http(), xuiRequest{
		method: http.MethodPost, url: baseURL + path, json: encoded,
		header: c.writeHeader(), cookie: c.cookie,
	})
	if err != nil {
		return nil, err
	}
	return parseXUIResponse(raw, statusCode)
}

// ---- inbound / client 数据模型（只取 UI 需要的字段，其余原样透传）----

// xuiClientSnapshot 是 inbound.settings.clients[] 里的一员。3X-UI 各协议的字段
// 名不统一（vless/vmess 用 id，trojan 用 password，ss 用 email+password），
// 这里全收进 map，UI 按需取。
type xuiClientSnapshot = map[string]any

// xuiInboundSummary 给列表页用的精简视图。完整的 inbound JSON（含
// streamSettings / sniffing / fallbacks）不透传 —— 那是对方面板网页该干的事，
// 控制台只做运维动作。
type xuiInboundSummary struct {
	ID         int                 `json:"id"`
	Port       int                 `json:"port"`
	Protocol   string              `json:"protocol"`
	Remark     string              `json:"remark"`
	Enable     bool                `json:"enable"`
	UpBytes    int64               `json:"up"`
	DownBytes  int64               `json:"down"`
	TotalBytes int64               `json:"total"`
	ExpiryTime int64               `json:"expiryTime"`
	Clients    []xuiClientSnapshot `json:"clients"`
}

// xuiAsInt / xuiAsInt64 容忍 3X-UI 数字字段的多种形态。3X-UI 把很多配置存成
// JSON 字符串（Go 侧 `encoding/json` 对 int 字段收到 "123" 会整包失败 —— 与
// provisioning 里 traffic_gb 0.0 的坑同一族），且版本间字段增减频繁，所以
// 非数字输入一律取零值而不是报错：配额字段不值得让整个列表失败。
func xuiAsInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return int(parsed)
		}
		return 0
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}

func xuiAsInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return parsed
		}
		return 0
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return parsed
		}
		return 0
	default:
		return 0
	}
}

// normalizeXUIInbound 把 3X-UI 返回的一条 inbound 归一成摘要。字段缺失时取零值，
// 不报错 —— 3X-UI 版本间字段增减频繁，严格校验反而容易在真实面板上翻车。
//
// 两代结构的差异都在这儿吸收掉：
//   - 旧版：settings 是 JSON **字符串**，客户端流量挂在每个 client 的
//     clientStats 子对象上。
//   - 新版：settings 是**对象**，客户端流量在同级的顶格 clientStats 数组里，
//     要按 email 对齐后塞回对应的 client，UI 侧才不用分版本。
func normalizeXUIInbound(item map[string]any) xuiInboundSummary {
	summary := xuiInboundSummary{Clients: []xuiClientSnapshot{}}
	summary.ID = xuiAsInt(item["id"])
	summary.Port = xuiAsInt(item["port"])
	summary.Protocol, _ = item["protocol"].(string)
	summary.Remark, _ = item["remark"].(string)
	summary.Enable, _ = item["enable"].(bool)
	summary.UpBytes = xuiAsInt64(item["up"])
	summary.DownBytes = xuiAsInt64(item["down"])
	summary.TotalBytes = xuiAsInt64(item["total"])
	summary.ExpiryTime = xuiAsInt64(item["expiryTime"])
	// settings 在旧版是 JSON 字符串、新版是对象，两种都要认。
	switch settings := item["settings"].(type) {
	case string:
		var parsed struct {
			Clients []xuiClientSnapshot `json:"clients"`
		}
		if json.Unmarshal([]byte(settings), &parsed) == nil {
			summary.Clients = parsed.Clients
		}
	case map[string]any:
		if clients, ok := settings["clients"].([]any); ok {
			for _, client := range clients {
				if asMap, ok := client.(map[string]any); ok {
					summary.Clients = append(summary.Clients, asMap)
				}
			}
		}
	}
	// 新版：把顶格 clientStats 按 email 合并进对应 client，补齐流量与在线时间，
	// 让调用侧拿到与旧版一致的形状。
	if stats, ok := item["clientStats"].([]any); ok {
		byEmail := make(map[string]map[string]any, len(stats))
		orderedStatEmails := make([]string, 0, len(stats))
		for _, entry := range stats {
			asMap, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if email, ok := asMap["email"].(string); ok && email != "" {
				if _, exists := byEmail[email]; !exists {
					orderedStatEmails = append(orderedStatEmails, email)
				}
				byEmail[email] = asMap
			}
		}
		existingEmails := make(map[string]bool, len(summary.Clients))
		for _, client := range summary.Clients {
			email, _ := client["email"].(string)
			if email != "" {
				existingEmails[email] = true
			}
			if stat, ok := byEmail[email]; ok {
				client["up"] = stat["up"]
				client["down"] = stat["down"]
				client["lastOnline"] = stat["lastOnline"]
			}
		}
		// 顶格统计里存在但 settings.clients 里没有的条目（例如只读到的孤儿统计），
		// 按 stats 原顺序 O(N) 补齐最小 client，避免 O(M*N) 扫描及 map 随机序引起界面抖动。
		for _, email := range orderedStatEmails {
			if existingEmails[email] {
				continue
			}
			stat := byEmail[email]
			summary.Clients = append(summary.Clients, map[string]any{
				"email": email, "enable": stat["enable"], "up": stat["up"], "down": stat["down"],
				"totalGB": stat["total"], "expiryTime": stat["expiryTime"], "subId": stat["subId"],
				"id": stat["uuid"], "lastOnline": stat["lastOnline"],
			})
		}
	}
	return summary
}

// xuiClientIdentity 从 client map 里取出 (email, clientId)。clientId 按协议取
// id / password / email（3X-UI 官方 README 的说明），delClient 用它。
//
// 2026-11-03：必须先看协议。3X-UI 的 DelInboundClient 对 shadowsocks 用 email
// 匹配，而 shadowsocks 客户端本身带 password —— 原来先取 password，
// 旧版面板上删 shadowsocks 客户端一律「Client Not Found」。
func xuiClientIdentity(client xuiClientSnapshot, protocol string) (email, clientID string) {
	email, _ = client["email"].(string)
	switch strings.ToLower(protocol) {
	case "shadowsocks":
		return email, email
	case "trojan":
		if password, ok := client["password"].(string); ok && password != "" {
			return email, password
		}
	}
	if id, ok := client["id"].(string); ok && id != "" {
		return email, id
	}
	if id, ok := client["id"].(float64); ok && id > 0 {
		return email, fmt.Sprintf("%.0f", id)
	}
	if password, ok := client["password"].(string); ok && password != "" {
		return email, password
	}
	return email, email
}

// xuiClientStatus 是列表页每个客户端的展开展示。
type xuiClientStatus struct {
	Email       string `json:"email"`
	ClientID    string `json:"client_id"`
	InboundID   int    `json:"inbound_id"`
	InboundName string `json:"inbound_name"`
	Protocol    string `json:"protocol"`
	Online      bool   `json:"online"`
	UpBytes     int64  `json:"up"`
	DownBytes   int64  `json:"down"`
	TotalBytes  int64  `json:"total"`
	ExpiryTime  int64  `json:"expiry_time"`
}

// ---- 数据库访问 ----

// xuiPanelTarget 是一次面板调用需要的全部信息。
type xuiPanelTarget struct {
	ID       string
	Name     string
	BaseURL  string
	Username string
	Password string
}

var (
	errXUINotFound = errors.New("3X-UI 面板不存在")
	// errXUIDatabase 包住读面板时的数据库错误：原文只进日志，不回给前端。
	errXUIDatabase = errors.New("3X-UI 面板读取失败")
)

func (s *Server) loadXUITarget(ctx context.Context, panelID string) (*xuiPanelTarget, error) {
	if !uuidPattern.MatchString(panelID) {
		return nil, errXUINotFound
	}
	var name, baseURL, username string
	var ciphertext []byte
	err := s.db.QueryRow(ctx, `SELECT name, base_url, username, password FROM xui_panels WHERE id=$1::uuid`, panelID).Scan(&name, &baseURL, &username, &ciphertext)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, errXUINotFound
		}
		return nil, fmt.Errorf("%w: %v", errXUIDatabase, err)
	}
	if s.box == nil {
		return nil, errors.New("服务端未配置凭据加密密钥")
	}
	plain, err := s.box.Open(ciphertext)
	if err != nil {
		return nil, errors.New("3X-UI 凭据解密失败")
	}
	return &xuiPanelTarget{ID: panelID, Name: name, BaseURL: baseURL, Username: username, Password: string(plain)}, nil
}

func (s *Server) writeXUITargetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errXUINotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errXUIDatabase):
		// 2026-11-03：数据库错误（例如畸形 id 触发的 22P02）原来以 502 原样回给前端，
		// 暴露表结构。与 writePortalTargetError 一致：原文只进日志。
		s.logger.Warn("xui panel: load target failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 3X-UI 面板失败，请查看服务端日志"})
	default:
		s.logger.Warn("xui panel: request failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

// recordXUIStatus 把一次探测结果写回面板行，供列表页显示「最近状态」。
// 探测路径失败不应阻塞主流程：写库失败只记日志。
//
// 2026-11-03：探测失败时只写状态与错误，保留上次成功的计数。原来失败一律写 0，
// 面板一次连不上就把「入站 / 客户端 / 在线」全部清零，与「失败不覆盖上次成功结果」的约定相反。
// online < 0 表示「这次没取到在线列表」：保留上次的在线数（2026-11-06）。
func (s *Server) recordXUIStatus(ctx context.Context, panelID, status, errMsg string, inbounds, clients, online int) {
	if _, err := s.db.Exec(ctx, `UPDATE xui_panels SET
last_status=$1, last_error=$2, last_checked_at=now(),
inbounds_count=CASE WHEN $1='ok' THEN $3 ELSE inbounds_count END,
clients_count=CASE WHEN $1='ok' THEN $4 ELSE clients_count END,
online_count=CASE WHEN $1='ok' AND $5 >= 0 THEN $5 ELSE online_count END, updated_at=now()
WHERE id=$6::uuid`, status, errMsg, inbounds, clients, online, panelID); err != nil {
		s.logger.Warn("xui panel: status writeback failed", "error", err)
	}
}

// normalizeXUIBaseURL 校验并归一 3X-UI 面板地址。
//
// 与 normalizePortalURL 不同：3X-UI 面板经常挂在反代的子路径下
// （如 https://host/xui/），此时 API 前缀是 /<subpath>/panel/api/...。
// 所以这里**允许单段路径**，把调用侧要用的完整前缀拼好存下来。
func normalizeXUIBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("请填写 3X-UI 面板地址")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New("地址不合法")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("只支持 http/https")
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return "", errors.New("地址缺少主机名")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("地址里不要带账号或查询参数")
	}
	// 路径最多一段（反代子路径），每段只允许安全字符 —— 它会被拼进 URL，且禁止 . / .. 路径穿越段。
	if parsed.Path != "" && parsed.Path != "/" {
		trimmed := strings.Trim(parsed.Path, "/")
		for _, segment := range strings.Split(trimmed, "/") {
			if segment == "" || segment == "." || segment == ".." || !safeUnitName.MatchString(segment) {
				return "", errors.New("子路径只允许字母、数字、点、下划线、连字符")
			}
		}
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	return parsed.String(), nil
}

// xuiAPIPrefix 返回该面板 API 的完整前缀：base（含可选反代子路径）+ /panel/api。
func xuiAPIPrefix(baseURL string) string {
	return strings.TrimSuffix(baseURL, "/") + "/panel/api"
}
