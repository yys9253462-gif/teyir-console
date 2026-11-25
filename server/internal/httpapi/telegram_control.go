package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodeops/server/internal/ippurity"
)

// Telegram 复用现有 HTTP 路由、管理员会话校验、限流、主机授权和审计。
// 只有已配置的私聊本人可控制；群聊不升级为管理员。所有非 GET 请求
// 和有副作用的 GET 均先生成两分钟单次确认，不直接执行消息中的写操作。
type telegramRoute struct{ Pattern, Group string }
type telegramAction struct {
	Method, Path string
	Body         json.RawMessage
}
type telegramPending struct {
	Action              telegramAction
	Chat, Sender, Admin string
	Until               time.Time
}
type telegramDraft struct {
	Pattern, Path string
	Until         time.Time
}
type telegramControlState struct {
	sync.Mutex
	Wizards map[string]*telegramWizard
	Routes  []telegramRoute
	Pending map[string]telegramPending
	Drafts  map[string]telegramDraft
	// Alerts 是「告警处置按钮」的待执行动作登记表（2026-10-24 加）。
	// 键是随机 nonce，值是按钮对应的面板动作。
	Alerts map[string]telegramAlertAction
}

func telegramGroup(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/nodes"), strings.HasPrefix(path, "/api/categories"), strings.HasPrefix(path, "/api/portal-update"), strings.HasPrefix(path, "/api/sub-ids"):
		return "nodes"
	case strings.HasPrefix(path, "/api/servers"), strings.HasPrefix(path, "/api/metrics"), strings.HasPrefix(path, "/api/install-jobs"):
		return "servers"
	case strings.HasPrefix(path, "/api/subscriptions"):
		return "subs"
	case strings.HasPrefix(path, "/api/xui"):
		return "xui"
	case strings.HasPrefix(path, "/api/tasks"), strings.HasPrefix(path, "/api/orphans"), strings.HasPrefix(path, "/api/schedules"):
		return "tasks"
	case strings.HasPrefix(path, "/api/alerts"):
		return "alerts"
	case strings.HasPrefix(path, "/api/monitoring"), strings.HasPrefix(path, "/api/speedtest"), strings.HasPrefix(path, "/api/ip-purity"), strings.HasPrefix(path, "/api/unlock"), strings.HasPrefix(path, "/api/block-checks"), strings.HasPrefix(path, "/api/reports"):
		return "reports"
	default:
		return "settings"
	}
}

// panelRoute 仅用于 authenticated 路由注册。菜单自动随面板路由扩展，避免
// 两套管理代码分叉。公开商城/订阅/登录路由不加入机器人入口。
func (s *Server) panelRoute(pattern string, handler http.HandlerFunc) {
	if !strings.HasPrefix(pattern, "GET ") && strings.Contains(pattern, "/api/subscriptions/{id}") {
		handler = s.subscriptionMutation(handler)
	}
	s.mux.HandleFunc(pattern, handler)
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) == 2 && strings.HasPrefix(parts[1], "/api/") && !telegramForbidden(pattern) {
		s.telegramControl.Routes = append(s.telegramControl.Routes, telegramRoute{pattern, telegramGroup(parts[1])})
	}
}

// telegramForbidden 列出**永远不能**经 Telegram 调用的面板接口（2026-10-16 审查后加）。
//
// 机器人的二次确认挡不住「聊天本身已经被别人拿到」—— Telegram 账号被盗、手机没锁。
// 那种情况下，这些接口能让对方建一个隐藏的管理员、改掉机器人绑定、换掉商城密钥，
// 在你改了面板密码之后依然留着后门。所以账号、凭据、密钥、机器人绑定、
// 终端与文件这几类一律只能在网页面板（需要登录密码）里操作。
// 不在这里的接口（节点、主机、订阅的日常运维）照常可用，仍需确认。
var telegramForbiddenPrefixes = []string{
	"/api/users",                // 建 / 删账号、改角色、重置他人密码
	"/api/auth/",                // 改自己密码、签发终端会话
	"/api/settings/purity-keys", // 第三方 API 密钥（含机器人 Token 本身）
	"/api/settings/telegram",    // 机器人绑定的 Chat ID
	"/api/settings/store-signature",
	"/api/settings/delivery-domain", // 交付域名：改了会把新订单的链接指到别处
	"/api/subscriptions/store-key",  // 轮换商城对接密钥 = 商城立即无法下单
	"/api/alerts/webhook",           // 告警外发地址（可被改去别处收告警）
	// 2026-11-06：跨节点账号清单（每台节点上所有 sub_* 账号）。回执只展示白名单字段，
	// 但没必要让这个端点出现在聊天菜单里 —— 它的用途是脚本/审计，不是手机上点一下。
	"/api/sub-ids",
}

var telegramForbiddenContains = []string{
	"/credential",         // 上传 / 删除主机 SSH 凭据
	"/fingerprint/accept", // 接受新的 SSH 主机指纹
	"/write-ops",          // 打开主机写操作授权
	"/terminal",           // 远程终端
	"/files",              // 远程文件浏览 / 预览
}

// telegramForbiddenPatterns 是按「方法 + 路由模式」精确禁止的接口（2026-11-04）。
// 它们写入的是节点 / 面板的**访问凭据**：在机器人里能改，就能把节点的 api_url 指到自己的机器，
// 此后开户把买家密码直接发过去；而且这些表单要在聊天里明文输入 api_key / 面板密码，
// 会永久留在 Telegram 聊天记录里。同一路径的 GET（列表、详情）照常可用。
var telegramForbiddenPatterns = map[string]bool{
	"POST /api/nodes":              true, // 接入节点（api_url + api_key）
	"POST /api/nodes/preview":      true, // 用 api_key 预探
	"POST /api/nodes/{id}/api":     true, // 换绑节点 API（url / api_key / basic_password）
	"POST /api/xui/panels":         true, // 添加 3X-UI 面板（面板账号密码）
	"POST /api/xui/panels/preview": true, // 用面板账号密码预探
	// 2026-11-06：批量门户更新会在**每台节点上跑 root 脚本并重启门户**，进度与每台结果只在面板页面上
	// 看得全。从聊天里发起，操作者拿不到逐台结果，也无从判断哪台半更新。查看进度（GET）照常可用。
	"POST /api/portal-update-batches": true,
}

// telegramTouchesSSHEndpoint 判断这次调用是否在改主机的 SSH 地址 / 端口 / 用户或重置主机指纹（2026-11-04）。
//
// 这几个字段一改，updateServer 就清空已登记的主机指纹，采集器下一轮按「首次信任」连过去，
// 把已存的 SSH 凭据（含明文口令）发给新地址 —— 与被禁止的 /credential、/fingerprint/accept 是同一件事，
// 只是换了个入口。机器人以管理员身份执行，updateServer 里的「仅管理员」挡不住它。
// 不整条禁止 PATCH /api/servers/{id}：改名称、地区、标签在机器人里仍然有用。
func telegramTouchesSSHEndpoint(s *Server, a telegramAction) bool {
	r, err := http.NewRequest(a.Method, a.Path, nil)
	if err != nil {
		return true
	}
	if _, pattern := s.mux.Handler(r); pattern != "PATCH /api/servers/{id}" {
		return false
	}
	// 2026-11-06：按 updateServer 自己的解码方式判断。encoding/json 解进结构体时字段名不分大小写：
	// 原来按小写键查 map，{"Host":"1.2.3.4"} 或 {"host":null,"HOST":"1.2.3.4"} 就绕过了守卫，
	// updateServer 却照样改地址、清指纹，采集器随即把 SSH 凭据发给新地址。
	var body struct {
		Host      *string `json:"host"`
		SSHPort   *int    `json:"ssh_port"`
		SSHUser   *string `json:"ssh_user"`
		ResetHost bool    `json:"reset_host_key"`
	}
	if len(a.Body) > 0 && json.Unmarshal(a.Body, &body) != nil {
		return true // 读不懂的请求体不放行
	}
	return body.Host != nil || body.SSHPort != nil || body.SSHUser != nil || body.ResetHost
}

// telegramRunTimeout 必须大于任何经机器人可达的接口自身的处理预算。
// 2026-11-06：最慢的可达接口是 portal-action 的重型 root 操作（install-* / do-upgrade 620 秒、
// manage-amneziawg 500 秒），外加最多 15 秒等节点写锁与凭据加载。原来按 120 秒算（150 秒），
// 机器人会在节点已经成功后回「未完成」、不写审计，还提前释放写锁。
const telegramRunTimeout = portalHeavyInstallTimeout + 60*time.Second

func telegramForbidden(pattern string) bool {
	if telegramForbiddenPatterns[pattern] {
		return true
	}
	parts := strings.SplitN(pattern, " ", 2)
	path := pattern
	if len(parts) == 2 {
		path = parts[1]
	}
	for _, p := range telegramForbiddenPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	for _, c := range telegramForbiddenContains {
		if strings.Contains(path, c) {
			return true
		}
	}
	return false
}

var telegramGroups = []struct{ ID, Label string }{
	{"nodes", "🌐 节点管理"}, {"servers", "🖥 主机管理"}, {"subs", "🎫 订阅管理"}, {"xui", "🔌 3X-UI"},
	{"tasks", "🛠 任务与巡检"}, {"alerts", "🚨 告警管理"}, {"reports", "📊 检测与报表"}, {"settings", "⚙️ 系统设置"},
}

// telegramPanelURL 返回可配置的面板外部地址；未配置时返回空串，避免生成无效外链。
func telegramPanelURL() string {
	for _, key := range []string{"PANEL_PUBLIC_URL", "SUBSCRIPTION_PUBLIC_BASE_URL"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return strings.TrimRight(raw, "/")
		}
	}
	return ""
}

// banWebhookBaseURL 读取宿主机封禁联动接口地址；未配置时返回空串。
func banWebhookBaseURL() string {
	raw := strings.TrimSpace(os.Getenv("BAN_WEBHOOK_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.TrimRight(raw, "/")
}

func tgButton(text, data string) map[string]string {
	return map[string]string{"text": text, "callback_data": data}
}
func tgCommand(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(strings.SplitN(f[0], "@", 2)[0])
}
func telegramPrivateOwner(configured, chat, sender string) bool {
	return configured != "" && configured == chat && chat == sender && !strings.HasPrefix(chat, "-") && chat != "0"
}
func (s *Server) telegramRich(ctx context.Context, chat, text string, rows [][]map[string]string) error {
	payload := map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}
	if len(rows) > 0 {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	return telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "sendMessage", payload, nil)
}
func (s *Server) telegramMenu(ctx context.Context, chat string) error {
	return s.telegramBottomMenu(ctx, chat)
}
func (s *Server) telegramRouteMenu(ctx context.Context, chat, group string, page int) {
	routes := []int{}
	for i, r := range s.telegramControl.Routes {
		if r.Group == group {
			routes = append(routes, i)
		}
	}
	if page < 0 || page > len(routes)/8 || page*8 >= len(routes) {
		page = 0
	}
	rows := [][]map[string]string{}
	end := min((page+1)*8, len(routes))
	var text strings.Builder
	text.WriteString("请选择功能，随后按提示点选或填写参数：\n")
	for _, idx := range routes[page*8 : end] {
		r := s.telegramControl.Routes[idx]
		fmt.Fprintf(&text, "%d. %s\n", idx+1, telegramRouteTitle(r.Pattern))
		rows = append(rows, []map[string]string{tgButton(telegramRouteTitle(r.Pattern), "tg:r:"+strconv.Itoa(idx)+":"+telegramRouteTag(r.Pattern))})
	}
	nav := []map[string]string{tgButton("⬅️ 主菜单", "tg:menu")}
	if page > 0 {
		nav = append(nav, tgButton("上一页", fmt.Sprintf("tg:a:%s:%d", group, page-1)))
	}
	if end < len(routes) {
		nav = append(nav, tgButton("下一页", fmt.Sprintf("tg:a:%s:%d", group, page+1)))
	}
	rows = append(rows, nav)
	_ = s.telegramRich(ctx, chat, text.String(), rows)
}

var telegramPathVars = regexp.MustCompile(`\{([^{}]+)\}`)

func telegramFillPath(pattern string, params map[string]string) (string, error) {
	path := pattern
	for _, m := range telegramPathVars.FindAllStringSubmatch(pattern, -1) {
		v := params[m[1]]
		if v == "" || strings.ContainsAny(v, "/\\?#\r\n") || v == "." || v == ".." {
			return "", fmt.Errorf("路径参数 %s 缺失或无效", m[1])
		}
		path = strings.ReplaceAll(path, m[0], url.PathEscape(v))
	}
	return path, nil
}
func parseTelegramAPI(text string) (telegramAction, error) {
	f := strings.Fields(text)
	if len(f) < 3 {
		return telegramAction{}, fmt.Errorf("格式：/api METHOD /api/路径 {JSON请求体}")
	}
	method := strings.ToUpper(f[1])
	path := f[2]
	if method != "GET" && method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" {
		return telegramAction{}, fmt.Errorf("不支持此 HTTP 方法")
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/api/") || strings.ContainsAny(path, "\\\r\n#") || strings.Contains(path, "{") {
		return telegramAction{}, fmt.Errorf("请输入完整的面板 /api/ 路径，不接受外部 URL")
	}
	// 只分割命令前三段，JSON 中的空格原样保留。
	rest := strings.TrimSpace(text)
	for i := 0; i < 3; i++ {
		j := strings.IndexAny(rest, " \t\r\n")
		if j < 0 {
			rest = ""
			break
		}
		rest = strings.TrimSpace(rest[j:])
	}
	var body json.RawMessage
	if rest != "" {
		if len(rest) > 12000 || !json.Valid([]byte(rest)) {
			return telegramAction{}, fmt.Errorf("请求体必须是有效 JSON（最多 12KB）")
		}
		body = json.RawMessage(rest)
	}
	return telegramAction{method, path, body}, nil
}
func telegramNeedsConfirm(a telegramAction) bool {
	// 某些历史 GET 会刷新节点、探测主机、签发临时终端票据；保守确认。
	if a.Method != "GET" {
		return true
	}
	// 2026-11-04：按**解码后**的路径判断。原来直接比原始字符串，而路由匹配用的是解码后的路径 ——
	// GET /api/nodes/<id>/%61pi 照样命中会写库的 refreshNodeAPI，.../pr%6Fbe 命中出口探测，都不弹确认。
	// 解码失败（畸形百分号）一律要求确认。
	u, err := url.ParseRequestURI(a.Path)
	if err != nil {
		return true
	}
	path := u.Path
	if strings.HasSuffix(path, "/api") || strings.Contains(path, "/probe") || strings.Contains(path, "/terminal") || strings.Contains(path, "/download") {
		return true
	}
	// refresh=1 会跳过缓存、对每台节点发真实请求（能力总览、门户版本检查）——同样算有副作用。
	return u.Query().Get("refresh") != ""
}
func (s *Server) telegramAllowed(a telegramAction) bool {
	r, err := http.NewRequest(a.Method, a.Path, nil)
	if err != nil {
		return false
	}
	_, pattern := s.mux.Handler(r)
	for _, v := range s.telegramControl.Routes {
		if v.Pattern == pattern {
			return true
		}
	}
	return false
}
func (s *Server) telegramAdmin(ctx context.Context) (string, string, int64, error) {
	var id, email string
	var ver int64
	err := s.db.QueryRow(ctx, `SELECT id::text,email,token_version FROM users WHERE role='admin' ORDER BY created_at,id LIMIT 1`).Scan(&id, &email, &ver)
	return id, email, ver, err
}
func (s *Server) telegramRun(ctx context.Context, chat, sender string, a telegramAction, confirmed bool) {
	if !telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
		_ = s.sendTelegram(ctx, chat, "控制操作只允许已绑定的私人 Chat ID 本人。群聊仅提供原有只读查询；请在系统设置中绑定你的机器人私聊 ID。")
		return
	}
	if !s.telegramAllowed(a) {
		_ = s.sendTelegram(ctx, chat, "该路径不是已登记的面板管理接口，请从 /menu 选择。")
		return
	}
	if telegramTouchesSSHEndpoint(s, a) {
		_ = s.sendTelegram(ctx, chat, "修改主机 SSH 地址、端口、用户或重置主机指纹只能在网页工作台操作；机器人里只能改名称、地区、标签等字段。")
		return
	}
	admin, email, ver, err := s.telegramAdmin(ctx)
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "无法取得有效面板管理员，未执行。")
		return
	}
	if telegramNeedsConfirm(a) && !confirmed {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			return
		}
		nonce := hex.EncodeToString(buf)
		st := &s.telegramControl
		st.Lock()
		if st.Pending == nil {
			st.Pending = map[string]telegramPending{}
		}
		for k, v := range st.Pending {
			if time.Now().After(v.Until) || v.Chat == chat {
				delete(st.Pending, k)
			}
		}
		st.Pending[nonce] = telegramPending{a, chat, sender, admin, time.Now().Add(2 * time.Minute)}
		st.Unlock()
		preview := telegramWizardPreview(a.Body)
		if len(preview) > 1500 {
			preview = string([]rune(preview)[:min(1500, len([]rune(preview)))]) + "…"
		}
		err = s.telegramRich(ctx, chat, "⚠️ 确认操作（2 分钟内有效，仅执行一次）\n"+s.telegramActionText(ctx, a)+"\n"+preview, [][]map[string]string{{tgButton("✅ 确认执行", "tg:yes:"+nonce), tgButton("取消", "tg:no:"+nonce)}})
		if err != nil {
			st.Lock()
			delete(st.Pending, nonce)
			st.Unlock()
			noteTelegramError(err.Error())
		}
		return
	}
	token, err := s.signer.IssueVersion(admin, email, "admin", ver)
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "创建面板操作会话失败，未执行。")
		return
	}
	// 2026-11-04：原来 90 秒，比门户写操作的预算（portalActionTimeout 120 秒，外加最多 15 秒等节点写锁）短：
	// 更新门户 / 安装 WARP 这类 95 秒才完成的操作，节点上已经成功，机器人却回「未完成」、不写业务审计，
	// 还提前释放了节点写锁。
	runCtx, cancel := context.WithTimeout(ctx, telegramRunTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(runCtx, a.Method, a.Path, strings.NewReader(string(a.Body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "Teyir-Telegram-Control")
	req.RemoteAddr = "127.0.0.1:0"
	if confirmed {
		req.Header.Set("X-Confirm-Dangerous", "yes")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// handlers 自身写原有业务审计；这里再记 Telegram 入口（不写请求体/凭据）。
	if claims, err := s.authenticate(req); err == nil {
		auditReq := req.WithContext(context.WithValue(req.Context(), claimsKey, claims))
		s.audit(auditReq.Context(), auditReq, "telegram_control", "telegram", "", map[string]any{"telegram_chat": chat, "method": a.Method, "path": strings.SplitN(a.Path, "?", 2)[0], "http_status": rec.Code, "telegram_user": sender})
	}
	result := telegramFriendlyResult(a, rec.Code, rec.Body.Bytes())
	// 2026-11-04：有些接口用 200 + 结构化字段报「部分节点没生效」（停用订阅的 node_state_ok=false、
	// 对账的 failed_nodes>0）。原来只认 207，这类结果照样打「✅」，而结果里的失败字段又不在展示白名单里 ——
	// 机器人回复「✅ 已停用」，实际有节点上的买家仍然能连。
	partial, partialNote := telegramPartialOutcome(rec.Body.Bytes())
	if rec.Code == 207 || partial {
		head := "部分操作未完成，请在面板核对详情。"
		if partialNote != "" {
			head += "\n" + partialNote
		}
		result = head + "\n" + result
	}
	if rec.Code == http.StatusSwitchingProtocols || strings.Contains(rec.Header().Get("Content-Type"), "octet-stream") {
		result = "该接口返回终端流或二进制文件，请使用网页工作台。"
	}
	prefix := s.telegramActionText(ctx, a) + "\n"
	if rec.Code >= 200 && rec.Code < 300 && rec.Code != 207 && rec.Code != 202 && !partial {
		prefix = "✅ " + prefix
	} else {
		prefix = "⚠️ " + prefix
	}
	runes := []rune(result)
	if len(runes) > 10000 {
		runes = runes[:10000]
		runes = append(runes, []rune("\n结果过长，仅显示前 10000 字符，请在面板查看完整结果。")...)
	}
	for start := 0; start < len(runes) || start == 0; start += 2800 {
		end := min(start+2800, len(runes))
		chunk := string(runes[start:end])
		if start == 0 {
			chunk = prefix + chunk
		}
		if err := s.sendTelegram(ctx, chat, chunk); err != nil {
			noteTelegramError(err.Error())
			break
		}
		if end == len(runes) {
			break
		}
	}
}
func telegramCleanJSON(raw []byte) string {
	if len(raw) == 0 {
		return "{}"
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "（非 JSON 响应，避免把文件或凭据内容发送到聊天，请到面板查看）"
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				n := strings.ToLower(k)
				if strings.Contains(n, "password") || strings.Contains(n, "secret") || strings.Contains(n, "token") || n == "api_key" || strings.Contains(n, "private_key") || n == "authorization" || n == "cookie" {
					x[k] = "[隐藏]"
				} else {
					x[k] = clean(e)
				}
			}
		case []any:
			for i, e := range x {
				x[i] = clean(e)
			}
		}
		return v
	}
	b, _ := json.MarshalIndent(clean(value), "", "  ")
	return string(b)
}
func (s *Server) telegramControlMessage(ctx context.Context, chat, sender, text string) bool {
	if s.telegramFriendlyText(ctx, chat, sender, text) {
		return true
	}
	cmd := tgCommand(text)
	if cmd == "/hide" {
		s.telegramHideMenu(ctx, chat)
		return true
	}
	if cmd == "/menu" || cmd == "/start" {
		s.telegramMenu(ctx, chat)
		return true
	}
	if cmd == "/help" {
		_ = s.sendTelegram(ctx, chat, "点击输入框下方中文按钮即可使用。节点、主机和订阅先点名称，再选操作；更改操作必须确认。\n/menu 展开底部菜单，/cancel 取消待执行操作。")
		return true
	}
	if cmd == "/cancel" {
		s.telegramControl.Lock()
		delete(s.telegramControl.Drafts, chat)
		delete(s.telegramControl.Wizards, chat)
		for k, p := range s.telegramControl.Pending {
			if p.Chat == chat {
				delete(s.telegramControl.Pending, k)
			}
		}
		s.telegramControl.Unlock()
		_ = s.sendTelegram(ctx, chat, "已取消待输入及待确认操作。")
		return true
	}
	// 封禁管理（列表 / 查 IP），由宿主机封禁联动服务（BAN_WEBHOOK_URL）完成。
	if cmd == "/bans" || text == "封禁列表" || text == "封禁管理" {
		if telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
			s.telegramBanQuery(ctx, chat, "/list", "")
		}
		return true
	}
	if cmd == "/ip" {
		if telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
			f := strings.Fields(text)
			if len(f) < 2 || net.ParseIP(f[1]) == nil {
				_ = s.sendTelegram(ctx, chat, "用法：/ip 1.2.3.4（也可以直接发送一个 IP 地址）")
			} else {
				s.telegramBanQuery(ctx, chat, "/ipinfo", f[1])
			}
		}
		return true
	}
	if cmd == "/servers" || cmd == "/subscriptions" {
		p := "/api/servers"
		if cmd == "/subscriptions" {
			p = "/api/subscriptions"
		}
		s.telegramRun(ctx, chat, sender, telegramAction{Method: "GET", Path: p}, false)
		return true
	}
	if cmd == "/api" {
		s.telegramControl.Lock()
		delete(s.telegramControl.Drafts, chat)
		s.telegramControl.Unlock()
		a, err := parseTelegramAPI(text)
		if err != nil {
			_ = s.sendTelegram(ctx, chat, err.Error())
		} else {
			s.telegramRun(ctx, chat, sender, a, false)
		}
		return true
	}
	if (text == "/skip" || !strings.HasPrefix(text, "/")) && s.telegramWizardInput(ctx, chat, text) {
		return true
	}
	if strings.HasPrefix(text, "/") {
		return false
	}
	st := &s.telegramControl
	st.Lock()
	draft, ok := st.Drafts[chat]
	st.Unlock()
	if !ok {
		// 没有待输入的草稿时，直接发一个 IP 地址 = 查询这个 IP。
		if ip := net.ParseIP(strings.TrimSpace(text)); ip != nil && telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
			s.telegramBanQuery(ctx, chat, "/ipinfo", ip.String())
			return true
		}
		return false
	}
	if !telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
		return true
	}
	if time.Now().After(draft.Until) {
		st.Lock()
		delete(st.Drafts, chat)
		st.Unlock()
		_ = s.sendTelegram(ctx, chat, "输入已过期，请重新从菜单选择。")
		return true
	}
	parts := strings.SplitN(draft.Pattern, " ", 2)
	if draft.Path == "" {
		var params map[string]string
		if json.Unmarshal([]byte(text), &params) != nil {
			_ = s.sendTelegram(ctx, chat, "请发送路径参数 JSON，例如 {\"id\":\"节点UUID\"}。/cancel 取消。")
			return true
		}
		p, err := telegramFillPath(parts[1], params)
		if err != nil {
			_ = s.sendTelegram(ctx, chat, err.Error())
			return true
		}
		draft.Path = p
		if parts[0] == "GET" || parts[0] == "DELETE" {
			st.Lock()
			delete(st.Drafts, chat)
			st.Unlock()
			s.telegramRun(ctx, chat, sender, telegramAction{Method: parts[0], Path: p}, false)
			return true
		}
		st.Lock()
		st.Drafts[chat] = draft
		st.Unlock()
		_ = s.sendTelegram(ctx, chat, "路径已选择："+draft.Path+"\n请发送请求体 JSON；无需参数则发送 {}。/cancel 取消。")
		return true
	}
	if len(text) > 12000 || !json.Valid([]byte(text)) {
		_ = s.sendTelegram(ctx, chat, "请求体必须是有效 JSON（最多 12KB）。")
		return true
	}
	st.Lock()
	delete(st.Drafts, chat)
	st.Unlock()
	s.telegramRun(ctx, chat, sender, telegramAction{parts[0], draft.Path, json.RawMessage(text)}, false)
	return true
}
func (s *Server) telegramCallback(ctx context.Context, id, chat, sender, data string) {
	token := s.purityKeys.Get(ippurity.KeyTelegram)
	_ = telegramCall(ctx, token, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
	if !telegramPrivateOwner(s.telegramChatID(ctx), chat, sender) {
		return
	}
	if s.telegramWizardCallback(ctx, chat, data) {
		return
	}
	if s.telegramFriendlyCallback(ctx, chat, sender, data) {
		return
	}
	switch {
	case strings.HasPrefix(data, "tg:al:"):
		// 告警处置按钮（2026-10-24 加）。
		// 取出登记的动作后交给 telegramRun 生成确认卡 —— 不直接执行。
		s.telegramAlertCallback(ctx, chat, sender, data)
		return
	case strings.HasPrefix(data, "tg:banip:"), strings.HasPrefix(data, "tg:unbanip:"):
		// 2026-11-03：封禁 / 解封改为两步确认。原来点一下就执行，旧消息里误触一次
		// 「🔓 解除封禁」就把攻击者 IP 永久放行，且 audit_logs 里没有任何记录。
		isUnban := strings.HasPrefix(data, "tg:unbanip:")
		ip := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(data, "tg:banip:"), "tg:unbanip:"))
		s.telegramBanConfirm(ctx, chat, ip, isUnban, time.Now())
		return
	case strings.HasPrefix(data, "tg:bnok:"), strings.HasPrefix(data, "tg:ubok:"):
		isUnban := strings.HasPrefix(data, "tg:ubok:")
		ip, ok := parseBanConfirm(data, time.Now())
		// 2026-11-04：确认按钮只能用一次。原来两分钟内每点一次就执行一次、记一条审计；
		// 双击、或重启后 Telegram 重发同一批更新时，封禁 / 解封会被重复下发。
		if ok && !consumeBanConfirm(data, time.Now()) {
			_ = s.sendTelegram(ctx, chat, "这个确认已经用过了；如需再次操作，请重新点击封禁 / 解封按钮。")
			return
		}
		if !ok {
			_ = s.sendTelegram(ctx, chat, "⌛ 确认已过期或无效，请重新点击封禁 / 解封按钮。")
			return
		}
		if !isUnban && isPrivateOrLoopbackIP(ip) {
			_ = s.sendTelegram(ctx, chat, "❌ "+ip+" 是内网或回环地址，封禁它会切断面板自身的流量，已拒绝。")
			return
		}
		if s.telegramBanIPAction(ctx, chat, ip, isUnban) {
			s.auditTelegramBan(ctx, ip, isUnban)
		}
		return
	case data == "tg:menu":
		s.telegramMenu(ctx, chat)
	case data == "tg:status":
		s.handleTelegramMessage(ctx, chat, "/status")
	case data == "tg:help":
		s.telegramControlMessage(ctx, chat, sender, "/help")
	case strings.HasPrefix(data, "tg:g:"):
		p := strings.Split(data, ":")
		if len(p) != 4 {
			return
		}
		names := map[string]string{"nodes": "🌐 节点管理", "servers": "🖥 主机管理", "subs": "🎫 订阅管理", "xui": "🔌 3X-UI面板", "tasks": "🛠 任务与巡检", "alerts": "🚨 告警中心", "reports": "🛡 检测与报表", "settings": "⚙️ 系统设置"}
		if name := names[p[2]]; name != "" {
			s.telegramFriendlyText(ctx, chat, sender, name)
		}

	case strings.HasPrefix(data, "tg:a:"):
		p := strings.Split(data, ":")
		if len(p) != 4 {
			return
		}
		page, err := strconv.Atoi(p[3])
		if err == nil {
			s.telegramRouteMenu(ctx, chat, p[2], page)
		}
	case strings.HasPrefix(data, "tg:r:"):
		// 2026-11-06：按钮带上接口的短哈希。下标是排序后路由表里的位置，部署时增删一条路由就整体移位：
		// 上周发出的「查看 · 节点」按钮会打开相邻的另一个接口（只读接口不经确认，直接执行）。
		// 哈希对不上（含旧版只有下标的按钮）一律请用户重新打开菜单。
		idxRaw, tag, _ := strings.Cut(strings.TrimPrefix(data, "tg:r:"), ":")
		index, err := strconv.Atoi(idxRaw)
		if err != nil || index < 0 || index >= len(s.telegramControl.Routes) || tag != telegramRouteTag(s.telegramControl.Routes[index].Pattern) {
			_ = s.sendTelegram(ctx, chat, "菜单已更新，这个按钮已失效。请发送 /menu 重新打开菜单。")
			return
		}
		s.telegramWizardStart(ctx, chat, index)

	case strings.HasPrefix(data, "tg:yes:"), strings.HasPrefix(data, "tg:no:"):
		nonce := strings.SplitN(data, ":", 3)[2]
		p, ok := s.telegramControl.takePending(nonce, chat, sender, time.Now())
		if !ok {
			_ = s.sendTelegram(ctx, chat, "确认已失效、已执行或不属于当前用户。")
			return
		}

		if strings.HasPrefix(data, "tg:no:") {
			_ = s.sendTelegram(ctx, chat, "已取消。")
			return
		}
		admin, _, _, err := s.telegramAdmin(ctx)
		if err != nil || admin != p.Admin {
			_ = s.sendTelegram(ctx, chat, "管理员身份已变化，请重新发起操作。")
			return
		}
		s.telegramRun(ctx, chat, sender, p.Action, true)
	}
}
func telegramCommands() []map[string]string {
	return []map[string]string{
		{"command": "menu", "description": "全部功能控制菜单"}, {"command": "bans", "description": "封禁列表"}, {"command": "ip", "description": "查询 IP：/ip 1.2.3.4"}, {"command": "status", "description": "面板总览"}, {"command": "nodes", "description": "节点状态"}, {"command": "servers", "description": "主机列表"}, {"command": "subscriptions", "description": "订阅列表"}, {"command": "alerts", "description": "未处理告警"}, {"command": "api", "description": "调用面板接口，写操作需确认"}, {"command": "cancel", "description": "取消待输入/待确认操作"}, {"command": "help", "description": "使用帮助"},
	}
}
func (s *Server) telegramInstallMenu(ctx context.Context, token string) error {
	if err := telegramCall(ctx, token, "setMyCommands", map[string]any{"commands": telegramCommands()}, nil); err != nil {
		return err
	}
	return telegramCall(ctx, token, "setChatMenuButton", map[string]any{"menu_button": map[string]string{"type": "commands"}}, nil)
}

// 保持菜单顺序稳定，不依赖 map 遍历顺序。
func (s *Server) sortTelegramRoutes() {
	sort.Slice(s.telegramControl.Routes, func(i, j int) bool { return s.telegramControl.Routes[i].Pattern < s.telegramControl.Routes[j].Pattern })
}

// 用中文标示常用动作；完整 HTTP 路径同时显示在菜单正文，避免同名动作歧义。
func telegramRouteTitle(pattern string) string {
	p := strings.SplitN(pattern, " ", 2)
	if len(p) != 2 {
		return pattern
	}
	names := map[string]string{
		"nodes": "节点", "servers": "主机", "subscriptions": "订阅", "categories": "分类", "users": "节点账号", "proxy-services": "代理服务", "overview": "节点总览", "api": "接入信息", "portal-status": "门户状态", "portal-action": "门户操作（BBR/WARP/Reality/重启等）", "ip-purity": "IP纯净度", "unlock": "解锁检测", "refresh": "刷新检测", "refresh-all": "批量检测", "outbounds": "出口设置", "probe": "出口探测", "install-hy2": "一键安装节点", "migrate": "迁移节点", "test-ssh": "SSH连接测试", "billing": "到期与成本", "change-ip": "更换IP", "tasks": "运维任务", "retry": "重试", "alerts": "告警", "acknowledge": "确认告警", "resolve": "结案", "schedules": "定时巡检", "run": "立即运行", "auto-hide": "故障自动下线", "profit": "收益报表", "sharing": "共享检测", "logins": "登录记录", "telegram": "Telegram设置", "purity-keys": "集成密钥", "store-signature": "商城签名", "traffic-limit": "流量限额", "renew": "续期", "status": "启用/停用", "reconcile": "账号对账", "reconcile-all": "全量账号对账", "rotate": "换新链接", "reset-ips": "重置设备IP", "portal-update-batches": "门户批量更新", "portal-update-status": "门户版本检查", "orphans": "待补删账号", "panels": "3X-UI面板", "inbounds": "入站", "clients": "客户账号", "credential": "SSH凭据", "host-key": "SSH指纹", "files": "文件列表", "preview": "预览", "containers": "容器", "compose": "Compose", "presets": "运维预设", "security": "安全检查", "audit": "审计记录", "runtime": "运行状态", "settings": "系统设置", "notifications": "通知配置",
	}
	segments := strings.Split(strings.Trim(p[1], "/"), "/")
	key := ""
	for i := len(segments) - 1; i >= 0; i-- {
		if !strings.HasPrefix(segments[i], "{") {
			key = segments[i]
			break
		}
	}
	name := names[key]
	if name == "" {
		name = "面板操作"
	}
	verb := map[string]string{"GET": "查看", "POST": "执行/新增", "PUT": "设置", "PATCH": "修改", "DELETE": "删除"}[p[0]]
	return verb + " · " + name
}

// 先原子消费确认，再调用下游。超时结果不自动重试，防止删除/续费重复执行。
func (st *telegramControlState) takePending(nonce, chat, sender string, now time.Time) (telegramPending, bool) {
	st.Lock()
	defer st.Unlock()
	p, ok := st.Pending[nonce]
	if !ok || p.Chat != chat || p.Sender != sender || !now.Before(p.Until) {
		return telegramPending{}, false
	}
	delete(st.Pending, nonce)
	return p, true
}

// banWebhookToken 是调宿主机封禁服务（web-security-events :8098）的口令。
// 原来直接用凭据加密主密钥 CREDENTIAL_KEY 当请求头口令，请求头一旦被记录，
// 主密钥就跟着泄露（2026-11-02 审计 M17）。改用独立的 BAN_WEBHOOK_TOKEN；
// 未配置时回退 CREDENTIAL_KEY，只为分步上线，配好后服务端不再接受旧口令。
func banWebhookToken() string {
	if token := strings.TrimSpace(os.Getenv("BAN_WEBHOOK_TOKEN")); token != "" {
		return token
	}
	return os.Getenv("CREDENTIAL_KEY")
}

// banConfirmTTL 与其它写操作的确认时效一致。
const banConfirmTTL = 2 * time.Minute

// telegramBanConfirm 发一张带时效的确认卡；callback_data 形如 tg:bnok:<unix>:<ip>
// （IPv6 最长 39 字符，总长 ≤ 58，在 Telegram 的 64 字节上限内）。
func (s *Server) telegramBanConfirm(ctx context.Context, chat, ip string, isUnban bool, now time.Time) {
	if net.ParseIP(ip) == nil {
		_ = s.sendTelegram(ctx, chat, "❌ 无效的 IP 地址")
		return
	}
	// 2026-11-06：内网 / 回环地址不能封。登录告警里的 IP 来自代理链：链路配错时记下的是 docker 网关
	// （172.19.0.1）或 127.0.0.1 —— 点一下「永久封禁」就把面板自己、商城回调乃至整台宿主机的入站全断了。
	if !isUnban && isPrivateOrLoopbackIP(ip) {
		_ = s.sendTelegram(ctx, chat, "❌ "+ip+" 是内网或回环地址，多半是代理链没带上真实来源；封禁它会切断面板自身的流量，已拒绝。")
		return
	}
	prefix, verb := "tg:bnok:", "永久封禁"
	if isUnban {
		prefix, verb = "tg:ubok:", "解除封禁"
	}
	_ = telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "sendMessage", map[string]any{
		"chat_id": chat,
		"text":    fmt.Sprintf("确认要%s %s 吗？两分钟内有效。", verb, ip),
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{{{
			"text": "✅ 确认" + verb, "callback_data": prefix + strconv.FormatInt(now.Unix(), 10) + ":" + ip,
		}}}},
	}, nil)
}

// usedBanConfirms 记录已执行过的封禁确认（callback_data → 用掉的时间）；只保留确认有效期内的条目。
var usedBanConfirms = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// consumeBanConfirm 第一次调用返回 true 并记下；同一确认再次调用返回 false。
func consumeBanConfirm(data string, now time.Time) bool {
	usedBanConfirms.Lock()
	defer usedBanConfirms.Unlock()
	for k, t := range usedBanConfirms.at {
		if now.Sub(t) > 2*banConfirmTTL {
			delete(usedBanConfirms.at, k)
		}
	}
	if _, used := usedBanConfirms.at[data]; used {
		return false
	}
	usedBanConfirms.at[data] = now
	return true
}

// parseBanConfirm 解析确认按钮并检查时效。IP 必须能被解析（IPv6 本身含冒号，按第一个冒号切）。
func parseBanConfirm(data string, now time.Time) (string, bool) {
	rest := strings.TrimPrefix(strings.TrimPrefix(data, "tg:bnok:"), "tg:ubok:")
	stamp, ip, found := strings.Cut(rest, ":")
	if !found {
		return "", false
	}
	unix, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", false
	}
	issued := time.Unix(unix, 0)
	if now.Before(issued.Add(-5*time.Second)) || now.Sub(issued) > banConfirmTTL || net.ParseIP(ip) == nil {
		return "", false
	}
	return ip, true
}

// auditTelegramBan 把机器人上执行的封禁 / 解封记进审计日志（归到机器人绑定的管理员名下）。
func (s *Server) auditTelegramBan(ctx context.Context, ip string, isUnban bool) {
	if s.db == nil {
		return
	}
	action := "telegram_ban_ip"
	if isUnban {
		action = "telegram_unban_ip"
	}
	var userID any
	if id, _, _, err := s.telegramAdmin(ctx); err == nil {
		userID = id
	}
	details, _ := json.Marshal(map[string]any{"ip": ip, "via": "telegram"})
	if _, err := s.db.Exec(ctx, `INSERT INTO audit_logs (user_id, action, resource_type, details) VALUES ($1, $2, 'ip', $3::jsonb)`,
		userID, action, string(details)); err != nil {
		s.logger.Warn("audit telegram ban failed", "ip", ip, "error", err)
	}
}

// banWebhookClient 专用于宿主机封禁联动接口：禁用重定向、限制空闲连接并设置独立超时，
// 避免误用全局 http.DefaultClient 造成重定向越权或无限等待。
var banWebhookClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	},
}

// telegramBanIPAction 调宿主机封禁服务；返回是否确认生效。
func (s *Server) telegramBanIPAction(ctx context.Context, chat, ip string, isUnban bool) bool {
	base := banWebhookBaseURL()
	if base == "" {
		_ = s.sendTelegram(ctx, chat, "⚠️ 未配置宿主机封禁服务，请先设置环境变量 BAN_WEBHOOK_URL")
		return false
	}
	endpoint := base + "/ban"
	if isUnban {
		endpoint = base + "/unban"
	}
	body, _ := json.Marshal(map[string]string{"ip": ip})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "❌ 构造封禁请求失败")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ban-Auth", banWebhookToken())
	resp, err := banWebhookClient.Do(req)
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "❌ 调用宿主机封禁服务失败："+err.Error())
		return false
	}
	defer resp.Body.Close()
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil || !res.OK {
		reason := firstNonEmpty(res.Error, fmt.Sprintf("服务返回异常（HTTP %d）", resp.StatusCode))
		_ = s.sendTelegram(ctx, chat, fmt.Sprintf("⚠️ 操作未生效 (%s)：%s", ip, reason))
		return false
	}
	token := s.purityKeys.Get(ippurity.KeyTelegram)
	if isUnban {
		_ = telegramCall(ctx, token, "sendMessage", map[string]any{
			"chat_id": chat,
			"text":    fmt.Sprintf("🔓 已解除对 %s 的永久封禁（已从 nftables 黑名单移除）。", ip),
			"reply_markup": map[string]any{
				"inline_keyboard": [][]map[string]string{{{
					"text":          "🚫 重新封禁此IP (" + ip + ")",
					"callback_data": "tg:banip:" + ip,
				}}},
			},
		}, nil)
	} else {
		_ = telegramCall(ctx, token, "sendMessage", map[string]any{
			"chat_id": chat,
			"text":    fmt.Sprintf("✅ 已永久封禁 IP：%s（已写入 fail2ban 与 nftables 宿主机+容器转发双链，全端口流量已彻底阻断）。", ip),
			"reply_markup": map[string]any{
				"inline_keyboard": [][]map[string]string{{{
					"text":          "🔓 解除封禁 (" + ip + ")",
					"callback_data": "tg:unbanip:" + ip,
				}}},
			},
		}, nil)
	}
	return true
}

// telegramBanQuery 调宿主机封禁服务的查询接口（/list 或 /ipinfo），把返回的中文文本发给管理员；
// 查 IP 时根据封禁状态附带「封禁 / 解封」按钮（自己的服务器和白名单不给按钮）。
func (s *Server) telegramBanQuery(ctx context.Context, chat, path, ip string) {
	base := banWebhookBaseURL()
	if base == "" {
		_ = s.sendTelegram(ctx, chat, "⚠️ 未配置宿主机封禁服务，请先设置环境变量 BAN_WEBHOOK_URL")
		return
	}
	body, _ := json.Marshal(map[string]string{"ip": ip})
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "❌ 构造查询请求失败")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ban-Auth", banWebhookToken())
	resp, err := banWebhookClient.Do(req)
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "❌ 调用宿主机封禁服务失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	var res struct {
		OK        bool   `json:"ok"`
		Text      string `json:"text"`
		Error     string `json:"error"`
		Banned    bool   `json:"banned"`
		Protected bool   `json:"protected"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil || !res.OK {
		reason := firstNonEmpty(res.Error, fmt.Sprintf("服务返回异常（HTTP %d）", resp.StatusCode))
		_ = s.sendTelegram(ctx, chat, "⚠️ 查询失败："+reason)
		return
	}
	// 2026-11-03：长列表（6 个 jail × 25 个 IPv6）超过 4096 字符会被整条拒收。
	// 先把超出部分作为普通消息发出，按钮挂在最后一段上。
	chunks := splitTelegramMessage(res.Text, telegramChunkLimit)
	for _, chunk := range chunks[:len(chunks)-1] {
		if err := s.sendTelegram(ctx, chat, chunk); err != nil {
			s.logger.Warn("telegram ban query: send failed", "error", err)
			return
		}
	}
	msg := map[string]any{"chat_id": chat, "text": chunks[len(chunks)-1], "disable_web_page_preview": true}
	if path == "/ipinfo" && !res.Protected {
		btn := map[string]string{"text": "🚫 永久封禁 " + ip, "callback_data": "tg:banip:" + ip}
		if res.Banned {
			btn = map[string]string{"text": "🔓 解除封禁 " + ip, "callback_data": "tg:unbanip:" + ip}
		}
		msg["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{btn}}}
	}
	_ = telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "sendMessage", msg, nil)
}

// telegramPartialOutcome 识别「2xx 但没有全部生效」的响应：node_state_ok=false、failed_nodes>0，
// 或非空的 failed 明细。返回值二为可直接给人看的说明（有就带上）。
// telegramRouteTag 是路由 pattern 的 8 位短哈希，写进菜单按钮，防止部署后下标移位打开别的接口。
func telegramRouteTag(pattern string) string {
	sum := sha256.Sum256([]byte(pattern))
	return hex.EncodeToString(sum[:4])
}

func telegramPartialOutcome(raw []byte) (bool, string) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return false, ""
	}
	note, _ := m["node_state_message"].(string)
	if ok, present := m["node_state_ok"].(bool); present && !ok {
		return true, note
	}
	if n, ok := m["failed_nodes"].(float64); ok && n > 0 {
		return true, note
	}
	// 2026-11-06：failed 也可能是计数（批量纯净度检测回 {"ok":3,"failed":2,"errors":[...]}），
	// 删节点的永久下线分支回 failed_count。原来只认数组 / 对象，这些情况机器人照样回「✅」。
	switch f := m["failed"].(type) {
	case []any:
		if len(f) > 0 {
			return true, note
		}
	case map[string]any:
		if len(f) > 0 {
			return true, note
		}
	case float64:
		if f > 0 {
			return true, firstNonEmpty(note, fmt.Sprintf("%d 项失败", int(f)))
		}
	}
	if n, ok := m["failed_count"].(float64); ok && n > 0 {
		return true, firstNonEmpty(note, fmt.Sprintf("%d 项失败", int(n)))
	}
	if errs, ok := m["errors"].([]any); ok && len(errs) > 0 {
		return true, firstNonEmpty(note, fmt.Sprintf("%d 项出错", len(errs)))
	}
	return false, ""
}
