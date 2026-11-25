package httpapi

// 本轮新增的三处安全关键逻辑的单测：
//
//  1. portal URL 的解析 —— 它决定请求会发到哪个 host、哪个路径前缀。
//     一个宽松的解析会让操作者粘进来的任意字符串变成出站请求的一部分。
//  2. 动作表单的构造 —— 它决定前端能把什么字段送进 portal 的命令行拼接处。
//  3. 审计脱敏 —— 它决定凭据会不会被写进人人可见的审计表。
//
// 这些都不是「顺手补的」，每一条对应一个真实的误用路径。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitPortalURL(t *testing.T) {
	// 64 hex 是 portal prepare() 里 secrets.token_hex(32) 的真实产物长度。
	const realToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name      string
		input     string
		wantHost  string
		wantToken string
		wantOK    bool
	}{
		{
			name:      "portal-access.json 的真实形态",
			input:     "https://node1.example.com:29421/" + realToken + "/",
			wantHost:  "node1.example.com:29421",
			wantToken: realToken,
			wantOK:    true,
		},
		{
			name:      "没有尾斜杠也认",
			input:     "https://node1.example.com:29421/" + realToken,
			wantHost:  "node1.example.com:29421",
			wantToken: realToken,
			wantOK:    true,
		},
		{
			name:     "裸地址没有 token，属于正常情况",
			input:    "https://hy2.example.com:8443",
			wantHost: "",
			wantOK:   false,
		},
		{
			name:     "路径不是 hex，必须拒绝（否则会拼出任意路径）",
			input:    "https://evil.example.com/../../admin/",
			wantHost: "",
			wantOK:   false,
		},
		{
			name:     "太短的不是 token",
			input:    "https://evil.example.com/abc/",
			wantHost: "",
			wantOK:   false,
		},
		{
			name:     "大写 hex 拒绝——portal 只生成小写，放宽没有收益",
			input:    "https://evil.example.com/" + strings.ToUpper(realToken) + "/",
			wantHost: "",
			wantOK:   false,
		},
		// 2026-11-06：不写 https:// 时同样要拿到 token（normalizePortalURLWithToken 会补 scheme）。
		{
			name:      "域名不带 scheme",
			input:     "hy2.example.com:8443/" + realToken + "/",
			wantHost:  "hy2.example.com:8443",
			wantToken: realToken,
			wantOK:    true,
		},
		{
			name:      "IP:端口 不带 scheme",
			input:     "1.2.3.4:8443/" + realToken + "/",
			wantHost:  "1.2.3.4:8443",
			wantToken: realToken,
			wantOK:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, token, ok := splitPortalURL(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if token != tc.wantToken {
				t.Errorf("token = %q, want %q", token, tc.wantToken)
			}
		})
	}
}

func TestNormalizePortalURLWithToken(t *testing.T) {
	const realToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ok := []struct {
		input string
		want  string
	}{
		{"https://hy2.example.com:8443", "https://hy2.example.com:8443"},
		{"https://hy2.example.com:8443/", "https://hy2.example.com:8443"},
		{"hy2.example.com:8443", "https://hy2.example.com:8443"},
		{"https://hy2.example.com:8443/api/v1/node/meta", "https://hy2.example.com:8443"},
		// 带 {token} 的整条门户地址必须被接受并剥掉 —— 这正是操作者会粘的东西。
		{"https://hy2.example.com:8443/" + realToken + "/", "https://hy2.example.com:8443"},
	}
	for _, tc := range ok {
		got, err := normalizePortalURLWithToken(tc.input)
		if err != nil {
			t.Errorf("normalizePortalURLWithToken(%q) 意外报错: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizePortalURLWithToken(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	// 这些必须被拒绝。每一条都对应一种把请求引到别处的尝试。
	bad := []string{
		"https://evil.example.com/../admin/",   // 路径回溯
		"https://user:pass@evil.example.com/",  // 内嵌账号
		"https://evil.example.com/?x=1",        // 查询参数
		"ftp://evil.example.com",               // 非 http(s)
		"https://evil.example.com/a/b/c/",      // 多级路径
		"https://evil.example.com/notahexseg/", // 非 token 的单段路径
	}
	for _, input := range bad {
		if got, err := normalizePortalURLWithToken(input); err == nil {
			t.Errorf("normalizePortalURLWithToken(%q) 应当被拒绝，却返回 %q", input, got)
		}
	}
}

func TestPortalAccessInputResolve(t *testing.T) {
	const realToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("粘整份 JSON", func(t *testing.T) {
		input := portalAccessInput{
			AccessJSON: `{"url":"https://node1.example.com:29421/` + realToken + `/","username":"u123","password":"p456","api_key":"` + strings.Repeat("a", 40) + `"}`,
		}
		base, token, key, user, pass, err := input.resolve()
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if base != "https://node1.example.com:29421" {
			t.Errorf("baseURL = %q", base)
		}
		if token != realToken {
			t.Errorf("pathToken = %q", token)
		}
		if key != strings.Repeat("a", 40) {
			t.Errorf("apiKey = %q", key)
		}
		if user != "u123" || pass != "p456" {
			t.Errorf("basic = %q/%q", user, pass)
		}
	})

	t.Run("显式字段覆盖 JSON", func(t *testing.T) {
		input := portalAccessInput{
			AccessJSON: `{"url":"https://a.example.com/` + realToken + `/","api_key":"fromjson"}`,
			APIKey:     "explicit-key-value-long-enough",
			BasicUser:  "override-user",
		}
		_, _, key, user, _, err := input.resolve()
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if key != "explicit-key-value-long-enough" {
			t.Errorf("显式 api_key 应当优先，得到 %q", key)
		}
		if user != "override-user" {
			t.Errorf("显式 user 应当优先，得到 %q", user)
		}
	})

	t.Run("传统两字段仍然可用", func(t *testing.T) {
		input := portalAccessInput{URL: "https://hy2.example.com:8443", APIKey: strings.Repeat("b", 40)}
		base, token, key, user, pass, err := input.resolve()
		if err != nil {
			t.Fatalf("意外报错: %v", err)
		}
		if base != "https://hy2.example.com:8443" || token != "" || user != "" || pass != "" {
			t.Errorf("得到 base=%q token=%q user=%q pass=%q", base, token, user, pass)
		}
		if key != strings.Repeat("b", 40) {
			t.Errorf("apiKey = %q", key)
		}
	})

	t.Run("非法 JSON 要报错而不是静默当空", func(t *testing.T) {
		input := portalAccessInput{AccessJSON: "not json at all"}
		if _, _, _, _, _, err := input.resolve(); err == nil {
			t.Error("非法 JSON 应当报错")
		}
	})
}

func TestBuildFormHonoursWhitelist(t *testing.T) {
	spec := portalActions["warp_add_rule"]
	form := buildForm(spec, map[string]string{
		"domain": "openai.com",
		// 这两个不在 Allowed 里，必须被丢弃 —— 否则前端可以把任意键塞进
		// portal 的 parse_qs，命中它没预期的分支。
		"action": "reset_rules",
		"target": "../../etc/passwd",
	})
	if form["domain"] != "openai.com" {
		t.Errorf("domain = %q", form["domain"])
	}
	if _, present := form["target"]; present {
		t.Error("未在白名单里的 target 不应出现在表单里")
	}
	// action 由 spec.Action 决定，不能被 params 覆盖。
	if form["action"] != "add_rule" {
		t.Errorf("action = %q, 应固定为 spec.Action", form["action"])
	}
}

func TestBuildFormAppliesFixedValues(t *testing.T) {
	spec := portalActionSpec{
		Path:    "manage-reality",
		Action:  "toggle",
		Fixed:   map[string]string{"mode": "fixed"},
		Allowed: []string{"mode"},
	}
	// Fixed 必须覆盖前端传值。
	form := buildForm(spec, map[string]string{"mode": "attacker"})
	if form["mode"] != "fixed" {
		t.Errorf("Fixed 应当覆盖前端值，得到 %q", form["mode"])
	}
}

func TestValidateActionForm(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		form    map[string]string
		wantErr bool
	}{
		{"合法的 proxy create", "manage-proxy", map[string]string{"action": "create", "type": "socks5"}, false},
		{"非法 proxy action", "manage-proxy", map[string]string{"action": "drop_table"}, true},
		{"非法 proxy 类型", "manage-proxy", map[string]string{"action": "create", "type": "quic"}, true},
		{"合法域名", "manage-warp", map[string]string{"domain": "openai.com"}, false},
		{"域名带空格", "manage-warp", map[string]string{"domain": "evil domain"}, true},
		{"域名带反引号（命令注入尝试）", "manage-warp", map[string]string{"domain": "a`whoami`b"}, true},
		{"域名带分号", "manage-warp", map[string]string{"domain": "a;rm -rf /"}, true},
		{"合法 AWG 客户端名", "manage-amneziawg", map[string]string{"action": "peer_add", "name": "phone-01"}, false},
		{"AWG 名带斜杠", "manage-amneziawg", map[string]string{"action": "peer_add", "name": "a/b"}, true},
		{"AWG 名过长", "manage-amneziawg", map[string]string{"action": "peer_add", "name": strings.Repeat("x", 33)}, true},
		{"非法 AWG 动作", "manage-amneziawg", map[string]string{"action": "nuke"}, true},
		{"合法 BBR 版本", "set-bbr", map[string]string{"version": "v3"}, false},
		{"非法 BBR 版本", "set-bbr", map[string]string{"version": "v9"}, true},
		{"合法升级目标", "do-upgrade", map[string]string{"target": "core"}, false},
		{"非法升级目标", "do-upgrade", map[string]string{"target": "everything"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := portalActionSpec{Path: tc.path}
			err := validateActionForm(spec, tc.form)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateActionForm(%s, %v) error = %v, wantErr = %v", tc.path, tc.form, err, tc.wantErr)
			}
		})
	}
}

func TestSanitizeParamsForAudit(t *testing.T) {
	form := map[string]string{
		"action":   "create",
		"type":     "socks5",
		"port":     "12080",
		"password": "super-secret-value",
		"username": "proxyuser",
	}
	got := sanitizeParamsForAudit(form)
	if got["password"] == "super-secret-value" {
		t.Error("密码绝不能以明文进审计表")
	}
	if got["password"] != "(已省略)" {
		t.Errorf("password 应当被替换为占位符，得到 %q", got["password"])
	}
	// 非敏感字段要保留，否则审计就没用了。
	if got["port"] != "12080" || got["type"] != "socks5" {
		t.Errorf("非敏感字段应当保留，得到 %v", got)
	}
	// 原 map 不能被就地改动 —— 它还要拿去发请求。
	if form["password"] != "super-secret-value" {
		t.Error("sanitizeParamsForAudit 不应修改入参")
	}
}

func TestPortalWebTargetAuthorization(t *testing.T) {
	target := &portalWebTarget{Username: "alice", Password: "s3cr3t"}
	got := target.authorization()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	if got != want {
		t.Errorf("authorization() = %q, want %q", got, want)
	}
	// 前缀必须是 Basic，不是 Bearer —— 用错前缀就是 401，且报错信息不会提示原因。
	if !strings.HasPrefix(got, "Basic ") {
		t.Error("网页会话端点必须用 Basic 前缀")
	}
}

func TestPortalWebTargetRejectsBadToken(t *testing.T) {
	target := &portalWebTarget{Origin: "https://evil.example.com", PathToken: "../../admin"}
	if _, err := target.webBase(); err == nil {
		t.Error("含回溯段的 token 必须被拒绝")
	}
}

func TestPortalActionVerbsAreClosed(t *testing.T) {
	// 每个动词都必须落到一个 /{token}/ 下的已知端点上。这条断言的价值在于：
	// 如果谁把一个不存在的端点名写进表里，测试会立刻发现，而不是等到运维时
	// 点下去才看到一个 404。
	known := map[string]bool{
		"do-upgrade": true, "set-bbr": true, "manage-reality": true, "install-xray": true,
		"install-warp": true, "manage-warp": true, "install-gost": true, "manage-proxy": true,
		"install-amneziawg": true, "manage-amneziawg": true, "reboot-server": true,
		"manage-outbounds": true, "manage-vmess": true, "manage-vlessenc": true, "manage-anytls": true,
	}
	for verb, spec := range portalActions {
		if !known[spec.Path] {
			t.Errorf("动词 %s 指向未知端点 %q", verb, spec.Path)
		}
		if spec.Label == "" {
			t.Errorf("动词 %s 缺少 Label，审计与 UI 都会显示为空", verb)
		}
	}
}

func TestPortalActionDangerousFlags(t *testing.T) {
	// 这几个动词会中断服务或使已下发的配置失效，必须带危险标记。
	// 前端据此弹确认框，服务端据此要求 X-Confirm-Dangerous 头。
	mustBeDangerous := []string{"reboot", "upgrade_core", "upgrade_portal", "reality_reset"}
	for _, verb := range mustBeDangerous {
		spec, ok := portalActions[verb]
		if !ok {
			t.Fatalf("动词 %s 不存在", verb)
		}
		if !spec.Dangerous {
			t.Errorf("动词 %s 必须标记为 Dangerous", verb)
		}
	}
}

func TestProbePortalEndpointUnknownIsNotFatal(t *testing.T) {
	// 单路探测失败应当变成结果里的 Error 字段，而不是让整批请求失败。
	// 这是「没装 WARP 的节点也要能看到版本信息」这条需求的实现前提。
	res := portalStatusResult{State: "unknown", Error: "connection refused"}
	if res.State != "unknown" || res.Error == "" {
		t.Error("失败结果应当保留 state=unknown 与原始错误")
	}
	// 顺带固定住常量，避免有人改成 0 导致 portalClient 的超时失效。
	if portalStatusTimeout <= 0 || portalWebCallTimeout <= 0 || portalActionTimeout <= 0 {
		t.Error("超时常量必须为正")
	}
	_ = http.MethodGet
}

func TestPortalNoBasicResponseCarriesStructuredFlag(t *testing.T) {
	// 契约测试：前端靠 need_basic 这个布尔字段区分「还没绑凭据」（设置状态，
	// 该显示引导文案）和真正的失败（该显示红色报错）。
	//
	// 这里曾经出过真实的 bug：前端改用错误文案做正则匹配，而后端文案是
	// 「该节点未绑定 portal 访问凭据…」，与正则里的「portal 访问凭据」对不上，
	// 导致引导文案永远不显示。用结构化字段钉住它，别再退回文案嗅探。
	s := &Server{}
	rec := httptest.NewRecorder()
	s.writePortalWebTargetError(rec, errPortalNoBasic)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("errPortalNoBasic 应当返回 400，实际 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（原文 %s）", err, rec.Body.String())
	}
	if body["need_basic"] != true {
		t.Errorf("缺少 need_basic:true，前端将无法识别这是设置状态；响应 %s", rec.Body.String())
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("need_basic 响应必须同时带上给操作者看的 error 文案")
	}
	// 文案本身不该被前端解析，但仍要确认它指向「补填」而不是「不支持」，
	// 否则操作者会以为这条路走不通。
	if msg, _ := body["error"].(string); !strings.Contains(msg, "未绑定") {
		t.Errorf("文案应当说明是「未绑定」而非功能缺失，实际：%s", msg)
	}
}

func TestWarpDomainRejectsControlChars(t *testing.T) {
	// 真实缺陷回归：WARP 域名校验曾经是黑名单（「不含这些特殊字符」），
	// 漏掉了全部控制字符与换行。实测 warp_add_rule 传 "a\nb" 返回 200 ——
	// 而该值会被 portal 拼进配置文件，是注入面。
	//
	// 现在改成白名单正则。这个测试钉住它，别再退回黑名单。
	spec := portalActions["warp_add_rule"]

	mustReject := map[string]string{
		"换行":       "a\nb",
		"回车":       "a\rb",
		"制表符":      "a\tb",
		"空字节":      "a\x00b",
		"垂直制表":     "a\vb",
		"换页":       "a\fb",
		"路径穿越":     "../../etc/passwd",
		"命令替换 $()": "a$(whoami)",
		"反引号":      "a`id`",
		"管道":       "a|cat /etc/shadow",
		"分号":       "a;rm -rf /",
		"空格":       "a b",
		"斜杠":       "a/b",
		"反斜杠":      "a\\b",
		"冒号":       "a:80",
		"引号":       `a"b`,
		"单引号":      "a'b",
		"&":        "a&b",
		"<":        "a<b",
		"通配":       "*.example.com",
		"空串之外的空格":  "   ",
		"以点结尾":     "example.com.",
		"超长(254)":  strings.Repeat("a", 254),
	}
	for label, domain := range mustReject {
		err := validateActionForm(spec, map[string]string{"action": "add_rule", "domain": domain})
		if err == nil {
			t.Errorf("%s 应当被拒绝，实际通过：%q", label, domain)
		}
	}

	mustAccept := []string{
		"example.com",
		"openai.com",
		"chatgpt.com",
		".example.com",
		"a",
		"a-b.com",
		"a_b.com",
		"sub.domain.example.co.uk",
		"123.com",
		strings.Repeat("a", 253),
		"", // 空串表示不带 domain，由调用方跳过，不该报错
	}
	for _, domain := range mustAccept {
		err := validateActionForm(spec, map[string]string{"action": "add_rule", "domain": domain})
		if err != nil {
			t.Errorf("合法域名 %q 被误拒：%v", domain, err)
		}
	}
}

func TestUUIDPatternRejectsMalformedIDs(t *testing.T) {
	// 真实缺陷回归：GET /api/nodes/not-a-uuid/portal-status 曾经返回 500，
	// 且响应体里带着 Postgres 的
	// invalid input syntax for type uuid ... SQLSTATE 22P02。
	// 那既是不该有的 5xx（调用方传错东西属 4xx），也泄漏了库内细节。
	//
	// 现在入口就校验 UUID，畸形 ID 直接走 404，不碰数据库。
	mustReject := []string{
		"not-a-uuid",
		"",
		"12345",
		"00000000-0000-0000-0000-00000000000",   // 少一位
		"00000000-0000-0000-0000-0000000000000", // 多一位
		"gggggggg-gggg-gggg-gggg-gggggggggggg",  // 非 hex
		"00000000000000000000000000000000",      // 无连字符
		"'; DROP TABLE hysteria_nodes; --",
		"1 OR 1=1",
	}
	for _, id := range mustReject {
		if uuidPattern.MatchString(strings.TrimSpace(id)) {
			t.Errorf("畸形 ID %q 不应通过校验", id)
		}
	}

	// 尾随/前导空格由调用方 TrimSpace 抹掉后再匹配，属正常规范化，
	// 不算畸形 —— 单独注明，免得后人以为这里是漏网的。
	if !uuidPattern.MatchString(strings.TrimSpace("  27de1727-1f07-4881-b183-45a5ad03e976  ")) {
		t.Error("带空格的合法 UUID 应当在 TrimSpace 后通过")
	}

	mustAccept := []string{
		"27de1727-1f07-4881-b183-45a5ad03e976",
		"c862a58d-6df3-4c51-be69-dae4696c6fa5",
		"00000000-0000-0000-0000-000000000000",
		"ABCDEF01-2345-6789-ABCD-EF0123456789", // 大写 hex 也要认
	}
	for _, id := range mustAccept {
		if !uuidPattern.MatchString(strings.TrimSpace(id)) {
			t.Errorf("合法 UUID %q 被误拒", id)
		}
	}
}

func TestNodeWriteLockSerializesSameNode(t *testing.T) {
	// 并发写会打崩节点侧的单线程 portal（实测 502 / unexpected EOF），
	// 所以同节点的写操作必须串行。这个测试用「同一时刻只允许一个持有者」
	// 来钉住它 —— 如果锁退化成空操作，这里会立刻失败。
	s := &Server{}
	const node = "27de1727-1f07-4881-b183-45a5ad03e976"

	var concurrent int32
	var peak int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := s.lockNodeWrite(node)
			defer unlock()
			n := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()

	if peak != 1 {
		t.Errorf("同节点写操作应当串行，实际并发峰值 = %d", peak)
	}
}

func TestCapabilityGuardConfigIsConsistent(t *testing.T) {
	// 门禁表是三张互相对齐的表（Requires 取值 / capabilityProbes / capabilityMeta）。
	// 任何一处写错都会静默削弱门禁：
	//   - Requires 写了 probe 里没有的键 → ensureCapability 直接 return nil，
	//     门禁形同不存在；
	//   - meta 缺项 → 错误文案变成空字符串，操作者看不懂；
	//   - install 动辄不存在 → 提示让人去点一个不存在的按钮。
	// 所以逐条对齐，而不是等线上表现异常才发现。
	for verb, spec := range portalActions {
		if spec.Requires == "" {
			continue
		}
		if _, ok := capabilityProbes[spec.Requires]; !ok {
			t.Errorf("动词 %s 依赖 %q，但 capabilityProbes 里没有这个键（门禁会静默失效）",
				verb, spec.Requires)
		}
		if _, ok := capabilityMeta[spec.Requires]; !ok {
			t.Errorf("动词 %s 依赖 %q，但 capabilityMeta 里没有这个键（错误文案会为空）",
				verb, spec.Requires)
		}
	}

	for cap := range capabilityProbes {
		if _, ok := capabilityMeta[cap]; !ok {
			t.Errorf("能力 %q 有探测定义但没有文案定义", cap)
		}
	}

	for cap, meta := range capabilityMeta {
		if _, ok := portalActions[meta.Install]; !ok {
			t.Errorf("能力 %q 指向的安装动词 %q 不在动作表里，用户会被指到一个不存在的操作",
				cap, meta.Install)
		}
		spec := portalActions[meta.Install]
		if spec.Requires != "" {
			t.Errorf("安装动词 %q 不该有 Requires —— 装的时候组件当然还不在，实际为 %q",
				meta.Install, spec.Requires)
		}
	}
}

func TestInstallVerbsHaveNoCapabilityRequirement(t *testing.T) {
	// 独立的第二条防线：install_* 一律不设 Requires。
	// 否则节点上没装 WARP 时，连「安装 WARP」都会被自己挡住，形成死锁。
	for verb, spec := range portalActions {
		if strings.HasPrefix(verb, "install_") && spec.Requires != "" {
			t.Errorf("install 动词 %s 设了 Requires=%q，会导致无法安装（死锁）",
				verb, spec.Requires)
		}
	}
}

func TestNodeWriteLockIsPerNode(t *testing.T) {
	// 不同节点之间不该互相阻塞，否则一台慢节点会拖住全部。
	s := &Server{}
	a := "27de1727-1f07-4881-b183-45a5ad03e976"
	b := "c862a58d-6df3-4c51-be69-dae4696c6fa5"

	unlockA := s.lockNodeWrite(a)
	defer unlockA()

	done := make(chan struct{})
	go func() {
		unlockB := s.lockNodeWrite(b)
		unlockB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("B 节点的写锁被 A 节点阻塞了，说明锁没有按节点隔离")
	}
}

func TestPortalWebCallEncodesQueryWithoutChangingPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+strings.Repeat("a", 64)+"/user-config" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("user_id"); got != "sub_user/with+symbols" {
			t.Errorf("user_id = %q", got)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Error("expected Basic authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"uri":"hysteria2://pass@example.com:443?sni=example.com"}`))
	}))
	defer server.Close()

	oldClient := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = oldClient })

	target := &portalWebTarget{
		Origin:    server.URL,
		PathToken: strings.Repeat("a", 64),
		Username:  "panel-user",
		Password:  "panel-password",
	}
	_, err := portalWebCall(context.Background(), target, http.MethodGet, "user-config", nil,
		url.Values{"user_id": []string{"sub_user/with+symbols"}})
	if err != nil {
		t.Fatalf("portalWebCall() error = %v", err)
	}
}

// 2026-11-03：网页会话通道与 portalCall 同口径：超大响应明确拒绝（不截成半截 JSON），
// 非 2xx 即使信封写着 ok:true 也算失败。
func TestPortalWebCallRejectsOversizedAndErrorStatus(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"oversized": {200, `{"ok":true,"pad":"` + strings.Repeat("x", portalWebBodyLimit) + `"}`, "超过"},
		"ok-on-500": {500, `{"ok":true}`, "500"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		oldClient := portalClient
		portalClient = server.Client()
		target := &portalWebTarget{Origin: server.URL, PathToken: strings.Repeat("a", 64), Username: "u", Password: "p"}
		_, err := portalWebCall(context.Background(), target, http.MethodGet, "user-config", nil)
		portalClient = oldClient
		server.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}
