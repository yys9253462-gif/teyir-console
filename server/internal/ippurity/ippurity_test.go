package ippurity

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// fakeProvider 用于覆盖 API 分支，避免测试真的出网。
type fakeProvider struct {
	verdict *Verdict
	err     error
	calls   int
}

func (f *fakeProvider) Lookup(_ context.Context, _ *http.Client, _ string) (*Verdict, error) {
	f.calls++
	return f.verdict, f.err
}
func (f *fakeProvider) Name() string { return "fake" }

func TestIsPublicIP(t *testing.T) {
	reject := []string{
		"", " ", "127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", "::1", "fe80::1", "0.0.0.0",
		"1.2.3.4:443", "[::1]:443", "1.2.3.0/24", "2001:db8::1",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "100.64.0.1",
		"not-an-ip", "example.com",
	}
	for _, value := range reject {
		if IsPublicIP(value) {
			t.Errorf("IsPublicIP(%q) = true，应当拒绝", value)
		}
	}
	accept := []string{"8.8.8.8", "1.1.1.1", "9.9.9.9", "2606:4700:4700::1111"}
	for _, value := range accept {
		if !IsPublicIP(value) {
			t.Errorf("IsPublicIP(%q) = false，应当接受", value)
		}
	}
}

func TestLookupRejectsNonPublic(t *testing.T) {
	r := NewResolver(Options{API: &fakeProvider{verdict: &Verdict{ConnectionType: "datacenter"}}})
	if _, err := r.Lookup(context.Background(), "192.168.1.1", false); err == nil {
		t.Fatal("内网 IP 应当报错")
	}
}

// 离线能判连接类型，但评分/双ISP缺失 → 仍应继续走 API 补全。
func TestLookupFallsBackToAPIForScore(t *testing.T) {
	db := &stubDB{verdict: &Verdict{ConnectionType: "residential", ASN: "AS123"}}
	api := &fakeProvider{verdict: &Verdict{
		ConnectionType: "residential",
		FraudScore:     IntPtr(12),
		DualISP:        BoolPtr(true),
	}}
	r := NewResolver(Options{DB: db, API: api})
	v, err := r.Lookup(context.Background(), "8.8.8.8", false)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("应当调用一次 API，实际 %d 次", api.calls)
	}
	if v.FraudScore == nil || *v.FraudScore != 12 {
		t.Errorf("评分未从 API 补全: %+v", v.FraudScore)
	}
	if v.DualISP == nil || *v.DualISP != true {
		t.Errorf("双ISP未从 API 补全: %+v", v.DualISP)
	}
	if v.Source != "api" {
		t.Errorf("来源应为 api，实际 %q", v.Source)
	}
}

// API 挂了但有离线结果 → 返回离线结果，不报错。
func TestLookupAPIErrorKeepsOffline(t *testing.T) {
	db := &stubDB{verdict: &Verdict{ConnectionType: "datacenter", ASN: "AS16509"}}
	api := &fakeProvider{err: context.DeadlineExceeded}
	r := NewResolver(Options{DB: db, API: api})
	v, err := r.Lookup(context.Background(), "3.5.0.1", false)
	if err != nil {
		t.Fatalf("有离线结果时 API 失败不应报错: %v", err)
	}
	if v.ConnectionType != "datacenter" {
		t.Errorf("应保留离线连接类型，实际 %q", v.ConnectionType)
	}
	if v.Source != "offline" {
		t.Errorf("来源应为 offline，实际 %q", v.Source)
	}
}

// 离线与 API 都没有 → 报错。
func TestLookupNoSource(t *testing.T) {
	r := NewResolver(Options{})
	if _, err := r.Lookup(context.Background(), "8.8.8.8", false); err == nil {
		t.Fatal("无离线库无 API 时应报错")
	}
}

// forceAPI=true 时即使离线已判出连接类型也要走 API（界面「深度检测」）。
func TestLookupForceAPI(t *testing.T) {
	db := &stubDB{verdict: &Verdict{ConnectionType: "residential"}}
	api := &fakeProvider{verdict: &Verdict{ConnectionType: "residential", FraudScore: IntPtr(5)}}
	r := NewResolver(Options{DB: db, API: api})
	if _, err := r.Lookup(context.Background(), "8.8.8.8", true); err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("forceAPI 时应调用 API，实际 %d 次", api.calls)
	}
}

// mergeVerdict：API 的空字段不能清掉离线已有值。
func TestMergeVerdictKeepsOfflineWhenAPINil(t *testing.T) {
	base := Verdict{ConnectionType: "residential", ASN: "AS1", ASNOrg: "ISP A"}
	api := Verdict{ConnectionType: "unknown"} // API 没判出类型
	got := mergeVerdict(base, api)
	if got.ConnectionType != "residential" {
		t.Errorf("API 的 unknown 不应覆盖离线 residential，实际 %q", got.ConnectionType)
	}
	if got.ASN != "AS1" {
		t.Errorf("ASN 不应丢失，实际 %q", got.ASN)
	}
}

// ipapi.is 归一化：机房判定、评分换算、双ISP 判定。
// ⚠️ 用真实的 JSON 反序列化路径（而不是手搓结构体字面量），因为线上出过的
// bug 恰恰是"字段类型和上游对不上"—— 手搓字面量永远测不出来。
func TestNormalizeIPAPIIs(t *testing.T) {
	rawJSON := `{
		"ip": "1.2.3.4",
		"is_datacenter": true,
		"is_abuser": true,
		"asn": {"asn": 16509, "org": "Amazon.com, Inc.", "type": "hosting", "abuser_score": "0.2"}
	}`
	var raw ipapiIsResponse
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	v := normalizeIPAPIIs("1.2.3.4", raw)
	if v.ConnectionType != "datacenter" {
		t.Errorf("应判为机房，实际 %q", v.ConnectionType)
	}
	if v.ASN != "AS16509" {
		t.Errorf("ASN 应为 AS16509，实际 %q", v.ASN)
	}
	// 被标滥用 → 至少 70，避免"滥用但 0 分"的自相矛盾。
	if v.FraudScore == nil || *v.FraudScore < 70 {
		t.Errorf("滥用标记下评分应 >=70，实际 %v", v.FraudScore)
	}
	if v.DualISP != nil {
		t.Errorf("缺 company 口径时双ISP应留 nil（未判定），实际 %v", *v.DualISP)
	}
}

// ⭐ 回归：ipapi.is 的 abuser_score 实际是带单位的字符串（如 "0.0023 (Low)"）。
// 曾经把它当 float64 解析，导致**每一个真实 IP 都解析失败**。
func TestNormalizeAbuserScoreString(t *testing.T) {
	cases := []struct {
		raw    string
		wantOK bool
		wantF  float64
	}{
		{`"0.0023 (Low)"`, true, 0.0023},
		{`"0.0051 (Low)"`, true, 0.0051},
		{`"12 (High)"`, true, 12},
		{`"1.5"`, true, 1.5},
		{`0.42`, true, 0.42},
		{`"High"`, false, 0},
		{`""`, false, 0},
		{`null`, false, 0},
	}
	for _, c := range cases {
		f, ok := parseAbuserScore(json.RawMessage(c.raw))
		if ok != c.wantOK {
			t.Errorf("parseAbuserScore(%s) ok=%v，期望 %v", c.raw, ok, c.wantOK)
			continue
		}
		if ok && (f < c.wantF-1e-9 || f > c.wantF+1e-9) {
			t.Errorf("parseAbuserScore(%s)=%v，期望 %v", c.raw, f, c.wantF)
		}
	}
}

// 端到端：真实形态的 JSON（字符串 abuser_score）必须能被完整解析出 Verdict。
func TestNormalizeFullRealisticJSON(t *testing.T) {
	rawJSON := `{
		"ip": "198.51.100.32",
		"is_datacenter": true,
		"is_proxy": false,
		"is_vpn": false,
		"is_abuser": false,
		"company": {"name": "Amazon.com, Inc.", "type": "hosting"},
		"asn": {"asn": 16509, "org": "Amazon.com, Inc.", "type": "hosting", "abuser_score": "0.0023 (Low)"},
		"datacenter": {"datacenter": "Amazon AWS"}
	}`
	var raw ipapiIsResponse
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("反序列化真实形态失败: %v", err)
	}
	v := normalizeIPAPIIs("198.51.100.32", raw)
	if v.ConnectionType != "datacenter" {
		t.Errorf("应判机房，实际 %q", v.ConnectionType)
	}
	if v.FraudScore == nil {
		t.Fatalf("评分不应为 nil")
	}
	if *v.FraudScore != 0 { // 0.0023*100=0.23 → 四舍五入 0
		t.Logf("评分 = %d（0.0023*100 取整）", *v.FraudScore)
	}
	if v.ASNOrg != "Amazon.com, Inc." {
		t.Errorf("ASNOrg 应为 Amazon.com, Inc.，实际 %q", v.ASNOrg)
	}
}

// 双 ISP：ASN 与 company 两个口径都是 isp → true（组织名同不同无关）。
func TestNormalizeDualISPPositive(t *testing.T) {
	rawJSON := `{
		"ip": "9.9.9.9",
		"asn": {"asn": 100, "org": "ISP Alpha", "type": "isp", "abuser_score": "0.1 (Low)"},
		"company": {"name": "ISP Beta", "type": "isp"}
	}`
	var raw ipapiIsResponse
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	v := normalizeIPAPIIs("9.9.9.9", raw)
	if v.DualISP == nil || *v.DualISP != true {
		t.Errorf("应判为双ISP，实际 %v", v.DualISP)
	}
	if v.ConnectionType != "residential" {
		t.Errorf("isp 类型应判家宽，实际 %q", v.ConnectionType)
	}
}

// 缺少字段时，双 ISP 必须是 nil（未判定），不能是 false。
func TestNormalizeDualISPUnknownStaysNil(t *testing.T) {
	raw := ipapiIsResponse{} // 全空
	v := normalizeIPAPIIs("9.9.9.9", raw)
	if v.DualISP != nil {
		t.Errorf("无数据时双ISP应为 nil，实际 %v", *v.DualISP)
	}
	if v.FraudScore != nil {
		t.Errorf("无数据时评分应为 nil，实际 %v", *v.FraudScore)
	}
}

// ipapi.is 适配器：HTTP 层用 httptest 覆盖，包括 429 与正常返回。
func TestIPAPIIsHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") == "" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ip": "1.2.3.4", "is_datacenter": true,
		})
	}))
	defer server.Close()

	p := IPAPIIs{APIKey: "test", BaseURL: server.URL}
	v, err := p.Lookup(context.Background(), server.Client(), "1.2.3.4")
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if v.ConnectionType != "datacenter" {
		t.Errorf("应判为机房，实际 %q", v.ConnectionType)
	}
}

func TestIPAPIIsNoKey(t *testing.T) {
	p := IPAPIIs{}
	if _, err := p.Lookup(context.Background(), http.DefaultClient, "1.2.3.4"); err == nil {
		t.Fatal("无密钥应报错")
	}
}

// stubDB 是 OfflineDB 的测试替身。
type stubDB struct {
	verdict *Verdict
}

func (s *stubDB) Lookup(string) *Verdict { return s.verdict }
func (s *stubDB) Source() string         { return "stub" }

// 离线库最长前缀匹配：更具体的条目应当胜出。
func TestOfflineLongestPrefix(t *testing.T) {
	db := &OfflineFile{
		entries: []offlineEntry{
			mustEntry(t, "3.0.0.0/8", "datacenter"),
			mustEntry(t, "3.5.0.0/16", "residential"),
		},
	}
	// 手工按前缀长度降序排序，模拟加载后的状态。
	sort.Slice(db.entries, func(i, j int) bool {
		return db.entries[i].prefixLen > db.entries[j].prefixLen
	})
	if v := db.Lookup("3.5.1.1"); v == nil || v.ConnectionType != "residential" {
		t.Fatalf("最长前缀应为 residential，实际 %+v", v)
	}
	if v := db.Lookup("3.9.1.1"); v == nil || v.ConnectionType != "datacenter" {
		t.Fatalf("应命中 /8 条目，实际 %+v", v)
	}
	if v := db.Lookup("4.4.4.4"); v != nil {
		t.Fatalf("未收录 IP 应返回 nil，实际 %+v", v)
	}
}

func mustEntry(t *testing.T, cidr, connType string) offlineEntry {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", cidr, err)
	}
	ones, _ := network.Mask.Size()
	return offlineEntry{network: network, prefixLen: ones, verdict: Verdict{ConnectionType: connType}}
}

// ⭐ 回归（2026-10-15）：真实数据 —— NTT 两个口径都是 isp、组织同名，
// 曾被误判为「否」；Amazon 两个口径都是 hosting，曾显示「未判定」。
func TestNormalizeDualISPRealResponses(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"ntt-both-isp-same-org", `{"is_datacenter":true,"asn":{"asn":2914,"org":"NTT America, Inc.","type":"isp"},"company":{"name":"NTT America, Inc.","type":"isp"}}`, true},
		{"amazon-hosting", `{"is_datacenter":true,"asn":{"asn":16509,"org":"Amazon.com, Inc.","type":"hosting"},"company":{"name":"Amazon Technologies Inc.","type":"hosting"}}`, false},
		{"isp-asn-hosting-company", `{"asn":{"asn":1,"org":"X","type":"isp"},"company":{"name":"Y Cloud","type":"hosting"}}`, false},
	}
	for _, c := range cases {
		var raw ipapiIsResponse
		if err := json.Unmarshal([]byte(c.json), &raw); err != nil {
			t.Fatal(err)
		}
		v := normalizeIPAPIIs("1.1.1.1", raw)
		if v.DualISP == nil || *v.DualISP != c.want {
			t.Errorf("%s: 双ISP 应为 %v，实际 %v", c.name, c.want, v.DualISP)
		}
	}
}

// 2026-11-03：网络错误时错误信息不能带出 API 密钥（密钥在 query 里，*url.Error 会打出完整 URL）。
func TestIPAPIIsNetworkErrorDoesNotLeakKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := server.URL
	server.Close() // 连接必然被拒
	p := IPAPIIs{APIKey: "SECRET-KEY-123456", BaseURL: base}
	_, err := p.Lookup(context.Background(), &http.Client{}, "1.2.3.4")
	if err == nil {
		t.Fatal("连接被拒应返回错误")
	}
	if strings.Contains(err.Error(), "SECRET-KEY-123456") {
		t.Fatalf("错误信息泄露了 API 密钥: %v", err)
	}
}
