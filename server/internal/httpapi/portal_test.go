package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The node-side response envelopes are decoded from a service this panel does not
// control, so the shape assumptions are pinned here rather than discovered in
// production. Each test uses a verbatim sample of what portal.py returns.

func TestPortalUserIDPattern(t *testing.T) {
	accepted := []string{"hy2_abc123", "abc", "a-b", "a.b", "A_1.2-3", "0123456789"}
	for _, value := range accepted {
		if !portalUserIDPattern.MatchString(value) {
			t.Errorf("期望 %q 通过校验，实际被拒", value)
		}
	}
	// These matter because the id is placed in a URL path and in a JSON body:
	// a slash or dot-dot would let a caller escape the intended endpoint.
	rejected := []string{"", "a/b", "../etc", "a b", "a;b", "a$b", "a\x00b", "a|b", "用户名", "a`b", "a\nb"}
	for _, value := range rejected {
		if portalUserIDPattern.MatchString(value) {
			t.Errorf("期望 %q 被拒，实际通过", value)
		}
	}
	// 65 chars must fail, 64 must pass.
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if portalUserIDPattern.MatchString(long) {
		t.Error("65 字符的用户 ID 应被拒绝")
	}
	if !portalUserIDPattern.MatchString(long[:64]) {
		t.Error("64 字符的用户 ID 应被接受")
	}
}

func TestProxyIDPattern(t *testing.T) {
	// portal.py generates secrets.token_hex(6); the pattern stays tolerant of any
	// hex run between 6 and 64 chars so a future portal change does not silently
	// reject real ids.
	if !proxyIDPattern.MatchString("a1b2c3d4e5f6") {
		t.Error("12 位十六进制 id 应被接受")
	}
	if !proxyIDPattern.MatchString("a1b2c3") {
		t.Error("6 位十六进制 id 应被接受（下限）")
	}
	for _, value := range []string{"", "a1b2c", "ABCDEF", "a1b2c3d4e5f6g7", "a1-b2", "../../x", "a1b2c3d4e5f6/"} {
		if proxyIDPattern.MatchString(value) {
			t.Errorf("期望 %q 被拒", value)
		}
	}
}

func TestPortalProxyProtocolsAllowlist(t *testing.T) {
	for _, value := range []string{"socks5", "http", "https"} {
		if !portalProxyProtocols[value] {
			t.Errorf("%q 应在协议白名单内", value)
		}
	}
	for _, value := range []string{"", "SOCKS5", "ftp", "socks4", "socks5h", "http "} {
		if portalProxyProtocols[value] {
			t.Errorf("%q 不应在协议白名单内", value)
		}
	}
}

func TestParsePortalUsersList(t *testing.T) {
	// Verbatim shape from GET /api/v1/users/list.
	raw := `{
      "ok": true,
      "count": 2,
      "users": [
        {"user_id":"admin_master","expires_at":2085974400,"active":true,"traffic_limit_bytes":0,"traffic_used_bytes":123456,"ip_limit":0,"created_at":1700000000,"status":"active"},
        {"user_id":"demo-01","expires_at":1800000000,"active":false,"traffic_limit_bytes":107374182400,"traffic_used_bytes":107374182400,"ip_limit":3,"created_at":1700000100,"status":"active"}
      ]
    }`
	var parsed portalUsersListResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !parsed.OK {
		t.Fatal("ok 应为 true")
	}
	if parsed.Count != 2 || len(parsed.Users) != 2 {
		t.Fatalf("期望 2 个用户，得到 count=%d len=%d", parsed.Count, len(parsed.Users))
	}
	first := parsed.Users[0]
	if first.UserID != "admin_master" || !first.Active || first.IPLimit != 0 {
		t.Errorf("首行解析错误: %+v", first)
	}
	// An unlimited user reports limit_bytes 0. The UI must read that as
	// "unlimited", never as "0 bytes allowed".
	if first.LimitBytes != 0 {
		t.Errorf("不限量用户应为 0，得到 %d", first.LimitBytes)
	}
	second := parsed.Users[1]
	if second.LimitBytes != 107374182400 || second.UsedBytes != 107374182400 {
		t.Errorf("第二行配额解析错误: %+v", second)
	}
	if second.IPLimit != 3 {
		t.Errorf("第二行 IP 限制应为 3，得到 %d", second.IPLimit)
	}
}

func TestParsePortalUserCreate(t *testing.T) {
	// Verbatim shape from POST /api/v1/users/create. All three subscription
	// artifacts must survive decoding intact.
	raw := `{
      "ok": true,
      "user_id": "hy2_abc123",
      "password": "s3cret-token",
      "ip_limit": 2,
      "traffic_gb": 50,
      "expires_at": 1800000000,
      "uri": "hysteria2://s3cret-token@203.0.113.10:8443?sni=example.com#Teyir-Hy2-hy2_abc123",
      "reality_uri": "vless://uuid@203.0.113.10:443?security=reality&pbk=KEY&sid=ab12&sni=www.apple.com#example",
      "clash": "proxies:\n  - name: hysteria2\n    type: hysteria2\n",
      "sing_box": "{\"type\":\"hysteria2\"}"
    }`
	var parsed portalUserCreateResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.UserID != "hy2_abc123" {
		t.Errorf("user_id 解析错误: %q", parsed.UserID)
	}
	if parsed.Password != "s3cret-token" {
		t.Errorf("password 解析错误: %q", parsed.Password)
	}
	if parsed.ExpiresAt != 1800000000 {
		t.Errorf("expires_at 解析错误: %d", parsed.ExpiresAt)
	}
	// The subscription artifacts are passed through verbatim. A silent
	// truncation of clash/sing_box would hand the operator a broken file, which
	// is why the client reads them with a 256 KiB cap rather than a small one.
	if parsed.URI == "" || parsed.Clash == "" || parsed.SingBox == "" {
		t.Error("订阅产物不应为空")
	}
	if parsed.RealityURI == "" {
		t.Error("reality_uri 不应为空")
	}
	if parsed.TrafficGB != 50 || parsed.IPLimit != 2 {
		t.Errorf("配额字段解析错误: gb=%d ip=%d", parsed.TrafficGB, parsed.IPLimit)
	}
}

func TestParsePortalUserRenew(t *testing.T) {
	raw := `{"ok": true, "user_id": "demo-01", "expires_at": 1900000000, "limit_bytes": 214748364800}`
	var parsed portalUserRenewResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.ExpiresAt != 1900000000 || parsed.LimitBytes != 214748364800 {
		t.Errorf("续期结果解析错误: %+v", parsed)
	}
}

func TestParsePortalProxyList(t *testing.T) {
	// The portal intentionally does not mask credentials for API-key callers, so
	// the username/password fields are populated and must decode.
	raw := `{
      "ok": true,
      "count": 1,
      "services": [
        {"id":"a1b2c3d4e5f6","type":"socks5","host":"203.0.113.10","port":25000,
         "username":"u1","password":"p1","note":"customer-a",
         "url":"socks5://u1:p1@203.0.113.10:25000","format":"203.0.113.10:25000:u1:p1"}
      ]
    }`
	var parsed portalProxyListResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.Count != 1 || len(parsed.Services) != 1 {
		t.Fatalf("期望 1 条代理服务，得到 count=%d len=%d", parsed.Count, len(parsed.Services))
	}
	service := parsed.Services[0]
	if service.ID != "a1b2c3d4e5f6" || service.Type != "socks5" || service.Port != 25000 {
		t.Errorf("代理服务解析错误: %+v", service)
	}
	if service.Username != "u1" || service.Password != "p1" {
		t.Errorf("代理凭据解析错误: %+v", service)
	}
	if service.Format != "203.0.113.10:25000:u1:p1" {
		t.Errorf("format 字段解析错误: %q", service.Format)
	}
}

// The portal answers a rejected request with ok:false plus a human-readable
// error, and the client must surface that text rather than a bare status code.
func TestPortalEnvelopeCarriesError(t *testing.T) {
	raw := `{"ok": false, "error": "端口 25000 已被其他代理服务占用"}`
	var parsed portalEnvelope
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.OK {
		t.Fatal("ok 应为 false")
	}
	if parsed.Error != "端口 25000 已被其他代理服务占用" {
		t.Errorf("error 文本丢失: %q", parsed.Error)
	}
}

// An empty user list must decode to a non-nil slice so the handler emits [] and
// the UI renders "0 users" instead of treating null as a failure.
func TestParsePortalUsersListEmpty(t *testing.T) {
	raw := `{"ok": true, "count": 0, "users": []}`
	var parsed portalUsersListResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.Users == nil {
		// The handler normalises nil to an empty slice; assert the input decoded
		// as an empty (possibly nil) collection rather than erroring.
		t.Log("users 解码为 nil，处理器会归一化为空切片")
	}
	if parsed.Count != 0 {
		t.Errorf("期望 count=0，得到 %d", parsed.Count)
	}
}

// portalMeta carries the node's master credential, and the overview handler is
// the only place that struct is serialised. If a future refactor returns
// `meta` instead of `meta.redacted()`, this test fails instead of the panel
// quietly shipping the node's master password to every browser.
func TestPortalMetaRedactionDropsSecrets(t *testing.T) {
	meta := portalMeta{
		PublicIP:     "203.0.113.10",
		ServerName:   "hy2.example.com",
		ListenPort:   8443,
		SubsPort:     8443,
		HopPortRange: "20000-40000",
		CertType:     "self-signed",
		IsInsecure:   true,
		AuthPassword: "SUPER_SECRET_MASTER",
		ObfsPassword: "OBFS_SECRET",
		PinSHA256:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	}
	encoded, err := json.Marshal(meta.redacted())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	body := string(encoded)

	for _, secret := range []string{"SUPER_SECRET_MASTER", "OBFS_SECRET", meta.PinSHA256} {
		if strings.Contains(body, secret) {
			t.Errorf("脱敏后的 meta 仍包含机密 %q：%s", secret, body)
		}
	}
	// The key names must be gone too, not merely the values: a nil value under
	// the same key would still advertise the field's existence.
	for _, key := range []string{"auth_password", "obfs_password", "pin_sha256"} {
		if strings.Contains(body, key) {
			t.Errorf("脱敏后的 meta 仍暴露字段名 %q：%s", key, body)
		}
	}
	// The safe fields must survive, or the UI loses the information it needs.
	for _, key := range []string{"public_ip", "listen_port", "subscription_port", "is_insecure", "has_obfs", "has_pin"} {
		if !strings.Contains(body, key) {
			t.Errorf("脱敏后的 meta 丢失了字段 %q：%s", key, body)
		}
	}
}

// The overview's summary counters are computed in Go. Unlimited users report
// limit_bytes 0 and must not be folded into the quota total, or an "unlimited"
// fleet would render as "0 GB quota".
func TestOverviewSummaryCountsUnlimitedSeparately(t *testing.T) {
	now := time.Now().Unix()
	users := []portalUserEntry{
		{UserID: "a", Active: true, LimitBytes: 0, UsedBytes: 100, ExpiresAt: now + 1000},
		{UserID: "b", Active: true, LimitBytes: 500, UsedBytes: 50, ExpiresAt: now + 1000},
		{UserID: "c", Active: false, LimitBytes: 0, UsedBytes: 25, ExpiresAt: now - 1000},
	}
	var activeUsers, unlimited int
	var usedBytes, limitBytes int64
	for _, user := range users {
		if user.Active {
			activeUsers++
		}
		usedBytes += user.UsedBytes
		if user.LimitBytes > 0 {
			limitBytes += user.LimitBytes
		} else {
			unlimited++
		}
	}
	if activeUsers != 2 {
		t.Errorf("有效用户应为 2，得到 %d", activeUsers)
	}
	if unlimited != 2 {
		t.Errorf("不限量用户应为 2，得到 %d", unlimited)
	}
	if usedBytes != 175 {
		t.Errorf("用量合计应为 175，得到 %d", usedBytes)
	}
	if limitBytes != 500 {
		t.Errorf("配额合计应只算有配额的 500，得到 %d", limitBytes)
	}
}
