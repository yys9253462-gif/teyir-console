package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The subscription payload is what a client actually imports, so these tests pin
// the wire format field by field. A silent field rename would produce a link that
// imports cleanly and simply does not connect — the worst failure mode, because
// nothing reports an error.

func sampleNode() subscriptionNode {
	return subscriptionNode{
		ID:        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Name:      "hy2-tokyo-8443",
		Host:      "203.0.113.10",
		Port:      8443,
		Password:  "p@ss word/with+chars",
		SNI:       "hy2.example.com",
		Insecure:  true,
		PinSHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		ObfsPass:  "obfs-secret",
		HopRange:  "20000-40000",
	}
}

func TestApplySubscriptionClientURIImportsObfsAndTrustSettings(t *testing.T) {
	node := sampleNode()
	uri := "hysteria2://p%40ss%20word%2Fwith%2Bchars@203.0.113.10:8443" +
		"?sni=hy2.example.com&insecure=1&pinSHA256=" + node.PinSHA256 +
		"&obfs=salamander&obfs-password=sal%21secret&mport=20000-40000#node"

	got, err := applySubscriptionClientURI(node, uri)
	if err != nil {
		t.Fatalf("applySubscriptionClientURI() error = %v", err)
	}
	if got.ObfsPass != "sal!secret" {
		t.Errorf("ObfsPass = %q, want decoded Salamander password", got.ObfsPass)
	}
	if !got.Insecure || got.PinSHA256 != node.PinSHA256 || got.HopRange != "20000-40000" {
		t.Errorf("client trust/transport settings not copied: insecure=%v pin=%q hop=%q", got.Insecure, got.PinSHA256, got.HopRange)
	}
	if got.Password != node.Password || got.Host != node.Host || got.Port != node.Port {
		t.Errorf("URI must not replace service-owned credentials or endpoint: %+v", got)
	}
}

func TestApplySubscriptionClientURIRejectsMismatchedIdentityAndIncompleteObfs(t *testing.T) {
	node := sampleNode()
	cases := []struct {
		name string
		uri  string
	}{
		{
			name: "credential mismatch",
			uri:  "hysteria2://wrong@203.0.113.10:8443?sni=hy2.example.com",
		},
		{
			name: "endpoint mismatch",
			uri:  "hysteria2://p%40ss%20word%2Fwith%2Bchars@203.0.113.11:8443?sni=hy2.example.com",
		},
		{
			name: "missing salamander password",
			uri:  "hysteria2://p%40ss%20word%2Fwith%2Bchars@203.0.113.10:8443?sni=hy2.example.com&obfs=salamander",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := applySubscriptionClientURI(node, tc.uri); err == nil {
				t.Fatal("expected URI to be rejected")
			}
		})
	}
}

// A space in the password must become %20, not "+". url.QueryEscape would emit
// "+", and in the URI's userinfo section that is a literal plus — the client
// would then authenticate with the wrong password and just fail to connect.
func TestPctEncodeEncodesSpaceAsPercent20(t *testing.T) {
	got := pctEncode("p@ss word/with+chars")
	want := "p%40ss%20word%2Fwith%2Bchars"
	if got != want {
		t.Errorf("pctEncode 结果不符\n got=%s\nwant=%s", got, want)
	}
	if strings.Contains(got, "+") {
		t.Errorf("pctEncode 不应产生加号（会被当成字面加号）：%s", got)
	}
	// Unreserved characters must pass through untouched.
	if pctEncode("aA0-_.~") != "aA0-_.~" {
		t.Errorf("unreserved 字符不应被编码：%s", pctEncode("aA0-_.~"))
	}
}

func TestHysteria2URIReflectsEveryOption(t *testing.T) {
	uri := hysteria2URI(sampleNode())

	if !strings.HasPrefix(uri, "hysteria2://p%40ss%20word%2Fwith%2Bchars@203.0.113.10:8443?") {
		t.Errorf("URI 前缀不符（口令或 host:port 编码有误）：%s", uri)
	}
	for _, want := range []string{
		"sni=hy2.example.com",
		"insecure=1",
		"pinSHA256=abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"obfs=salamander",
		"obfs-password=obfs-secret",
		"mport=20000-40000",
		"#hy2-tokyo-8443",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI 缺少 %q：%s", want, uri)
		}
	}
}

// A trusted-cert node must not carry insecure/pin: emitting them would make
// clients skip verification on a node that actually has a valid certificate.
func TestHysteria2URIWithTrustedCertOmitsInsecureAndPin(t *testing.T) {
	node := sampleNode()
	node.Insecure = false
	uri := hysteria2URI(node)
	if strings.Contains(uri, "insecure") {
		t.Errorf("可信证书节点不应带 insecure：%s", uri)
	}
	if strings.Contains(uri, "pinSHA256") {
		t.Errorf("可信证书节点不应带 pinSHA256：%s", uri)
	}
	if !strings.Contains(uri, "sni=hy2.example.com") {
		t.Errorf("sni 应始终存在：%s", uri)
	}
}

func TestValidPinSHA256(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	if got := validPinSHA256(hex64); got != hex64 {
		t.Errorf("合法 HEX64 应通过，得到 %q", got)
	}
	if got := validPinSHA256(strings.ToUpper(hex64)); got != hex64 {
		t.Errorf("大写应被规范化为小写，得到 %q", got)
	}
	// A base64 pin is the historical bug: Xray reads this field as hex and aborts
	// the whole config build with `encoding/hex: invalid byte`.
	base64ish := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if len(base64ish) == 64 {
		if got := validPinSHA256(base64ish); got != "" {
			t.Errorf("base64 形态的 pin 必须被丢弃，得到 %q", got)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if got := validPinSHA256(bad); got != "" {
			t.Errorf("非法 pin %q 应被丢弃，得到 %q", bad, got)
		}
	}
}

func TestRenderClashShape(t *testing.T) {
	nodes := []subscriptionNode{sampleNode()}
	body, err := renderClash(nodes)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	proxies, ok := doc["proxies"].([]any)
	if !ok || len(proxies) != 1 {
		t.Fatalf("proxies 应为 1 条，得到 %v", doc["proxies"])
	}
	proxy := proxies[0].(map[string]any)
	if proxy["type"] != "hysteria2" || proxy["server"] != "203.0.113.10" || proxy["password"] != sampleNode().Password {
		t.Errorf("proxy 关键字段不符: %#v", proxy)
	}
	// skip-cert-verify must mirror is_insecure, or a self-signed node fails TLS.
	if proxy["skip-cert-verify"] != true {
		t.Errorf("自签节点 skip-cert-verify 应为 true: %#v", proxy)
	}
	if proxy["obfs"] != "salamander" || proxy["obfs-password"] != "obfs-secret" {
		t.Errorf("obfs 字段缺失: %#v", proxy)
	}
	// Port hopping is expressed as "port,range" in a separate ports field.
	if proxy["ports"] != "8443,20000-40000" {
		t.Errorf("跳段节点 ports 应为 8443,20000-40000，得到 %#v", proxy["ports"])
	}

	groups, ok := doc["proxy-groups"].([]any)
	if !ok {
		t.Fatalf("缺少 proxy-groups: %v", doc["proxy-groups"])
	}
	byName := map[string]map[string]any{}
	for _, raw := range groups {
		group := raw.(map[string]any)
		byName[group["name"].(string)] = group
	}
	for name, typ := range map[string]string{groupSelect: "select", groupAuto: "url-test", groupFallback: "fallback"} {
		g := byName[name]
		if g == nil {
			t.Fatalf("缺少代理组 %s: %v", name, groups)
		}
		if g["type"] != typ {
			t.Errorf("%s 应为 %s，得到 %v", name, typ, g["type"])
		}
	}
	for _, name := range []string{groupAuto, groupFallback} {
		if byName[name]["url"] == "" || byName[name]["interval"] == nil {
			t.Errorf("%s 组必须有 url 与 interval: %#v", name, byName[name])
		}
	}
	// 节点选择：首项是自动选择（默认即自动），末项是 DIRECT（可完全绕过代理），中间含节点。
	members := byName[groupSelect]["proxies"].([]any)
	if members[0] != groupAuto {
		t.Errorf("节点选择首项应为自动选择，得到 %v", members[0])
	}
	if members[len(members)-1] != "DIRECT" {
		t.Errorf("节点选择末项应为 DIRECT，得到 %v", members[len(members)-1])
	}
	if !strings.Contains(members[len(members)-2].(string), "hy2-tokyo-8443") {
		t.Errorf("节点选择应包含节点名，得到 %v", members)
	}

	// 规则：国内直连在兜底之前，最后一条必须是 MATCH 到节点选择；所有引用的组都必须存在。
	rules := doc["rules"].([]any)
	if rules[len(rules)-1] != "MATCH,"+groupSelect {
		t.Errorf("最后一条规则应为 MATCH,%s，得到 %v", groupSelect, rules[len(rules)-1])
	}
	hasCN := false
	for _, raw := range rules {
		rule := raw.(string)
		if rule == "GEOIP,CN,DIRECT" {
			hasCN = true
		}
		if strings.HasPrefix(rule, "GEOSITE") || strings.HasPrefix(rule, "RULE-SET") {
			t.Errorf("不得使用 GEOSITE / RULE-SET（Stash 与老 Clash 不支持）：%s", rule)
		}
		parts := strings.Split(rule, ",")
		target := parts[len(parts)-1]
		if target == "no-resolve" {
			target = parts[len(parts)-2]
		}
		if target != "DIRECT" && target != "REJECT" && byName[target] == nil {
			t.Errorf("规则引用了不存在的组 %q: %s", target, rule)
		}
	}
	if !hasCN {
		t.Error("缺少 GEOIP,CN,DIRECT")
	}
	if doc["mixed-port"] == nil {
		t.Error("缺少 mixed-port")
	}
}

func TestRenderSingBoxShape(t *testing.T) {
	nodes := []subscriptionNode{sampleNode()}
	body, err := renderSingBox(nodes)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	outbounds := doc["outbounds"].([]any)
	// 1 node + selector + urltest + direct
	if len(outbounds) != 4 {
		t.Fatalf("应有 4 个 outbound，得到 %d", len(outbounds))
	}
	byTag := map[string]map[string]any{}
	for _, raw := range outbounds {
		out := raw.(map[string]any)
		byTag[out["tag"].(string)] = out
	}
	node := byTag["hy2-tokyo-8443"]
	if node == nil || node["type"] != "hysteria2" || node["password"] != sampleNode().Password {
		t.Fatalf("节点 outbound 不符: %#v", node)
	}
	if _, exists := node["server_port"]; exists {
		t.Error("跳段节点不应同时有 server_port")
	}
	if node["server_ports"] == nil {
		t.Error("跳段节点必须有 server_ports")
	}
	// sing-box 要求每一项都是 "起:止" 区间，单个端口写成 "8443" 会让整份配置加载失败。
	for _, p := range node["server_ports"].([]any) {
		if !strings.Contains(p.(string), ":") {
			t.Errorf("server_ports 每项都必须是 起:止 区间，得到 %q", p)
		}
	}
	tlsBlock, ok := node["tls"].(map[string]any)
	if !ok || tlsBlock["enabled"] != true || tlsBlock["insecure"] != true || tlsBlock["server_name"] != "hy2.example.com" {
		t.Errorf("tls 块不符: %#v", node["tls"])
	}
	if byTag["PROXY"] == nil || byTag["PROXY"]["type"] != "selector" {
		t.Error("缺少 selector 类型的 PROXY")
	}
	if byTag["AUTO"] == nil || byTag["AUTO"]["type"] != "urltest" {
		t.Error("缺少 urltest 类型的 AUTO")
	}
	if byTag["DIRECT"] == nil || byTag["DIRECT"]["type"] != "direct" {
		t.Error("缺少 direct")
	}
}

// Without port hopping the outbound must use server_port; emitting server_ports
// with a single value is accepted by some sing-box builds and rejected by others.
func TestRenderSingBoxWithoutHopUsesServerPort(t *testing.T) {
	node := sampleNode()
	node.HopRange = ""
	body, err := renderSingBox([]subscriptionNode{node})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if !strings.Contains(string(body), `"server_port": 8443`) {
		t.Errorf("无跳段时应使用 server_port：%s", body)
	}
	if strings.Contains(string(body), "server_ports") {
		t.Errorf("无跳段时不应出现 server_ports：%s", body)
	}
}

func TestRenderURIListIsBase64OfLines(t *testing.T) {
	nodes := []subscriptionNode{sampleNode(), sampleNode()}
	nodes[1].Name = "hy2-se-19984"
	nodes[1].Host = "203.0.113.20"
	nodes[1].Port = 19984
	body := renderURIList(nodes)

	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("输出不是合法 base64: %v", err)
	}
	lines := strings.Split(string(decoded), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，得到 %d", len(lines))
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "hysteria2://") {
			t.Errorf("第 %d 行不是 hysteria2 URI: %s", i, line)
		}
	}
	if !strings.Contains(lines[1], "203.0.113.20:19984") {
		t.Errorf("第二行 host:port 不符: %s", lines[1])
	}
}

func TestNegotiateSubscriptionFormat(t *testing.T) {
	cases := []struct {
		ua     string
		query  string
		expect string
	}{
		{"clash-verge/v1.5.0", "", subscriptionFormatClash},
		{"mihomo/1.18.0", "", subscriptionFormatClash},
		{"ClashMetaForAndroid/2.9", "", subscriptionFormatClash},
		{"sing-box 1.8.0", "", subscriptionFormatSingBox},
		{"SFA/1.8.0 (io.nekohasekai.sfa)", "", subscriptionFormatSingBox},
		{"v2rayNG/1.8.5", "", subscriptionFormatURI},
		{"Hysteria/2.6.1", "", subscriptionFormatURI},
		{"", "", subscriptionFormatClash},
		{"clash-verge/v1.5.0", "?format=singbox", subscriptionFormatSingBox},
		{"sing-box 1.8.0", "?format=uri", subscriptionFormatURI},
		{"", "?format=base64", subscriptionFormatURI},
		{"", "?format=yaml", subscriptionFormatClash},
		// An unknown ?format= must fall back to UA detection, not error out.
		{"clash-verge/v1.5.0", "?format=bogus", subscriptionFormatClash},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "/sub/token"+tc.query, nil)
		if tc.ua != "" {
			req.Header.Set("User-Agent", tc.ua)
		}
		if got := negotiateSubscriptionFormat(req); got != tc.expect {
			t.Errorf("UA=%q query=%q 期望 %s，得到 %s", tc.ua, tc.query, tc.expect, got)
		}
	}
}

// The management API tells the operator which nodes are in the link. It must not
// hand back the master password that the link itself is built from — the panel
// keeps that server-side, and a leak here would expose every node at once.
func TestSubscriptionNodeRedactionDropsCredentials(t *testing.T) {
	encoded, err := json.Marshal(redactSubscriptionNodes([]subscriptionNode{sampleNode()}))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	body := string(encoded)
	for _, secret := range []string{sampleNode().Password, sampleNode().ObfsPass, sampleNode().PinSHA256} {
		if strings.Contains(body, secret) {
			t.Errorf("脱敏后仍含机密 %q：%s", secret, body)
		}
	}
	for _, key := range []string{"Password", "password", "ObfsPass", "PinSHA256"} {
		if strings.Contains(body, key) {
			t.Errorf("脱敏后仍暴露字段 %q：%s", key, body)
		}
	}
	// Routing facts must survive, or the UI cannot show what is included.
	for _, want := range []string{"hy2-tokyo-8443", "203.0.113.10", "has_obfs"} {
		if !strings.Contains(body, want) {
			t.Errorf("脱敏后丢失了 %q：%s", want, body)
		}
	}
}

func TestSubscriptionNotFoundMatchesUnknownToken(t *testing.T) {
	// Same status and body regardless of whether the token exists, so the endpoint
	// gives no oracle for guessing.
	recorder := httptest.NewRecorder()
	subscriptionNotFound(recorder)
	if recorder.Code != 404 {
		t.Errorf("应为 404，得到 %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "token") {
		t.Errorf("404 正文不应提及 token：%s", recorder.Body.String())
	}
}

// Duplicate proxy names are silently fatal in Clash/sing-box: the client drops
// one node and reports nothing. Collisions must therefore be resolved, and
// deterministically so a re-import does not shuffle the list.
func TestDedupeNodeNamesResolvesCollisions(t *testing.T) {
	nodes := dedupeNodeNames([]subscriptionNode{
		{Name: "hy2-01", Port: 443},
		{Name: "hy2-01", Port: 8443},
		// Same name and same port: the port suffix is not enough, so the counter
		// path must kick in rather than looping forever.
		{Name: "hy2-01", Port: 8443},
		{Name: "hy2-02", Port: 443},
	})
	if nodes[0].Name != "hy2-01" {
		t.Errorf("首个节点应保留原名，得到 %q", nodes[0].Name)
	}
	if nodes[1].Name != "hy2-01-8443" {
		t.Errorf("第二个重名节点应带端口后缀，得到 %q", nodes[1].Name)
	}
	if nodes[2].Name != "hy2-01-8443-x" {
		t.Errorf("同名同端口应继续加后缀，得到 %q", nodes[2].Name)
	}
	if nodes[3].Name != "hy2-02" {
		t.Errorf("未冲突的节点不应被改名，得到 %q", nodes[3].Name)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.Name] {
			t.Errorf("仍存在重名：%q", n.Name)
		}
		seen[n.Name] = true
	}
	// Dedupe must be idempotent: applying it again must not rename anything.
	again := dedupeNodeNames(append([]subscriptionNode(nil), nodes...))
	for i := range nodes {
		if again[i].Name != nodes[i].Name {
			t.Errorf("第二次去重不应再改名：%q → %q", nodes[i].Name, again[i].Name)
		}
	}
}

func TestNewSubscriptionTokenIsLongAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		token, err := newSubscriptionToken()
		if err != nil {
			t.Fatalf("生成令牌失败: %v", err)
		}
		// 32 random bytes in base64url without padding.
		if len(token) != 43 {
			t.Fatalf("令牌长度应为 43，得到 %d（%q）", len(token), token)
		}
		if strings.ContainsAny(token, "+/= ") {
			t.Errorf("base64url 令牌不应含 +/= 或空格：%q", token)
		}
		if seen[token] {
			t.Fatalf("令牌重复：%q", token)
		}
		seen[token] = true
	}
}

func TestSubscriptionPublicURLHonoursForwardedHeaders(t *testing.T) {
	// Behind the bundled Caddy: https + original Host, so the operator gets the
	// address they should actually paste into a client.
	req := httptest.NewRequest("GET", "/api/subscription", nil)
	req.Host = "panel.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := subscriptionPublicURL(req, "TOKEN"); got != "https://panel.example.com/sub/TOKEN" {
		t.Errorf("反代场景 URL 不符：%s", got)
	}

	// Direct HTTP access keeps http.
	plain := httptest.NewRequest("GET", "/api/subscription", nil)
	plain.Host = "127.0.0.1:8080"
	if got := subscriptionPublicURL(plain, "TOKEN"); got != "http://127.0.0.1:8080/sub/TOKEN" {
		t.Errorf("直连场景 URL 不符：%s", got)
	}

	// An explicit forwarded host wins over the proxied Host header.
	withHost := httptest.NewRequest("GET", "/api/subscription", nil)
	withHost.Host = "internal:8080"
	withHost.Header.Set("X-Forwarded-Proto", "https")
	withHost.Header.Set("X-Forwarded-Host", "outer.example.com")
	if got := subscriptionPublicURL(withHost, "TOKEN"); got != "https://outer.example.com/sub/TOKEN" {
		t.Errorf("X-Forwarded-Host 场景 URL 不符：%s", got)
	}

	// With no Host at all the path alone is still returned rather than a broken URL.
	empty := httptest.NewRequest("GET", "/api/subscription", nil)
	empty.Host = ""
	if got := subscriptionPublicURL(empty, "TOKEN"); got != "/sub/TOKEN" {
		t.Errorf("无 Host 时应退回相对路径，得到 %s", got)
	}
}

func TestClashGroupsStayMinimal(t *testing.T) {
	names := []string{"美国-哥伦布", "美国-哥伦布-Reality", "日本-东京", "哥伦布-AWS"}
	groups := clashProxyGroups(names)
	if len(groups) != 4 {
		t.Fatalf("应只有 4 个分组（节点选择 / 自动选择 / 故障转移 / AI），得到 %d: %v", len(groups), groups)
	}
	ai := groups[3]
	if ai["name"] != groupAI || len(ai["proxies"].([]string)) != 2 {
		t.Fatalf("AI 组应只含 2 个美国节点（含 Reality）: %v", ai)
	}
	// 没有美国节点：不生成 AI 组，AI 规则也不能引用它。
	if g := clashProxyGroups([]string{"日本-东京"}); len(g) != 3 {
		t.Fatalf("无美国节点时应为 3 个分组，得到 %d", len(g))
	}
	for _, r := range clashRules(false) {
		if strings.Contains(r, groupAI) {
			t.Fatalf("无 AI 组时规则不得引用它: %s", r)
		}
	}
	// 空订阅：组成员不能为空，否则 Clash 拒绝整份配置。
	for _, g := range clashProxyGroups(nil) {
		if p, ok := g["proxies"].([]string); ok && len(p) == 0 {
			t.Errorf("空订阅时 %v 组成员为空", g["name"])
		}
	}
}
