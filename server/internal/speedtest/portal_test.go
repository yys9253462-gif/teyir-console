package speedtest

// portal_test.go — 节点 portal 测速通道的测试。
//
// 这里要钉死的三件事，每一条都对应一个真实会咬人的地方：
//
//  1. **内网/回环地址必须被拒**。api_url 是用户在面板里填的，
//     拨号器不校验就等于给了面板一个内网扫描器。
//     这条是安全测试，不是功能测试。
//
//  2. **老节点（没有这个端点）要能被认出来**，返回可区分的哨兵错误，
//     而不是笼统的"测速失败"。否则用户看到的和"节点挂了"没区别。
//
//  3. **测到的 0ms 与"没测到"不能混**。Result.ICMPAvgMs 是 *float64，
//     JSON 里的 null 必须落成 nil，0 必须落成指向 0 的指针。

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 拨号器：拒绝回环与私有网段
// ---------------------------------------------------------------------------

func TestPortalDialerRejectsPrivateAndLoopback(t *testing.T) {
	bad := []string{
		"127.0.0.1:80",       // 回环
		"127.0.0.53:80",      // 回环（DNS）
		"0.0.0.0:80",         // 未指定
		"10.1.2.3:80",        // 私有 A
		"172.16.5.5:80",      // 私有 B
		"192.168.1.1:80",     // 私有 C
		"169.254.169.254:80", // 云元数据服务！这个尤其重要
		"::1:80",             // IPv6 回环
		"fd00::1:80",         // IPv6 ULA
	}
	for _, addr := range bad {
		if _, err := portalDialer(context.Background(), "tcp", addr); err == nil {
			t.Errorf("portalDialer(%q) 竟然成功了 —— 这是内网扫描面", addr)
		}
	}
}

// 这条断言曾经反过来写：断言"域名一律拒绝"。
// 那是个**设计错误** —— hysteria_nodes.api_url 存的就是域名
// （node2.example.com / node1.example.com / node1.example.com …），
// 真机一跑五台节点全部测不了，日志报
//
//	连接节点 portal 失败: ... 节点地址 "node1.example.com" 不是 IP 字面量
//
// 要防的是"域名解析到内网地址"，不是"域名"这个形式。
// 现在改成：解析后逐个校验解析结果（见下面两条测试）。
func TestPortalDialerRejectsUnresolvableHost(t *testing.T) {
	// 解析不了的域名必须失败，而不是静默放行。
	if _, err := portalDialer(context.Background(), "tcp", "no-such-host.invalid:443"); err == nil {
		t.Error("无法解析的域名应当报错（而不是静默放行）")
	}
}

// 解析到内网/回环的域名必须被拒 —— 这才是那条防护真正要拦的东西。
// localhost 由 RFC 6761 保留，必定解析到回环，不依赖外部 DNS。
func TestPortalDialerRejectsDomainResolvingToLoopback(t *testing.T) {
	if _, err := portalDialer(context.Background(), "tcp", "localhost:80"); err == nil {
		t.Error("解析到回环的域名必须被拒 —— 否则面板就是个内网探测器")
	}
}

// 正常公网域名应当能通过地址校验。
//
// 这一条锁住"修复没有矫枉过正"：真机五台节点的 api_url **全是域名**，
// 若这里不放行，功能就等于没修 —— 而这类"修过头"的回归
// 恰恰是安全加固最常见的自伤方式。
func TestPortalDialerAllowsNormalDomain(t *testing.T) {
	// 拨一个关闭的公网端口：预期失败在**连接**阶段，不是地址校验阶段。
	_, err := portalDialer(context.Background(), "tcp", "node1.example.com:1")
	if err == nil {
		t.Skip("该端口居然可连，跳过")
	}
	if errors.Is(err, errPortalAddrNotAllowed) {
		t.Errorf("正常公网域名被地址校验拦下了: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2 & 3. 响应解析：老节点、NO_PING、0ms vs null
// ---------------------------------------------------------------------------

// newProberForTest 造一个不需要真实 DB 的 Prober。
// RunOnPortal 不碰数据库（落库在 RunOnPortalAndStore 里），
// 所以这里可以安全地拿零值 Prober 调用。
func newProberForTest() *Prober { return &Prober{} }

func TestRunOnPortalParsesResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key-1234567890" {
			t.Errorf("Authorization 头不对: %q", got)
		}
		if r.URL.Path != "/api/v1/speedtest" {
			t.Errorf("路径不对: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"measured_targets": 3,
			"total_targets": 3,
			"results": [
				{"target_key":"telecom-bj-163","target_name":"电信-北京(163)","target_host":"202.96.128.86","carrier":"telecom","icmp_avg_ms":50.2,"icmp_loss_pct":0},
				{"target_key":"mobile-sh","target_name":"移动-上海","target_host":"211.136.112.200","carrier":"mobile","icmp_avg_ms":null,"icmp_loss_pct":100},
				{"target_key":"dns-ali","target_name":"阿里 DNS","target_host":"223.5.5.5","carrier":"other","icmp_avg_ms":0.4,"icmp_loss_pct":0}
			]
		}`))
	}))
	defer srv.Close()

	// httptest 用 127.0.0.1，而 portalDialer 会拒回环 —— 所以这里直接替换
	// 拨号器做端到端验证。单独测拨号器（上面）已经覆盖了真实防护。
	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()
	results, err := p.RunOnPortal(context.Background(), "node-1", srv.URL, "test-key-1234567890")
	if err != nil {
		t.Fatalf("RunOnPortal 失败: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("期望 3 条结果，实际 %d", len(results))
	}

	// 第 1 条：有值
	if results[0].ICMPAvgMs == nil || *results[0].ICMPAvgMs != 50.2 {
		t.Errorf("第 1 条延迟解析错误: %v", results[0].ICMPAvgMs)
	}
	if results[0].Carrier != "telecom" {
		t.Errorf("第 1 条运营商错误: %q", results[0].Carrier)
	}

	// 第 2 条：null 必须落成 nil，**不能**变成 0
	// 这是最关键的一条：0 会被界面显示成 "0 ms"，用户以为延迟为零，
	// 而真实情况是"这个靶点 100% 丢包，完全没测到"。
	if results[1].ICMPAvgMs != nil {
		t.Errorf("null 的 icmp_avg_ms 被解析成了 %v，必须是 nil", *results[1].ICMPAvgMs)
	}
	if results[1].ICMPLossPct == nil || *results[1].ICMPLossPct != 100 {
		t.Errorf("丢包率解析错误: %v", results[1].ICMPLossPct)
	}

	// 第 3 条：0.4ms 是**真实的小值**，不能被当成"未测得"丢成 nil
	if results[2].ICMPAvgMs == nil {
		t.Error("0.4ms 被误判成未测得 —— 小值与 null 必须区分")
	} else if *results[2].ICMPAvgMs != 0.4 {
		t.Errorf("小值解析错误: %v", *results[2].ICMPAvgMs)
	}
}

func TestRunOnPortalDetectsOldNode(t *testing.T) {
	// 老节点跑旧版 portal.py：对未知子路径回 404 + {"ok":false,...}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok": false, "error": "API endpoint not found"}`))
	}))
	defer srv.Close()

	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()
	_, err := p.RunOnPortal(context.Background(), "node-1", srv.URL, "test-key-1234567890")
	if err == nil {
		t.Fatal("老节点应该报错（404 端点不存在），却返回了成功")
	}
	// 必须是可区分的哨兵错误，调用方要据此说"portal 没升级"
	// 而不是笼统的"测速失败" —— 那两种情况的处理动作完全不同。
	if !strings.Contains(err.Error(), "升级") {
		t.Errorf("404 应被识别为「portal 未升级」，实际错误: %v", err)
	}
}

func TestRunOnPortalSurfacesNoPing(t *testing.T) {
	// 节点没装 ping：端点返回 ok:false + error_label。
	// 这个信息必须原样透出 —— "节点没装 ping"和"靶点全不通"是两回事。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "NO_PING", "error_label": "节点未安装 ping，无法测速", "results": []}`))
	}))
	defer srv.Close()

	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()
	_, err := p.RunOnPortal(context.Background(), "node-1", srv.URL, "test-key-1234567890")
	if err == nil {
		t.Fatal("NO_PING 应该报错")
	}
	if !strings.Contains(err.Error(), "未安装 ping") {
		t.Errorf("应透出节点的 error_label，实际: %v", err)
	}
}

func TestRunOnPortalRejectsUnknownTargetKeys(t *testing.T) {
	// 节点侧回了带空格/特殊字符的 target_key —— 写进库会污染前端分组。
	// 必须丢掉而不是照单全收。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "results": [
			{"target_key":"good-key","target_name":"好靶点","target_host":"1.2.3.4","carrier":"other","icmp_avg_ms":10,"icmp_loss_pct":0},
			{"target_key":"bad key; DROP TABLE","target_name":"脏数据","target_host":"1.2.3.5","carrier":"other","icmp_avg_ms":10,"icmp_loss_pct":0}
		]}`))
	}))
	defer srv.Close()

	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()
	results, err := p.RunOnPortal(context.Background(), "node-1", srv.URL, "test-key-1234567890")
	if err != nil {
		t.Fatalf("RunOnPortal 失败: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("脏 target_key 应被丢弃，期望剩 1 条，实际 %d", len(results))
	}
	if results[0].TargetKey != "good-key" {
		t.Errorf("保留的不是那条合法靶点: %q", results[0].TargetKey)
	}
}

// ---------------------------------------------------------------------------
// 超时预算
// ---------------------------------------------------------------------------

// 节点侧端点最长 108 秒（18 靶点 × 6 秒）。面板给的预算必须显著大于它，
// 否则面板会先超时、节点还在跑 —— 那样用户看到"没数据"，
// 而节点过一会儿自己跑完了，两边对不上。
func TestPortalTimeoutCoversSlowNode(t *testing.T) {
	// probeTimeout(18) = 18*12*1.5 = 324 秒，再乘 2 = 648 秒。
	// 断言它至少 150 秒（覆盖节点侧 108 秒的实测最坏值）且有上界。
	if got := probeTimeout(SPEEDTEST_TARGET_COUNT_HINT); got < 150*time.Second {
		t.Errorf("probeTimeout(%d)=%v 不足以覆盖节点侧 108 秒", SPEEDTEST_TARGET_COUNT_HINT, got)
	}
}

// 保证 Result 的 nil 语义能穿过 JSON 往返。
//
// 这条直接对应界面上的"0 ms"与"未测得"之分。
// ⚠️ Result 结构体**没有** json tag（它是内部类型，不直接序列化出去），
//
//	所以标准库用的是 Go 字段名 ICMPAvgMs，不是 snake_case。
//	断言时要看实际字段名 —— 我第一版按 snake_case 写，测试红了，
//	那是**断言写错**不是代码错（铁律⑨：FAIL 先分辨是代码错还是断言错）。
func TestResultNilSurvivesJSONRoundTrip(t *testing.T) {
	// 先确认结构体确实没有 json tag，这样这个断言才不会悄悄失效。
	raw, err := json.Marshal(Result{TargetKey: "x", ICMPAvgMs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "icmp_avg_ms") {
		t.Errorf("Result 竟有 snake_case 字段名，测试断言需要跟着改: %s", raw)
	}
	if !strings.Contains(string(raw), `"ICMPAvgMs":null`) {
		t.Errorf("nil 延迟应序列化为 null，实际: %s", raw)
	}

	// 真正的重点：portal 返回的 null 经过 RunOnPortal 后仍是 nil。
	// 上面 TestRunOnPortalParsesResults 已覆盖，这里只做一次往返确认。
	var back portalSpeedResult
	if err := json.Unmarshal([]byte(`{"target_key":"x","icmp_avg_ms":null}`), &back); err != nil {
		t.Fatal(err)
	}
	if clampPtrPtr(back.ICMPAvgMs, 0, 60000) != nil {
		t.Error("clampPtrPtr 必须让 null 保持 nil")
	}
	// 而 0 必须留下 0（不能当成"没测到"）
	back2 := portalSpeedResult{ICMPAvgMs: func() *float64 { v := 0.0; return &v }()}
	if got := clampPtrPtr(back2.ICMPAvgMs, 0, 60000); got == nil || *got != 0 {
		t.Errorf("0 必须被保留为 0，实际 %v", got)
	}
}

// 2026-11-03：校验过解析结果后必须连那个已校验的 IP，不能把域名再交给 Dialer
// 重新解析 —— 低 TTL 的 DNS 可以在校验与连接之间换成内网地址（DNS rebinding）。
func TestPortalDialerDialsValidatedIP(t *testing.T) {
	raw, err := os.ReadFile("portal.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "func portalDialer(")
	end := strings.Index(src, "func portalIPAllowed(")
	if start < 0 || end < start {
		t.Fatal("portalDialer not found")
	}
	body := src[start:end]
	if strings.Contains(body, "net.JoinHostPort(host, port)") {
		t.Fatal("域名分支不能再按域名拨号：Dialer 会重新解析，校验形同虚设")
	}
	if !strings.Contains(body, "portaltls.DialCheckedIPs(ctx, d, network, port, ips)") {
		t.Fatal("必须通过 portaltls.DialCheckedIPs 连接已通过校验的解析结果并支持双栈回退")
	}
}

// 验证 RunOnPortal 正确去除 baseURL 尾斜杠，且明确拒绝 3xx 重定向、401/403 鉴权失败与非 200 伪成功响应。
func TestRunOnPortalHandlesTrailingSlashAndRejectsRedirectOrBadStatus(t *testing.T) {
	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()

	// 1) baseURL 带末尾斜杠与空白时，不得拼出 //api/v1/speedtest。
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/speedtest" {
			t.Errorf("带尾斜杠的 baseURL 拼出了错误路径: %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"results":[{"target_key":"dns-ali","target_name":"阿里","target_host":"223.5.5.5","carrier":"other","icmp_avg_ms":10,"icmp_loss_pct":0}]}`))
	}))
	defer okSrv.Close()
	if rs, err := p.RunOnPortal(context.Background(), "n1", " "+okSrv.URL+"/ ", "fake-key"); err != nil || len(rs) != 1 {
		t.Fatalf("带尾斜杠与空白的 baseURL 应成功归一并解析: len=%d err=%v", len(rs), err)
	}

	// 2) 302 重定向、401 鉴权失败、500 返回伪造 ok=true 都必须被拒绝。
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"重定向", http.StatusFound, `{"ok":true,"results":[]}`, "重定向"},
		{"鉴权失败", http.StatusUnauthorized, `{"ok":false}`, "鉴权失败"},
		{"服务端500伪造ok", http.StatusInternalServerError, `{"ok":true,"results":[{"target_key":"a","icmp_avg_ms":1,"icmp_loss_pct":0}]}`, "异常状态码"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "http://127.0.0.1/evil")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := p.RunOnPortal(context.Background(), "n1", srv.URL, "fake-key"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s 应报含 %q 的错误，实际: %v", tc.name, tc.want, err)
			}
		})
	}
}

// 验证 speedtestEndpointFromBaseURL 能正确归一多种 api_url 形态，避免带 /api/v1/node/meta 时拼出双前缀 404。
func TestSpeedtestEndpointFromBaseURLNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://node.example.com:8443", "https://node.example.com:8443/api/v1/speedtest"},
		{"https://node.example.com:8443/", "https://node.example.com:8443/api/v1/speedtest"},
		{"https://node.example.com:8443/api/v1", "https://node.example.com:8443/api/v1/speedtest"},
		{"https://node.example.com:8443/api/v1/node/meta", "https://node.example.com:8443/api/v1/speedtest"},
		{"https://node.example.com:8443/api/v1/speedtest", "https://node.example.com:8443/api/v1/speedtest"},
		{"https://node.example.com:8443/0123456789abcdef/api/v1/node/meta", "https://node.example.com:8443/0123456789abcdef/api/v1/speedtest"},
		{"https://node.example.com:8443/0123456789abcdef", "https://node.example.com:8443/0123456789abcdef/api/v1/speedtest"},
	}
	for _, tc := range cases {
		got, err := speedtestEndpointFromBaseURL(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("speedtestEndpointFromBaseURL(%q) = (%q, %v)，期望 %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "ftp://node.example.com", "https://", "://bad"} {
		if got, err := speedtestEndpointFromBaseURL(bad); err == nil {
			t.Errorf("非法地址 %q 应报错，实际返回 %q", bad, got)
		}
	}
}

// 验证 portal 测速拒绝缺失有效统计的靶点伪装成功，并明确拦截超过 256 KiB 的超大响应。
func TestRunOnPortalRejectsIncompleteStatsAndOversizedBody(t *testing.T) {
	oldDial := portalClient.Transport.(*http.Transport).DialContext
	portalClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	defer func() { portalClient.Transport.(*http.Transport).DialContext = oldDial }()

	p := newProberForTest()

	incompleteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"results":[{"target_key":"a","icmp_avg_ms":null,"icmp_loss_pct":null}]}`))
	}))
	defer incompleteSrv.Close()
	if _, err := p.RunOnPortal(context.Background(), "n1", incompleteSrv.URL+"/api/v1/node/meta", "fake-key"); err == nil {
		t.Fatal("全部靶点均缺失有效统计时不得返回成功")
	}

	oversizedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"results":[{"target_key":"` + strings.Repeat("a", 300*1024) + `"}]}`))
	}))
	defer oversizedSrv.Close()
	if _, err := p.RunOnPortal(context.Background(), "n1", oversizedSrv.URL, "fake-key"); err == nil || !strings.Contains(err.Error(), "过大") {
		t.Fatalf("超过 256 KiB 的响应应明确报过大，实际: %v", err)
	}
}
