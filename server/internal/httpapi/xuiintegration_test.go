package httpapi

// xuiintegration_test.go — 用「行为逼真的假 3X-UI」验证完整对接链路。
//
// 3X-UI 有**两代互不兼容的 API**，真实世界两种都在跑（实测：2.x 系列是旧版，
// 3.0.2+/3.8.5 是新版）。这个文件对两代各建一台假面板，把隐式契约钉住：
//
// 旧版（Vue）：
//   1. 登录是 form 表单，成功靠 Set-Cookie（cookie 名 "session"）；
//   2. 后续 /panel/api/* 完全依赖 cookie，没有独立 token；
//   3. 开户走 /panel/api/inbounds/addClient（form + settings 字符串）；
//   4. 删户/重置走 /panel/api/inbounds/{id}/delClient|resetClientTraffic/{key}。
//
// 新版（React / 3.8.5，逐条实测得来）：
//   1. 登录前必须先 GET /csrf-token 取令牌，登录时要带 X-CSRF-Token 头，
//      否则一律 403 + 空 body（极易误判成「密码错」）；
//   2. cookie 名是 "3x-ui"；
//   3. inbounds/list 的 settings 是**对象**，客户端流量在同级顶格 clientStats 数组；
//   4. 开户走 /panel/api/clients/add，body 是 JSON 且 inbound 字段是**数组**；
//   5. 删户/重置走 /panel/api/clients/del|resetTraffic/{email}（按 email，不是 clientId）。
//
// 只要任何一条写错，真机上都会表现为「连不上」或「操作静默不生效」。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeXUI 是一台最小但行为逼真的 3X-UI。modern 决定它是哪一代。
type fakeXUI struct {
	cookieName string
	modern     bool
	sessions   map[string]bool
	csrf       string
	inbounds   []map[string]any

	logins   int
	apiCalls []string
	lastForm url.Values
	// lastCSRF 记录登录时是否带上了 CSRF 头（新版必填）。
	lastCSRF  string
	rejectAll bool
	// onlinesDown：在线接口返回 500（面板关闭了在线统计 / 接口故障）。
	onlinesDown bool
	addClients  []map[string]any
	deletedKey  []string
	resetKey    []string
}

func newFakeXUI(cookieName string) *fakeXUI {
	return &fakeXUI{
		cookieName: cookieName,
		sessions:   map[string]bool{},
		csrf:       "csrf-token-for-test",
		inbounds: []map[string]any{
			{
				"id": 1, "port": 443, "protocol": "vless", "remark": "主入口", "enable": true,
				"up": 1024, "down": 2048, "total": 0, "expiryTime": 0,
				"settings": `{"clients":[{"id":"uuid-1","email":"a@b.com","totalGB":0,"expiryTime":0}],"decryption":"none"}`,
			},
		},
	}
}

// newFakeXUIModern 造一台新版面板：settings 是对象，流量在顶格 clientStats。
func newFakeXUIModern() *fakeXUI {
	panel := newFakeXUI("3x-ui")
	panel.modern = true
	panel.inbounds = []map[string]any{
		{
			"id": 1, "port": 15443, "protocol": "vless", "remark": "新版入口", "enable": true,
			"up": 10, "down": 20, "total": 0, "expiryTime": 0,
			"settings": map[string]any{
				"clients": []any{
					map[string]any{"id": "uuid-1", "email": "a@b.com", "totalGB": 0, "expiryTime": 0, "subId": "sub1"},
				},
				"decryption": "none",
			},
			// 新版：客户端流量在顶格数组里，按 email 对齐。
			"clientStats": []any{
				map[string]any{"email": "a@b.com", "up": 4096, "down": 8192, "enable": true, "total": 0, "expiryTime": 0, "lastOnline": 12345},
			},
		},
	}
	return panel
}

func (f *fakeXUI) handler() http.Handler {
	mux := http.NewServeMux()

	// 新版才有 /csrf-token；旧版对它返回 404（探测新旧就是看这个）。
	if f.modern {
		mux.HandleFunc("/csrf-token", func(w http.ResponseWriter, r *http.Request) {
			writeXUIJSON(w, map[string]any{"success": true, "obj": f.csrf})
		})
	}

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins++
		if f.rejectAll {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "invalid credentials"})
			return
		}
		if err := r.ParseForm(); err != nil {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "bad form"})
			return
		}
		f.lastForm = r.PostForm
		f.lastCSRF = r.Header.Get("X-CSRF-Token")
		// 两代都要求 form-urlencoded。
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "expect form"})
			return
		}
		// 新版要求 CSRF 头，缺失就 403 + 空 body（复刻真实行为，好让实现侧的
		// 错误翻译被真正测到）。
		if f.modern {
			if f.lastCSRF == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.lastCSRF != f.csrf {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		value := "session-token-abc"
		f.sessions[value] = true
		http.SetCookie(w, &http.Cookie{Name: f.cookieName, Value: value, Path: "/"})
		writeXUIJSON(w, map[string]any{"success": true, "msg": "Login Successfully", "obj": nil})
	})

	mux.HandleFunc("/panel/api/inbounds/list", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "list")
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		writeXUIJSON(w, map[string]any{"success": true, "msg": "", "obj": f.inbounds})
	})
	mux.HandleFunc("/panel/api/inbounds/onlines", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "onlines")
		if f.onlinesDown {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		writeXUIJSON(w, map[string]any{"success": true, "msg": "", "obj": []string{"a@b.com"}})
	})
	// 新版在线查询路径。
	mux.HandleFunc("/panel/api/clients/onlines", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "clients/onlines")
		if f.onlinesDown {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		writeXUIJSON(w, map[string]any{"success": true, "msg": "", "obj": []string{"a@b.com"}})
	})
	// 读单个 inbound（旧版开户前用来取协议）。
	mux.HandleFunc("/panel/api/inbounds/get/", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "get")
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/panel/api/inbounds/get/")
		for _, ib := range f.inbounds {
			if fmt.Sprint(ib["id"]) == id {
				writeXUIJSON(w, map[string]any{"success": true, "msg": "", "obj": ib})
				return
			}
		}
		writeXUIJSON(w, map[string]any{"success": false, "msg": "record not found"})
	})
	// 旧版开户。
	mux.HandleFunc("/panel/api/inbounds/addClient", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "addClient")
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		if err := r.ParseForm(); err != nil {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "bad form"})
			return
		}
		settings := r.PostForm.Get("settings")
		if settings == "" {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "missing settings"})
			return
		}
		var parsed struct {
			Clients []map[string]any `json:"clients"`
		}
		if err := json.Unmarshal([]byte(settings), &parsed); err != nil || len(parsed.Clients) == 0 {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "malformed settings"})
			return
		}
		// 复刻 3X-UI AddInboundClient 的身份校验：vless / vmess 要 id，trojan 要 password。
		protocol := ""
		for _, ib := range f.inbounds {
			if fmt.Sprint(ib["id"]) == r.PostForm.Get("id") {
				protocol, _ = ib["protocol"].(string)
			}
		}
		for _, c := range parsed.Clients {
			field := "id"
			if protocol == "trojan" {
				field = "password"
			}
			if v, _ := c[field].(string); protocol != "shadowsocks" && v == "" {
				writeXUIJSON(w, map[string]any{"success": false, "msg": "empty client ID"})
				return
			}
		}
		f.addClients = append(f.addClients, parsed.Clients...)
		writeXUIJSON(w, map[string]any{"success": true, "msg": "Client added"})
	})
	// 新版开户：JSON body + inboundIds 数组。少写数组要报错（复刻真实行为）。
	mux.HandleFunc("/panel/api/clients/add", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, "clients/add")
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		var body struct {
			InboundIds []int          `json:"inboundIds"`
			Client     map[string]any `json:"client"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "malformed body"})
			return
		}
		if len(body.InboundIds) == 0 {
			// 真实面板的原话就是这个，我们把它照搬以免实现侧误用单数 id。
			writeXUIJSON(w, map[string]any{"success": false, "msg": "Something went wrong (at least one inbound is required\n)"})
			return
		}
		if body.Client == nil {
			writeXUIJSON(w, map[string]any{"success": false, "msg": "missing client"})
			return
		}
		f.addClients = append(f.addClients, body.Client)
		writeXUIJSON(w, map[string]any{"success": true, "msg": "Inbound client(s) have been added."})
	})
	// 新版删户 / 重置：按 email 走路径。
	mux.HandleFunc("/panel/api/clients/del/", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, r.URL.Path)
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		f.deletedKey = append(f.deletedKey, strings.TrimPrefix(r.URL.Path, "/panel/api/clients/del/"))
		writeXUIJSON(w, map[string]any{"success": true, "msg": "Inbound client has been deleted."})
	})
	mux.HandleFunc("/panel/api/clients/resetTraffic/", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, r.URL.Path)
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		f.resetKey = append(f.resetKey, strings.TrimPrefix(r.URL.Path, "/panel/api/clients/resetTraffic/"))
		writeXUIJSON(w, map[string]any{"success": true, "msg": "Traffic has been reset."})
	})
	// 旧版删户 / 重置（路径兜底）。
	mux.HandleFunc("/panel/api/inbounds/", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls = append(f.apiCalls, r.URL.Path)
		if !f.authed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeXUIJSON(w, map[string]any{"success": false, "msg": "not logged in"})
			return
		}
		writeXUIJSON(w, map[string]any{"success": true, "msg": "ok"})
	})
	return mux
}

func (f *fakeXUI) authed(r *http.Request) bool {
	cookie, err := r.Cookie(f.cookieName)
	if err != nil {
		return false
	}
	return f.sessions[cookie.Value]
}

func writeXUIJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

// ---- 用例：旧版 ----

// 旧版 cookie 名 "session"：登录 → 拉列表，客户端与入站都要解析出来。
func TestFakeXUIFullFlowLegacyCookieName(t *testing.T) {
	panel := newFakeXUI("session")
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	inbounds, online, err := probeXUIPanelAllowLocal(context.Background(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("旧版完整链路失败: %v", err)
	}
	if panel.logins != 1 {
		t.Fatalf("应登录 1 次，实际 %d", panel.logins)
	}
	if panel.lastForm.Get("username") != "admin" || panel.lastForm.Get("password") != "secret" {
		t.Fatalf("登录表单字段不对: %v", panel.lastForm)
	}
	if len(inbounds) != 1 {
		t.Fatalf("应解析出 1 个入站，得到 %d", len(inbounds))
	}
	if len(inbounds[0].Clients) != 1 {
		t.Fatalf("应解析出 1 个客户端，得到 %d", len(inbounds[0].Clients))
	}
	if len(online) != 1 || online[0] != "a@b.com" {
		t.Fatalf("在线列表不符: %v", online)
	}
	if len(panel.apiCalls) < 2 {
		t.Fatalf("应至少调用 list + onlines，实际 %v", panel.apiCalls)
	}
}

// ---- 用例：新版 ----

// 新版全链路：探测到 CSRF → 带 CSRF 头登录 → 拉列表（settings 是对象）
// → 流量从顶格 clientStats 对齐到客户端上。
func TestFakeXUIModernFullFlow(t *testing.T) {
	panel := newFakeXUIModern()
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	result, err := probeXUIPanelAllowLocalResult(context.Background(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("新版完整链路失败: %v", err)
	}
	if !result.Modern {
		t.Fatal("应识别为新版面板")
	}
	// 新版必须带上 CSRF 头，否则假面板会 403。
	if panel.lastCSRF != panel.csrf {
		t.Fatalf("登录未带正确 CSRF 头，实际 %q", panel.lastCSRF)
	}
	if len(result.Inbounds) != 1 {
		t.Fatalf("应解析出 1 个入站，得到 %d", len(result.Inbounds))
	}
	clients := result.Inbounds[0].Clients
	if len(clients) != 1 {
		t.Fatalf("应解析出 1 个客户端，得到 %d", len(clients))
	}
	// 顶格 clientStats 的流量必须被合并到客户端上（这是新版最关键的结构差异）。
	if up := xuiAsInt64(clients[0]["up"]); up != 4096 {
		t.Fatalf("客户端 up 应从 clientStats 对齐为 4096，得到 %d", up)
	}
	if down := xuiAsInt64(clients[0]["down"]); down != 8192 {
		t.Fatalf("客户端 down 应从 clientStats 对齐为 8192，得到 %d", down)
	}
	// 新版在线状态走 clients/onlines。
	foundOnlines := false
	for _, call := range panel.apiCalls {
		if call == "clients/onlines" {
			foundOnlines = true
		}
	}
	if !foundOnlines {
		t.Fatalf("新版应请求 clients/onlines，实际 %v", panel.apiCalls)
	}
}

// 新版没有 CSRF 头时必须 403 且无 body —— 实现侧要把这个翻译成可操作的提示，
// 而不是让操作者以为「密码错了」。
func TestFakeXUIModernMissingCSRFIsTranslated(t *testing.T) {
	panel := newFakeXUIModern()
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	// 绕过 CSRF 探测，直接发一个不带 CSRF 头的登录。
	raw, status, _, err := xuiCall(context.Background(), allowLocalClient(), xuiRequest{
		method: http.MethodPost, url: server.URL + "/login",
		form: url.Values{"username": {"admin"}, "password": {"secret"}},
	})
	if err != nil {
		t.Fatalf("请求本身不该失败: %v", err)
	}
	if status != http.StatusForbidden || len(raw) != 0 {
		t.Fatalf("期望 403 + 空 body，得到 %d / %d 字节", status, len(raw))
	}
	if _, parseErr := parseXUIResponse(raw, status); parseErr == nil {
		t.Fatal("403 应产生错误")
	} else if !strings.Contains(parseErr.Error(), "会话") && !strings.Contains(parseErr.Error(), "凭据") {
		t.Fatalf("403 提示不具操作性: %v", parseErr)
	}
}

// 新版开户：body 必须是 {"inboundIds":[...],"client":{...}}。
// 这里直接走产品的 addXUIClient，确保它用对了形状。
func TestFakeXUIModernAddClientUsesInboundIdsArray(t *testing.T) {
	panel := newFakeXUIModern()
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	session, err := newXUISession(context.Background(), allowLocalClient(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if !session.modern {
		t.Fatal("会话应标记为新版")
	}
	err = addXUIClient(context.Background(), session, server.URL, xuiAddClientInput{
		InboundID: 1, Email: "new@user.com", ExpireDays: 7, LimitTotal: 5368709120,
	})
	if err != nil {
		t.Fatalf("开户被拒（多半是 body 形状不对）: %v", err)
	}
	if len(panel.addClients) != 1 {
		t.Fatalf("假面板应收到 1 个新客户端，得到 %d", len(panel.addClients))
	}
	if email, _ := panel.addClients[0]["email"].(string); email != "new@user.com" {
		t.Fatalf("客户端 email 不对: %v", panel.addClients[0]["email"])
	}
	// subId 必须是服务端生成的（前端不参与），且非空。
	if subID, _ := panel.addClients[0]["subId"].(string); subID == "" {
		t.Fatal("客户端应有服务端生成的 subId")
	}
}

// 新版删户/重置：按 email 走 /panel/api/clients/*，且必须带写操作的 CSRF 头。
func TestFakeXUIModernDeleteAndResetByEmail(t *testing.T) {
	panel := newFakeXUIModern()
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	session, err := newXUISession(context.Background(), allowLocalClient(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if err := deleteXUIClient(context.Background(), session, server.URL, "1", "a@b.com"); err != nil {
		t.Fatalf("删户被拒: %v", err)
	}
	if err := resetXUIClientTraffic(context.Background(), session, server.URL, "1", "a@b.com"); err != nil {
		t.Fatalf("重置被拒: %v", err)
	}
	if len(panel.deletedKey) != 1 || panel.deletedKey[0] != "a@b.com" {
		t.Fatalf("删户应以 email 定位，实际 %v", panel.deletedKey)
	}
	if len(panel.resetKey) != 1 || panel.resetKey[0] != "a@b.com" {
		t.Fatalf("重置应以 email 定位，实际 %v", panel.resetKey)
	}
}

// 旧版删户/重置：走 inbounds 命名空间，且不需要 CSRF 头。
func TestFakeXUILegacyDeleteAndReset(t *testing.T) {
	panel := newFakeXUI("session")
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	session, err := newXUISession(context.Background(), allowLocalClient(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if session.modern {
		t.Fatal("旧版面板不该被标记为新版")
	}
	if err := deleteXUIClient(context.Background(), session, server.URL, "1", "uuid-1"); err != nil {
		t.Fatalf("旧版删户被拒: %v", err)
	}
	expected := "/panel/api/inbounds/1/delClient/uuid-1"
	found := false
	for _, call := range panel.apiCalls {
		if call == expected {
			found = true
		}
	}
	if !found {
		t.Fatalf("旧版删户路径不对，实际调用: %v", panel.apiCalls)
	}
}

// ---- 用例：共性 ----

// 凭据错误：必须把面板的 msg 透出来，而不是笼统的「失败」。
func TestFakeXUIRejectsBadCredentials(t *testing.T) {
	panel := newFakeXUI("3x-ui")
	panel.rejectAll = true
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	_, _, err := probeXUIPanelAllowLocal(context.Background(), server.URL, "admin", "wrong")
	if err == nil {
		t.Fatal("错误凭据应当失败")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Fatalf("未透出面板错误信息: %v", err)
	}
}

// 未登录直接打 API：应得到 401 的可操作提示。
func TestFakeXUIUnauthenticatedAPICall(t *testing.T) {
	panel := newFakeXUI("3x-ui")
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	raw, status, _, err := xuiCall(context.Background(), allowLocalClient(), xuiRequest{
		method: http.MethodGet, url: server.URL + "/panel/api/inbounds/list",
	})
	if err != nil {
		t.Fatalf("请求本身不该失败: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("期望 401，得到 %d", status)
	}
	_, parseErr := parseXUIResponse(raw, status)
	if parseErr == nil || !strings.Contains(parseErr.Error(), "凭据") {
		t.Fatalf("401 提示不具操作性: %v", parseErr)
	}
}

// 旧版 addClient：服务端构造的 settings 必须是合法 JSON 且含 clients 数组。
func TestFakeXUILegacyAddClientPayloadIsWellFormed(t *testing.T) {
	panel := newFakeXUI("session")
	server := httptest.NewServer(panel.handler())
	defer server.Close()

	session, err := newXUISession(context.Background(), allowLocalClient(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	err = addXUIClient(context.Background(), session, server.URL, xuiAddClientInput{
		InboundID: 1, Email: "new@user.com", ExpireDays: 7, LimitTotal: 5368709120,
	})
	if err != nil {
		t.Fatalf("旧版开户被拒: %v", err)
	}
	if len(panel.addClients) != 1 {
		t.Fatalf("假面板应收到 1 个新客户端，得到 %d", len(panel.addClients))
	}
}

// ---- 本地测试辅助 ----
//
// 正式的 xuiCall 走 portalClient，其拨号器会拒绝私有地址 —— 那是生产必需的
// SSRF 防护，但会让 httptest（127.0.0.1）不可达。测试里显式注入一个普通
// http.Client，测的是「协议正确性」而不是「SSRF 防护」；后者由
// TestXUIClientRefusesPrivateAddresses 单独覆盖。

func allowLocalClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// probeXUIPanelAllowLocal 是 probeXUIPanel 的可注入版本，逻辑保持一致。
func probeXUIPanelAllowLocal(ctx context.Context, baseURL, username, password string) ([]xuiInboundSummary, []string, error) {
	result, err := probeXUIPanelAllowLocalResult(ctx, baseURL, username, password)
	if err != nil {
		return nil, nil, err
	}
	return result.Inbounds, result.Onlines, nil
}

func probeXUIPanelAllowLocalResult(ctx context.Context, baseURL, username, password string) (*xuiProbeResult, error) {
	session, err := newXUISession(ctx, allowLocalClient(), baseURL, username, password)
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
			return nil, fmt.Errorf("解析 inbound 列表失败: %w", err)
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

// TestXUIClientRefusesPrivateAddresses 钉住生产用的 SSRF 防护本身：
// portalClient 必须拒绝解析到私有/环回地址的目标。
func TestXUIClientRefusesPrivateAddresses(t *testing.T) {
	if _, err := xuiLogin(context.Background(), portalClient, "http://127.0.0.1:9999", "a", "b"); err == nil {
		t.Fatal("portalClient 应拒绝环回地址")
	} else if !strings.Contains(err.Error(), "非公网地址") {
		t.Fatalf("拒绝原因不对: %v", err)
	}
	if _, err := xuiLogin(context.Background(), portalClient, "http://10.0.0.1:9999", "a", "b"); err == nil {
		t.Fatal("portalClient 应拒绝私有地址")
	}
}

// 2026-11-03：旧版面板开户必须带上协议要求的身份字段（vless 的 id、trojan 的 password），
// 原来两者都不发 ⇒ 真实面板回「empty client ID」，旧版上开户 100% 失败（假面板之前不校验，测不出来）。
func TestLegacyXUIAddClientFillsProtocolIdentity(t *testing.T) {
	for _, protocol := range []string{"vless", "trojan"} {
		panel := newFakeXUI("session")
		panel.inbounds[0]["protocol"] = protocol
		server := httptest.NewServer(panel.handler())
		session, err := newXUISession(context.Background(), allowLocalClient(), server.URL, "admin", "secret")
		if err != nil {
			t.Fatal(err)
		}
		err = addXUIClient(context.Background(), session, server.URL, xuiAddClientInput{InboundID: 1, Email: protocol + "@user.com"})
		server.Close()
		if err != nil {
			t.Fatalf("%s: legacy add rejected: %v", protocol, err)
		}
		field := map[string]string{"vless": "id", "trojan": "password"}[protocol]
		if v, _ := panel.addClients[0][field].(string); v == "" {
			t.Fatalf("%s: client %s missing", protocol, field)
		}
	}
}

// 2026-11-06：在线接口失败不能当成「0 人在线」—— OnlinesOK 必须为 false，调用方据此保留上次的在线数。
func TestFakeXUIOnlinesFailureIsNotZeroOnline(t *testing.T) {
	panel := newFakeXUI("session")
	panel.onlinesDown = true
	server := httptest.NewServer(panel.handler())
	defer server.Close()
	result, err := probeXUIPanelAllowLocalResult(context.Background(), server.URL, "admin", "secret")
	if err != nil {
		t.Fatalf("inbounds still load: %v", err)
	}
	if result.OnlinesOK {
		t.Fatal("a failed onlines call must not be reported as a successful empty list")
	}
	panel.onlinesDown = false
	result, err = probeXUIPanelAllowLocalResult(context.Background(), server.URL, "admin", "secret")
	if err != nil || !result.OnlinesOK || len(result.Onlines) != 1 {
		t.Fatalf("a working onlines call must be OK: %+v %v", result, err)
	}
}
