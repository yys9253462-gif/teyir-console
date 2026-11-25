package httpapi

// xuipanel_test.go — 3X-UI 对接的纯逻辑单测。
//
// 这些用例刻意不依赖数据库与真实面板：对方面板的真实响应形态只能靠
// 契约文档钉住，而契约的三处最容易改坏的地方正好都是纯函数 ——
// URL 归一（反代子路径）、inbound 归一（settings 是字符串还是对象）、
// clientId 的协议差异（id / password / email）。任何一个改错，UI 上
// 表现都是「面板连上了但列表是空的」，最难从现象反推。

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. URL 归一：反代子路径必须保留
// ---------------------------------------------------------------------------

func TestNormalizeXUIBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		// 裸地址补 scheme，去掉尾斜杠。
		{name: "裸地址补 https", input: "panel.example.com:2053", want: "https://panel.example.com:2053"},
		{name: "去尾斜杠", input: "https://panel.example.com:2053/", want: "https://panel.example.com:2053"},
		// 3X-UI 常挂在反代子路径下，这一段必须保留 —— 丢了它 API 全部 404。
		{name: "保留反代子路径", input: "https://example.com/xui/", want: "https://example.com/xui"},
		{name: "保留多级子路径", input: "https://example.com/a/b/c", want: "https://example.com/a/b/c"},
		// 拒绝项。
		{name: "空地址", input: "", wantErr: true},
		{name: "非 http(s)", input: "ftp://example.com", wantErr: true},
		{name: "带账号", input: "https://user:pass@example.com", wantErr: true},
		{name: "带查询参数", input: "https://example.com/?a=1", wantErr: true},
		{name: "子路径含非法字符", input: "https://example.com/a b", wantErr: true},
		{name: "子路径含分号注入", input: "https://example.com/xui;rm -rf", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeXUIBaseURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("归一结果不符: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestXUIAPIPrefixKeepsSubpath(t *testing.T) {
	if got := xuiAPIPrefix("https://example.com/xui"); got != "https://example.com/xui/panel/api" {
		t.Fatalf("子路径前缀拼错: %s", got)
	}
	if got := xuiAPIPrefix("https://example.com"); got != "https://example.com/panel/api" {
		t.Fatalf("根路径前缀拼错: %s", got)
	}
}

// ---------------------------------------------------------------------------
// 2. inbound 归一：settings 的两种形态都要认
// ---------------------------------------------------------------------------

func TestNormalizeXUIInboundWithStringSettings(t *testing.T) {
	// 3X-UI 把 settings 序列化成字符串存进 DB，list 返回的也是字符串。
	item := map[string]any{
		"id":         float64(3),
		"port":       float64(443),
		"protocol":   "vless",
		"remark":     "主入口",
		"enable":     true,
		"up":         float64(1024),
		"down":       float64(2048),
		"total":      float64(0),
		"expiryTime": float64(0),
		"settings":   `{"clients":[{"id":"uuid-1","email":"a@b.com"},{"id":"uuid-2","email":"c@d.com"}],"decryption":"none"}`,
	}
	got := normalizeXUIInbound(item)
	if got.ID != 3 || got.Port != 443 || got.Protocol != "vless" || got.Remark != "主入口" {
		t.Fatalf("基础字段错: %+v", got)
	}
	if len(got.Clients) != 2 {
		t.Fatalf("应该解析出 2 个客户端，得到 %d", len(got.Clients))
	}
	if got.UpBytes != 1024 || got.DownBytes != 2048 {
		t.Fatalf("流量字段错: up=%d down=%d", got.UpBytes, got.DownBytes)
	}
}

func TestNormalizeXUIInboundWithObjectSettings(t *testing.T) {
	// 部分版本/中间层会把 settings 直接给成对象。
	item := map[string]any{
		"id": float64(7), "port": float64(8443), "protocol": "vmess",
		"settings": map[string]any{
			"clients": []any{
				map[string]any{"id": "uuid-x", "email": "x@y.com"},
			},
		},
	}
	got := normalizeXUIInbound(item)
	if len(got.Clients) != 1 {
		t.Fatalf("对象形态 settings 应解析出 1 个客户端，得到 %d", len(got.Clients))
	}
}

func TestNormalizeXUIInboundToleratesMissingFields(t *testing.T) {
	// 字段缺失时取零值而不是崩 —— 3X-UI 版本间字段增减频繁。
	got := normalizeXUIInbound(map[string]any{})
	if got.ID != 0 || got.Protocol != "" || len(got.Clients) != 0 {
		t.Fatalf("空 inbound 应得到零值: %+v", got)
	}
	if got.Clients == nil {
		t.Fatalf("Clients 应为空切片而非 nil，否则 JSON 序列化成 null")
	}
}

func TestNormalizeXUIInboundNumericStrings(t *testing.T) {
	// 存疑形态：数字以字符串给出（3X-UI 某些版本确实这样）。
	item := map[string]any{"id": "9", "port": "2053", "up": "4096"}
	got := normalizeXUIInbound(item)
	if got.ID != 9 || got.Port != 2053 || got.UpBytes != 4096 {
		t.Fatalf("数字字符串未正确解析: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 3. clientId 提取：三种协议三种来源
// ---------------------------------------------------------------------------

func TestXUIClientIdentity(t *testing.T) {
	cases := []struct {
		name     string
		client   xuiClientSnapshot
		protocol string
		wantID   string
		wantMai  string
	}{
		{
			// vless / vmess：clientId 取 id（uuid）。
			name:   "vless 用 id",
			client: map[string]any{"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "email": "v@1.com"},
			wantID: "b831381d-6324-4d53-ad4f-8cda48b30811", wantMai: "v@1.com",
		},
		{
			// trojan：没有 id，clientId 取 password。
			name:   "trojan 用 password",
			client: map[string]any{"password": "secret-pass", "email": "t@1.com"},
			wantID: "secret-pass", wantMai: "t@1.com",
		},
		{
			// shadowsocks：id 与 password 都可能缺，退到 email。
			name:   "ss 退到 email",
			client: map[string]any{"email": "s@1.com", "method": "aes-128-gcm"},
			wantID: "s@1.com", wantMai: "s@1.com",
		},
		{
			// 2026-11-03：shadowsocks 客户端带 password，但 3X-UI 删除时按 email 匹配。
			name:     "ss 带 password 仍用 email",
			client:   map[string]any{"email": "s2@1.com", "password": "ss-pass", "method": "2022-blake3-aes-128-gcm"},
			protocol: "shadowsocks",
			wantID:   "s2@1.com", wantMai: "s2@1.com",
		},
		{
			// 数值型 id（旧版 vmess 可能给数字）。
			name:   "数值 id",
			client: map[string]any{"id": float64(42), "email": "n@1.com"},
			wantID: "42", wantMai: "n@1.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email, clientID := xuiClientIdentity(tc.client, tc.protocol)
			if email != tc.wantMai {
				t.Fatalf("email 不符: got %q want %q", email, tc.wantMai)
			}
			if clientID != tc.wantID {
				t.Fatalf("clientID 不符: got %q want %q", clientID, tc.wantID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. 响应信封解析
// ---------------------------------------------------------------------------

func TestParseXUIResponse(t *testing.T) {
	// 成功。
	ok, err := parseXUIResponse([]byte(`{"success":true,"msg":"","obj":[]}`), 200)
	if err != nil {
		t.Fatalf("成功响应被拒: %v", err)
	}
	if !ok.Success {
		t.Fatalf("success 字段未解析")
	}
	// 业务失败：msg 必须透出给操作者，否则「为什么失败」全靠猜。
	_, err = parseXUIResponse([]byte(`{"success":false,"msg":"invalid credentials"}`), 200)
	if err == nil || err.Error() != "3X-UI 报错: invalid credentials" {
		t.Fatalf("业务错误未正确透出: %v", err)
	}
	// 业务失败但 msg 为空：不能把空串当错误抛给操作者。
	_, err = parseXUIResponse([]byte(`{"success":false,"msg":""}`), 200)
	if err == nil || err.Error() != "3X-UI 报错: 未知错误" {
		t.Fatalf("空 msg 未兜底: %v", err)
	}
	// 404 常见于地址带了反代子路径却漏填 —— 要给可操作的提示。
	_, err = parseXUIResponse([]byte(`not json`), 404)
	if err == nil || !containsAll(err.Error(), "404", "子路径") {
		t.Fatalf("404 提示不具操作性: %v", err)
	}
	// 401/403 提示重新保存凭据。
	_, err = parseXUIResponse([]byte(`{}`), 401)
	if err == nil || !containsAll(err.Error(), "凭据") {
		t.Fatalf("401 提示不具操作性: %v", err)
	}
	// 200 但不是 JSON：多半是把别的服务当成了 3X-UI。
	_, err = parseXUIResponse([]byte(`<html>hello</html>`), 200)
	if err == nil || !containsAll(err.Error(), "JSON") {
		t.Fatalf("非 JSON 响应提示不对: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. 客户端构造：入参受白名单约束
// ---------------------------------------------------------------------------

func TestXUIClientPayloadNeverCarriesInjectedFields(t *testing.T) {
	payload := xuiClientPayload(xuiAddClientInput{InboundID: 1, Email: "a@b.com"})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造 client 失败: %v", err)
	}
	// 只应包含服务端显式设置的那些键，不多不少。
	allowed := map[string]bool{
		"email": true, "enable": true, "limitIp": true, "totalGB": true,
		"expiryTime": true, "tgId": true, "subId": true, "reset": true,
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	for key := range decoded {
		if !allowed[key] {
			t.Fatalf("client 里出现未预期字段 %q", key)
		}
	}
	// subId 必须随机非空（3X-UI 用它做订阅路径段）。
	if subID, _ := decoded["subId"].(string); len(subID) < 8 {
		t.Fatalf("subId 太短或缺失: %q", subID)
	}
	// 永不过期时 expiryTime 必须是 0（3X-UI 的语义），而不是当前时间。
	if expiry, _ := decoded["expiryTime"].(float64); expiry != 0 {
		t.Fatalf("永不过期应写 0，得到 %v", expiry)
	}
}

func TestXUIClientPayloadExpiry(t *testing.T) {
	payload := xuiClientPayload(xuiAddClientInput{Email: "a@b.com", ExpireDays: 30, LimitTotal: 1024})
	expiry, _ := payload["expiryTime"].(int64)
	if expiry <= 0 {
		t.Fatalf("指定天数后应写入未来时间戳，得到 %v", expiry)
	}
	if total, _ := payload["totalGB"].(int64); total != 1024 {
		t.Fatalf("配额未透传: %v", total)
	}
}

// ---------------------------------------------------------------------------
// 6. 路径与 email 白名单
// ---------------------------------------------------------------------------

func TestXUIEmailPattern(t *testing.T) {
	valid := []string{"user@example.com", "a_b-c.d+e", "test123"}
	for _, value := range valid {
		if !xuiEmailPattern.MatchString(value) {
			t.Fatalf("合法 email 被拒: %q", value)
		}
	}
	// 这些会被拼进 settings JSON 与 URL 路径，必须在边界上挡住。
	invalid := []string{"", "a b", "a/b", "a;b", `a"b`, "a\nb", "a'b", "<script>"}
	for _, value := range invalid {
		if xuiEmailPattern.MatchString(value) {
			t.Fatalf("危险输入被放行: %q", value)
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		found := false
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
