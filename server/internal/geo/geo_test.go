package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 命名与归一 —— 这是「识别质量」的核心，必须逐条钉死
// ---------------------------------------------------------------------------

// 2026-10-09 规则变更：Name() = 国家中文-城市（旧为 城市-商家）。
// Country 存 ISO 码，Name() 内部会过 zhCountry 翻成中文。
func TestNameJoin(t *testing.T) {
	cases := []struct {
		name string
		info Info
		want string
	}{
		{"国家+城市", Info{Country: "JP", Region: "东京"}, "日本-东京"},
		{"只有国家", Info{Country: "US"}, "美国"},
		{"只有城市", Info{Region: "东京"}, "东京"},
		{"都没有", Info{}, ""},
		// 商家不再进名字：即使 Provider 有值也不出现。
		{"含商家被忽略", Info{Country: "JP", Region: "东京", Provider: "AWS"}, "日本-东京"},
		// 带空格要被收拾掉，否则会出现 "日本 - 东京" 这种名字。
		{"带空格", Info{Country: " JP ", Region: " 东京 "}, "日本-东京"},
		// 未收录的国家码原样返回，比空强。
		{"未知国家码", Info{Country: "ZZ", Region: "某城"}, "ZZ-某城"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Name(); got != tc.want {
				t.Fatalf("Name() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestIsPublicIPRejectsNonPublic(t *testing.T) {
	bad := []string{
		"", "127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // 云元数据端点，重点
		"::1", "fe80::1", "0.0.0.0",
		"1.2.3.4:443",   // 带端口
		"[2001:db8::1]", // 带方括号
		"example.com",   // 域名
		"1.2.3.4/24",    // 带掩码
		"999.1.1.1",     // 非法
		"2001:db8::1",   // RFC 3849 文档保留段
		"192.0.2.1",     // RFC 5737 TEST-NET-1
		"198.51.100.1",  // RFC 5737 TEST-NET-2
		"203.0.113.1",   // RFC 5737 TEST-NET-3
		"100.64.0.1",    // RFC 6598 运营商级 NAT
		"198.18.0.1",    // RFC 2544 基准测试段
		"not an ip at all",
	}
	for _, value := range bad {
		if IsPublicIP(value) {
			t.Errorf("IsPublicIP(%q) = true，应当拒绝", value)
		}
	}

	good := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "2606:4700:4700::1111"}
	for _, value := range good {
		if !IsPublicIP(value) {
			t.Errorf("IsPublicIP(%q) = false，应当接受", value)
		}
	}
}

func TestExtractASN(t *testing.T) {
	cases := map[string]string{
		"AS16509 Amazon.com, Inc.": "AS16509",
		"as24940 Hetzner":          "AS24940",
		"":                         "",
		"Amazon.com, Inc.":         "",
		"AS":                       "",
		"ASABC":                    "",
		"AS123":                    "AS123",
	}
	for input, want := range cases {
		if got := extractASN(input); got != want {
			t.Errorf("extractASN(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// 同一家 AWS 会以多种写法出现在 ASN 数据里；识别结果必须收敛到同一个短名，
// 否则「同一批机器得到三种前缀」等于白识别。
func TestKnownProviderNormalization(t *testing.T) {
	cases := []struct {
		org  string
		as   string
		isp  string
		want string
	}{
		{org: "Amazon.com, Inc.", as: "AS16509 Amazon.com, Inc.", want: "AWS"},
		{org: "Amazon Technologies Inc.", as: "AS14618 Amazon Technologies Inc.", want: "AWS"},
		{org: "Amazon Data Services Japan", as: "AS16509", want: "AWS"},
		{org: "Hetzner Online GmbH", as: "AS24940 Hetzner Online GmbH", want: "Hetzner"},
		{org: "OVH SAS", as: "AS16276 OVH SAS", want: "OVH"},
		{org: "Vultr Holdings, LLC", as: "AS20473 The Constant Company", want: "Vultr"},
		{org: "DigitalOcean, LLC", as: "AS14061 DigitalOcean, LLC", want: "DigitalOcean"},
		{org: "Google LLC", as: "AS396982 Google LLC", want: "GCP"},
		{org: "Microsoft Corporation", as: "AS8075 Microsoft Corporation", want: "Azure"},
		// 没有 ASN 时靠组织名子串兜底。
		{org: "Contabo GmbH", as: "", want: "Contabo"},
		{org: "", as: "", isp: "Cloudflare, Inc.", want: "Cloudflare"},
		// 未知商家不该被硬套一个已知短名。
		{org: "Some Tiny Hosting LLC", as: "AS64500 Some Tiny Hosting LLC", want: ""},
	}
	for _, tc := range cases {
		got, ok := matchKnownProvider(tc.org, tc.as, tc.isp)
		if tc.want == "" {
			if ok {
				t.Errorf("matchKnownProvider(%q,%q,%q) = %q，期望不命中", tc.org, tc.as, tc.isp, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("matchKnownProvider(%q,%q,%q) = (%q,%v)，期望 %q", tc.org, tc.as, tc.isp, got, ok, tc.want)
		}
	}
}

// ASN 命中优先于组织名子串：两者冲突时要信 ASN。
func TestASNTakesPriorityOverOrgSubstring(t *testing.T) {
	// 组织名里带 "amazon"（会被子串规则判成 AWS），但 ASN 是 Hetzner 的。
	got, ok := matchKnownProvider("Amazon Reseller Ltd", "AS24940 Hetzner", "")
	if !ok || got != "Hetzner" {
		t.Fatalf("ASN 应优先于组织名子串，得到 (%q,%v)", got, ok)
	}
}

func TestCleanProviderName(t *testing.T) {
	cases := map[string]string{
		"Hetzner Online GmbH": "Hetzner Online",
		"Example Inc.":        "Example",
		"Foo Ltd":             "Foo",
		"Bar LLC":             "Bar",
		"AMAZON-02":           "AMAZON",
		"Baz (us-central1)":   "Baz",
		"":                    "",
	}
	for input, want := range cases {
		if got := cleanProviderName(input); got != want {
			t.Errorf("cleanProviderName(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// 全是被过滤词时不能返回空串 —— 宁可返回原文，也别把商家名弄丢。
func TestCleanProviderNameNeverReturnsEmptyForNonEmptyInput(t *testing.T) {
	if got := cleanProviderName("GmbH"); got == "" {
		t.Fatal("全为过滤词时应退回原文，不能返回空串")
	}
}

// ---------------------------------------------------------------------------
// 离线库
// ---------------------------------------------------------------------------

func writeOfflineDB(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "geo.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时库失败: %v", err)
	}
	return path
}

func TestLoadOfflineFileAndLongestPrefixWins(t *testing.T) {
	path := writeOfflineDB(t, `{
	  "_meta": {"source": "unit-test", "count": 2},
	  "entries": [
	    {"network": "3.0.0.0/8",  "region": "粗粒度地区", "provider": "粗粒度商家"},
	    {"network": "3.5.0.0/16", "region": "Tokyo", "provider": "AWS", "country": "JP", "asn": "AS16509"}
	  ]
	}`)
	db, err := LoadOfflineFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if db.Len() != 2 {
		t.Fatalf("条目数 = %d，期望 2", db.Len())
	}

	// 3.5.x 落在 /16 里，必须选更具体的那个，而不是先出现的 /8。
	info := db.Lookup("3.5.1.1")
	if info == nil {
		t.Fatal("3.5.1.1 应当命中")
	}
	if info.Region != "Tokyo" || info.Provider != "AWS" {
		t.Fatalf("最长前缀匹配失效，得到 %+v", info)
	}

	// 3.9.x 只在 /8 里。
	info = db.Lookup("3.9.1.1")
	if info == nil || info.Region != "粗粒度地区" {
		t.Fatalf("3.9.1.1 应当命中 /8，得到 %+v", info)
	}

	// 库外地址返回 nil（不是空 Info）。
	if info := db.Lookup("8.8.8.8"); info != nil {
		t.Fatalf("库外地址应返回 nil，得到 %+v", info)
	}
}

// 坏条目跳过、好条目保留 —— 手工维护的数据里混进错 CIDR 是常态。
func TestLoadOfflineFileSkipsBadEntries(t *testing.T) {
	path := writeOfflineDB(t, `{
	  "entries": [
	    {"network": "not-a-cidr", "region": "坏条目"},
	    {"network": "1.0.0.0/8", "region": "好条目", "provider": "X"},
	    {"network": "2.0.0.0/8"}
	  ]
	}`)
	db, err := LoadOfflineFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if db.Len() != 1 {
		t.Fatalf("应当只保留 1 条可用条目，实际 %d", db.Len())
	}
	if db.Lookup("1.2.3.4") == nil {
		t.Fatal("好条目应当可查")
	}
}

func TestLoadOfflineFileErrorsAreNonFatalShaped(t *testing.T) {
	// 文件不存在
	if _, err := LoadOfflineFile(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("文件不存在应当返回 error")
	}
	// 空路径
	if _, err := LoadOfflineFile("  "); err == nil {
		t.Fatal("空路径应当返回 error")
	}
	// 语法坏掉的 JSON
	path := writeOfflineDB(t, `{ this is not json`)
	if _, err := LoadOfflineFile(path); err == nil {
		t.Fatal("坏 JSON 应当返回 error")
	}
	// 完全没有可用条目
	path = writeOfflineDB(t, `{"entries": []}`)
	if _, err := LoadOfflineFile(path); err == nil {
		t.Fatal("空库应当返回 error")
	}
}

func TestOfflineOnlyRegionFallback(t *testing.T) {
	// 只给了全称、没给短名时，短名要退回全称，避免识别出信息却填不进去。
	path := writeOfflineDB(t, `{
	  "entries": [{"network": "5.0.0.0/8", "region_long": "Frankfurt, Germany"}]
	}`)
	db, err := LoadOfflineFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	info := db.Lookup("5.5.5.5")
	if info == nil || info.Region != "Frankfurt, Germany" {
		t.Fatalf("短名应退回全称，得到 %+v", info)
	}
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

func TestCacheExpiry(t *testing.T) {
	cache := NewCache(10*time.Millisecond, 10)
	now := time.Now()
	cache.Put("1.1.1.1", Info{Region: "Tokyo"}, now)

	if _, ok := cache.Get("1.1.1.1"); !ok {
		t.Fatal("刚写入应当命中")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := cache.Get("1.1.1.1"); ok {
		t.Fatal("过期后不应命中")
	}
}

// 空结果不进缓存：否则一次限速抖动会让这台机器在 TTL 内永远识别不出。
func TestCacheDoesNotStoreEmpty(t *testing.T) {
	cache := NewCache(time.Hour, 10)
	cache.Put("1.1.1.1", Info{}, time.Now())
	if _, ok := cache.Get("1.1.1.1"); ok {
		t.Fatal("空结果不应被缓存")
	}
}

func TestCacheEvictionKeepsWorking(t *testing.T) {
	cache := NewCache(time.Hour, 4)
	for i := 0; i < 50; i++ {
		ip := "10." + itoa(i) + ".0.1"
		cache.Put(ip, Info{Region: "R", Provider: "P"}, time.Now())
	}
	if cache.Len() > 4 {
		t.Fatalf("缓存条目数 %d 超过上限 4", cache.Len())
	}
	// 清空后仍应可写可读。
	cache.Put("1.1.1.1", Info{Region: "Tokyo"}, time.Now())
	if _, ok := cache.Get("1.1.1.1"); !ok {
		t.Fatal("淘汰后写入应当可读")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// Resolver：优先级与降级
// ---------------------------------------------------------------------------

// stubAPI 是可控的兜底 provider，用来覆盖 API 的失败分支。
type stubAPI struct {
	info   *Info
	err    error
	calls  int
	lastIP string
}

func (s *stubAPI) Lookup(ctx context.Context, client *http.Client, ip string) (*Info, error) {
	s.calls++
	s.lastIP = ip
	if s.err != nil {
		return nil, s.err
	}
	return s.info, nil
}

func (s *stubAPI) Name() string { return "stub" }

func TestResolverPrefersOfflineOverAPI(t *testing.T) {
	path := writeOfflineDB(t, `{
	  "entries": [{"network": "1.0.0.0/8", "region": "Tokyo", "provider": "AWS"}]
	}`)
	db, err := LoadOfflineFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	api := &stubAPI{info: &Info{Region: "API地区", Provider: "API商家"}}
	resolver := NewResolver(Options{DB: db, API: api})

	info, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("识别失败: %v", err)
	}
	if info.Source != "offline" {
		t.Fatalf("应当走离线库，实际 source=%q", info.Source)
	}
	if api.calls != 0 {
		t.Fatalf("离线库命中时不应调用 API，实际调用 %d 次", api.calls)
	}
}

func TestResolverFallsBackToAPI(t *testing.T) {
	path := writeOfflineDB(t, `{
	  "entries": [{"network": "1.0.0.0/8", "region": "Tokyo", "provider": "AWS"}]
	}`)
	db, _ := LoadOfflineFile(path)
	api := &stubAPI{info: &Info{Region: "Frankfurt", Provider: "Hetzner"}}
	resolver := NewResolver(Options{DB: db, API: api})

	info, err := resolver.Lookup(context.Background(), "9.9.9.9") // 库外
	if err != nil {
		t.Fatalf("识别失败: %v", err)
	}
	// Lookup 出口做翻译：法兰克福 → 法兰克福（自身）、AWS 保持英文。
	if info.Source != "api" || info.Region != "法兰克福" {
		t.Fatalf("应当走 API 兜底，得到 %+v", info)
	}
	if api.calls != 1 {
		t.Fatalf("API 调用次数 = %d，期望 1", api.calls)
	}
}

// 没有离线库也必须能工作（降级到 API）—— 这是「库不进 git」的前提。
func TestResolverWorksWithoutOfflineDB(t *testing.T) {
	api := &stubAPI{info: &Info{Region: "Tokyo", Provider: "AWS"}}
	resolver := NewResolver(Options{API: api})

	info, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("没有离线库时应当能走 API，得到错误 %v", err)
	}
	if info.Region != "东京" {
		t.Fatalf("得到 %+v", info)
	}
}

// 两条路都不通时返回错误，但**不能 panic**，也不能返回垃圾数据。
func TestResolverDegradesGracefully(t *testing.T) {
	api := &stubAPI{err: errors.New("网络不通")}
	resolver := NewResolver(Options{API: api})

	info, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err == nil {
		t.Fatal("两条路都不通时应当返回 error")
	}
	if !info.Empty() {
		t.Fatalf("失败时应返回空 Info，得到 %+v", info)
	}
	if !strings.Contains(resolver.LastAPIError(), "网络不通") {
		t.Fatalf("应当记录 API 失败原因，实际 %q", resolver.LastAPIError())
	}
}

// Resolver 完全不配置（没库没 API）也不能崩。
func TestResolverWithNothingConfigured(t *testing.T) {
	resolver := NewResolver(Options{})
	if _, err := resolver.Lookup(context.Background(), "1.2.3.4"); err == nil {
		t.Fatal("什么都没配时应当返回 error")
	}
	if resolver.HasOfflineDB() || resolver.HasAPI() {
		t.Fatal("能力探测应当都是 false")
	}
}

// 非公网输入直接拒绝，且**绝不发起任何 API 调用**。
func TestResolverRejectsNonPublicWithoutCallingAPI(t *testing.T) {
	api := &stubAPI{info: &Info{Region: "Tokyo"}}
	resolver := NewResolver(Options{API: api})

	for _, value := range []string{"127.0.0.1", "10.1.1.1", "169.254.169.254", "not-an-ip", ""} {
		if _, err := resolver.Lookup(context.Background(), value); err == nil {
			t.Errorf("Lookup(%q) 应当被拒绝", value)
		}
	}
	if api.calls != 0 {
		t.Fatalf("非公网输入不应触发 API 调用，实际 %d 次", api.calls)
	}
}

// 第二次查询同一 IP 必须命中缓存、不再出网。
func TestResolverCachesResult(t *testing.T) {
	api := &stubAPI{info: &Info{Region: "Tokyo", Provider: "AWS"}}
	cache := NewCache(time.Hour, 10)
	resolver := NewResolver(Options{API: api, Cache: cache})

	first, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("首次识别失败: %v", err)
	}
	if first.Source != "api" {
		t.Fatalf("首次应走 API，得到 %q", first.Source)
	}

	second, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("二次识别失败: %v", err)
	}
	if second.Source != "cache" {
		t.Fatalf("二次应命中缓存，得到 %q", second.Source)
	}
	if api.calls != 1 {
		t.Fatalf("API 只应被调用 1 次，实际 %d 次", api.calls)
	}
}

// 缓存命中时也要能拿到完整的地区/商家（别只缓存了 source）。
func TestResolverCachePreservesFields(t *testing.T) {
	api := &stubAPI{info: &Info{Country: "DE", Region: "Frankfurt", Provider: "Hetzner", ASN: "AS24940"}}
	resolver := NewResolver(Options{API: api, Cache: NewCache(time.Hour, 10)})

	if _, err := resolver.Lookup(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("首次失败: %v", err)
	}
	info, err := resolver.Lookup(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("二次失败: %v", err)
	}
	if info.Region != "法兰克福" || info.Provider != "Hetzner" || info.ASN != "AS24940" {
		t.Fatalf("缓存丢失字段: %+v", info)
	}
	// 2026-10-09 规则变更：Name() = 国家中文-城市，商家不再进名字。
	if info.Name() != "德国-法兰克福" {
		t.Fatalf("缓存结果的 Name() = %q", info.Name())
	}
}

// ---------------------------------------------------------------------------
// ip-api 兜底的真实解析（用假服务器，不打真网络）
// ---------------------------------------------------------------------------

func TestIPAPIParsesRealShapedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验字段参数确实带上了（少要字段等于白拿带宽）。
		if !strings.Contains(r.URL.RawQuery, "fields=") {
			t.Errorf("请求应带 fields 参数，实际 query=%q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "status":"success",
		  "country":"Japan","countryCode":"JP","city":"Tokyo","regionName":"Tokyo",
		  "isp":"Amazon.com, Inc.","org":"Amazon.com, Inc.",
		  "as":"AS16509 Amazon.com, Inc.","query":"203.0.113.17"
		}`))
	}))
	defer server.Close()

	provider := IPAPI{BaseURL: server.URL}
	info, err := provider.Lookup(context.Background(), server.Client(), "203.0.113.17")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if info.Region != "Tokyo" || info.Provider != "AWS" {
		t.Fatalf("期望 Tokyo/AWS，得到 %+v", info)
	}
	if info.Country != "JP" || info.ASN != "AS16509" {
		t.Fatalf("国家/ASN 解析错误: %+v", info)
	}
}

func TestIPAPIHandlesFailEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"fail","message":"reserved range","query":"10.0.0.1"}`))
	}))
	defer server.Close()

	provider := IPAPI{BaseURL: server.URL}
	if _, err := provider.Lookup(context.Background(), server.Client(), "10.0.0.1"); err == nil {
		t.Fatal("status=fail 应当返回错误")
	}
}

func TestIPAPIHandlesRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	provider := IPAPI{BaseURL: server.URL}
	_, err := provider.Lookup(context.Background(), server.Client(), "1.2.3.4")
	if err == nil {
		t.Fatal("429 应当返回错误")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("错误信息应当能看出是限速，实际 %v", err)
	}
}

func TestIPAPIHandlesGarbageBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	provider := IPAPI{BaseURL: server.URL}
	if _, err := provider.Lookup(context.Background(), server.Client(), "1.2.3.4"); err == nil {
		t.Fatal("非 JSON 响应应当返回错误")
	}
}

// 安全客户端必须拒绝环回/私网目标 —— 防止 geo 成为内网探测跳板。
func TestSafeClientRejectsNonPublicTargets(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1:9/", "http://10.0.0.1:9/", "http://169.254.169.254/"} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			continue
		}
		resp, err := safeClient.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Errorf("安全客户端应当拒绝 %s", target)
		}
	}
}

// 2026-11-03：响应体必须有上限——一个 8 MiB 的「JSON」不能被整个读进来再解析。
func TestIPAPIBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","country":"` + strings.Repeat("A", 8<<20) + `","query":"1.2.3.4"}`))
	}))
	defer server.Close()
	_, err := IPAPI{BaseURL: server.URL}.Lookup(context.Background(), server.Client(), "1.2.3.4")
	if err == nil {
		t.Fatal("an oversized response must be rejected, not parsed")
	}
}

// 安全客户端与 IsPublicIP 必须拦截 CGNAT（含云元数据）、NAT64 及其他特殊保留段，杜绝 SSRF 旁路。
func TestSafeClientAndIsPublicIPRejectSpecialUseRanges(t *testing.T) {
	badIPs := []string{"100.100.100.200", "64:ff9b::7f00:1", "0.1.2.3", "240.0.0.1", "::127.0.0.1", "100::1", "fec0::1"}
	for _, ip := range badIPs {
		if IsPublicIP(ip) {
			t.Errorf("IsPublicIP(%q) = true，应拒绝特殊保留段", ip)
		}
	}
	for _, target := range []string{
		"http://100.100.100.200/latest/meta-data/",
		"http://[64:ff9b::7f00:1]:80/",
		"http://0.1.2.3:80/",
		"http://240.0.0.1:80/",
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatalf("构造请求 %s 失败: %v", target, err)
		}
		resp, err := safeClient.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Errorf("safeClient 应当拒绝特殊保留段目标 %s", target)
		}
	}
}

// Resolver 缓存读写须统一使用注入时钟，推进时钟超过 TTL 后必须失效重新查询。
func TestResolverCacheRespectsInjectedClock(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	api := &stubAPI{info: &Info{Region: "Tokyo", Provider: "AWS"}}
	resolver := NewResolver(Options{API: api, Cache: NewCache(5*time.Minute, 10)})
	resolver.now = func() time.Time { return now }

	if _, err := resolver.Lookup(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := resolver.Lookup(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("命中缓存失败: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("未过期前应命中缓存，实际调用 API %d 次", api.calls)
	}
	now = now.Add(6 * time.Minute)
	if _, err := resolver.Lookup(context.Background(), "1.2.3.4"); err != nil {
		t.Fatalf("过期后重查失败: %v", err)
	}
	if api.calls != 2 {
		t.Fatalf("推进注入时钟超过 TTL 后应重新查 API，实际调用 %d 次", api.calls)
	}
}
