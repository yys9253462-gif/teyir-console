package collector

// nodeapi_probe_test.go — 钉死 API 探测路径的两件关键事：
//
//   1. **SSRF 防护**：探测地址来自数据库里的 api_url，而那个值是用户填的。
//      没有这层防护，面板就变成了一个可以探测内网的跳板 ——
//      而面板本身跑在生产机上，内网里有数据库、有别的服务。
//   2. **失败分类**：unreachable 与 unauthorized 必须分开。
//      前者是"去查机器"，后者是"去换密钥"，处理动作完全不同。
//      混在一起会让运维者查错方向。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testDial 是给 httptest 用的拨号器：不做地址限制。
//
// 生产的 safeDialContext **刻意拒绝非公网地址**（必需的 SSRF 防护），
// 而 httptest 的服务器就在 127.0.0.1 —— 用生产拨号器的话，
// 所有需要真实 HTTP 往返的测试都拿不到响应，测不了状态解析。
// 防护本身由 TestSafeDialContextBlocksBeforeConnecting 单独验证。
func testDial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}
}

// probeForTest 是测试入口：用不受限的拨号器跑真实的 HTTP 往返。
func probeForTest(t *testing.T, baseURL, apiKey string, timeout time.Duration) nodeAPIProbe {
	t.Helper()
	return probeStatusViaAPIWithDial(context.Background(), baseURL, apiKey, timeout, testDial())
}

// ---------------------------------------------------------------------------
// 地址归一
// ---------------------------------------------------------------------------

func TestMetaEndpointFromAPIURL(t *testing.T) {
	// ⚠️ 这组期望值在修 404 bug 时全部更新过。
	//
	// 第一版函数返回的是 "base"，期望值也就是各种 base 形态 ——
	// 测试**全绿**，但生产上是 HTTP 404。原因是拼接发生在调用方
	// （`base + "/node/meta"`），而它漏了 `/api/v1` 前缀，
	// 测试压根没覆盖"最终请求的到底是哪个 URL"。
	//
	// 所以现在的期望值直接写**最终端点**。这不是"为了让测试通过而改期望"，
	// 而是把被测对象从"中间产物"改成"最终产物" —— 中间产物对了、
	// 最终产物错了，正是这类 bug 的藏身处。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// 生产上真实的形态就是这样（库里存的是裸 host:port）。
			// 第一版拼出的是 .../node/meta → 404。
			name: "只有主机与端口",
			in:   "https://node1.example.com:29421",
			want: "https://node1.example.com:29421/api/v1/node/meta",
		},
		{
			name: "带末尾斜杠",
			in:   "https://node1.example.com:29421/",
			want: "https://node1.example.com:29421/api/v1/node/meta",
		},
		{
			// 已经带完整路径：不该被重复拼接成
			// .../api/v1/node/meta/api/v1/node/meta
			name: "带完整 portal 路径",
			in:   "https://node1.example.com:29421/api/v1/node/meta",
			want: "https://node1.example.com:29421/api/v1/node/meta",
		},
		{
			// 带 token 段：保留它（能多解锁网页会话能力），
			// 补上 /api/v1/node/meta。
			name: "带 token 路径段 + portal 路径",
			in:   "https://host:29421/0123456789abcdef0123456789abcdef/api/v1/node/meta",
			want: "https://host:29421/0123456789abcdef0123456789abcdef/api/v1/node/meta",
		},
		{
			// 带 token 段但没写全路径 —— 补全它，而不是丢掉 token 段。
			name: "只带 token 段",
			in:   "https://host:29421/0123456789abcdef0123456789abcdef",
			want: "https://host:29421/0123456789abcdef0123456789abcdef/api/v1/node/meta",
		},
		{
			name: "http（内网/测试场景）",
			in:   "http://10.0.0.1:8080",
			want: "http://10.0.0.1:8080/api/v1/node/meta",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := metaEndpointFromAPIURL(tc.in)
			if err != nil {
				t.Fatalf("归一失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("metaEndpointFromAPIURL(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMetaEndpointFromAPIURLRejectsInvalid(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"ftp://host:1234",
		"file:///etc/passwd",
		"https://",
		"不是地址",
	}
	for _, in := range bad {
		if got, err := metaEndpointFromAPIURL(in); err == nil {
			t.Errorf("metaEndpointFromAPIURL(%q) 应当报错，却返回 %q", in, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SSRF 防护
// ---------------------------------------------------------------------------

// 拨号器必须拒绝一切非公网目标。
//
// 每一条都对应一类真实的攻击面：
//   - 127.0.0.1 / localhost：面板自己所在机器上的其他服务
//   - 169.254.169.254：云元数据端点（能拿到实例凭据！）
//   - 10./192.168./172.16.：内网里的数据库、别的业务
func TestSafeDialContextRejectsNonPublicTargets(t *testing.T) {
	dial := safeDialContext(2 * time.Second)
	targets := []string{
		"127.0.0.1:80",
		"localhost:80",
		"[::1]:80",
		"169.254.169.254:80", // 云元数据端点
		"10.0.0.1:80",
		"192.168.1.1:80",
		"172.16.0.1:80",
		"0.0.0.0:80",
	}
	for _, target := range targets {
		conn, err := dial(context.Background(), "tcp", target)
		if err == nil {
			conn.Close()
			t.Errorf("拨号器应当拒绝 %s —— 这会把面板变成内网探测跳板", target)
			continue
		}
		// 错误信息要能看出是"被策略拒绝"，而不是"连不上"——
		// 后者会让人以为是网络问题而去查网络。
		if !strings.Contains(err.Error(), "非公网") && !strings.Contains(err.Error(), "解析") {
			t.Errorf("拒绝 %s 的错误信息不够明确: %v", target, err)
		}
	}
}

// 非公网地址**绝不能被真的拨出去**（不止是返回错误，而是压根不建连）。
//
// 用一个必然拒绝连接的端口来区分：
// 若防护生效，错误是"非公网地址"；若防护失效，错误会是"connection refused"。
func TestSafeDialContextBlocksBeforeConnecting(t *testing.T) {
	// 起一个只在回环监听的服务器。若防护失效，拨号会真的连上它。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法在回环起监听: %v", err)
	}
	defer listener.Close()

	connected := make(chan struct{}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			// 有人连上了 —— 说明防护被绕过
			connected <- struct{}{}
			conn.Close()
		}
	}()

	dial := safeDialContext(2 * time.Second)
	_, err = dial(context.Background(), "tcp", listener.Addr().String())
	if err == nil {
		t.Fatal("拨号器应当拒绝回环地址")
	}

	select {
	case <-connected:
		t.Fatal("有连接真的到达了回环监听 —— 防护没有在拨号前拦住")
	case <-time.After(300 * time.Millisecond):
		// 正确：没有连接到达
	}
}

// ---------------------------------------------------------------------------
// 探测结果分类
// ---------------------------------------------------------------------------

// 401/403 必须归到 unauthorized —— 它证明"节点活着但密钥不对"，
// 与"节点挂了"是不同的处理动作。
func TestProbeStatusDistinguishesUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid key"}`))
	}))
	defer server.Close()

	result := probeForTest(t, server.URL, "wrong-key", 3*time.Second)
	if result.OK {
		t.Fatal("401 不该被判为成功")
	}
	if result.ErrorKind != "unauthorized" {
		t.Fatalf("401 应归类为 unauthorized，实际 %q", result.ErrorKind)
	}
}

// 成功的响应要能解析出端口与节点时间。
func TestProbeStatusParsesMeta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 密钥要从 Authorization 头取，不能靠 query 参数 ——
		// query 会进访问日志。
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/node/meta" {
			// ⚠️ 这里原本断言的是 "/node/meta"，也就是**我写错的那个路径**。
			// 于是测试忠实地验证了错误实现，生产上才暴露 404。
			//
			// 教训：测试断言必须来自**外部契约**（节点门户的 API 前缀是
			// /api/v1/），不能来自"我以为实现应该长什么样"。
			// 断言写错比代码写错更危险 —— 它会给你一个绿色的假象。
			t.Errorf("请求路径不对: %s（节点门户的 API 前缀是 /api/v1/）", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true,"time":1790880371,"meta":{"listen_port":14617,"public_ip":"1.2.3.4"},"users_count":3}`))
	}))
	defer server.Close()

	result := probeForTest(t, server.URL, "good-key", 3*time.Second)
	if !result.OK {
		t.Fatalf("应当探测成功，实际 %+v", result)
	}
	if result.ListenPort != 14617 {
		t.Fatalf("端口解析错误: %d", result.ListenPort)
	}
	if result.NodeTime != 1790880371 {
		t.Fatalf("节点时间解析错误: %d", result.NodeTime)
	}
}

// 信封 ok=false 时，**节点是活的**（它能应答就证明活着），
// 所以仍算探测成功，只是把节点的话带出来。
// 把它判成 offline 会让人跑去查一台其实好好的机器。
func TestProbeStatusTreatsEnvelopeFailAsAlive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"节点正在重启中"}`))
	}))
	defer server.Close()

	result := probeForTest(t, server.URL, "key", 3*time.Second)
	if !result.OK {
		t.Fatal("能应答就说明节点活着，信封 ok=false 不该判成不可达")
	}
	if result.Detail == "" {
		t.Error("应当把节点给的原因带出来，便于排障")
	}
}

// 响应不是 JSON（比如连到的是一个网页）要明确报出来 ——
// 这种配置错误很常见（把面板地址填成了别的服务）。
func TestProbeStatusRejectsNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>这是一个网页，不是节点门户</html>"))
	}))
	defer server.Close()

	result := probeForTest(t, server.URL, "key", 3*time.Second)
	if result.OK {
		t.Fatal("非 JSON 响应不该判为成功")
	}
	if result.ErrorKind != "invalid" {
		t.Fatalf("应归类为 invalid，实际 %q", result.ErrorKind)
	}
}

// 缺地址或缺密钥时要在**发出请求之前**就拒绝 ——
// 否则会拿空密钥去打对方的鉴权接口，白白触发对方的失败计数
// （很多服务会对连续失败做封禁）。
func TestProbeStatusRequiresAddressAndKey(t *testing.T) {
	if r := probeStatusViaAPI(context.Background(), "", "key", time.Second); r.ErrorKind != "invalid" {
		t.Errorf("空地址应报 invalid，实际 %q", r.ErrorKind)
	}
	if r := probeStatusViaAPI(context.Background(), "https://example.com", "", time.Second); r.ErrorKind != "invalid" {
		t.Errorf("空密钥应报 invalid，实际 %q", r.ErrorKind)
	}
}

// 大响应要被截断，不能吃满内存。
func TestProbeStatusLimitsResponseSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 返回远超上限的内容。它不会是合法 JSON，所以结果必是失败；
		// 关键是**不能 OOM**、要能正常返回。
		_, _ = w.Write([]byte(`{"ok":true,"padding":"` + strings.Repeat("x", 200*1024) + `"}`))
	}))
	defer server.Close()

	done := make(chan nodeAPIProbe, 1)
	go func() {
		done <- probeStatusViaAPIWithDial(context.Background(), server.URL, "key", 5*time.Second, testDial())
	}()
	select {
	case res := <-done:
		if res.OK || res.ErrorKind != "invalid" || !strings.Contains(res.Detail, "过大") {
			t.Fatalf("超大响应应明确返回 invalid 与过大提示，实际: %+v", res)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("大响应处理超时")
	}
}

// ---------------------------------------------------------------------------
// 错误信息
// ---------------------------------------------------------------------------

func TestShortErrStripsURL(t *testing.T) {
	// 真实形态：中间那段 URL 对排障是噪音（地址就写在相邻字段里）。
	raw := `Get "https://node1.example.com:29421/api/v1/node/meta": dial tcp 1.2.3.4:29421: i/o timeout`
	got := shortErr(errFromString(raw))
	if strings.Contains(got, "https://") {
		t.Errorf("应当去掉 URL 包装，实际 %q", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("应当保留真正的根因，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// SSH 失败分类：决定要不要退回 API 探测
// ---------------------------------------------------------------------------

// 「认证失败」与「网络不通」必须分开。
//
// 前者说明机器活着（它应答了 SSH 握手），只是凭据过期 —— 值得退回
// API 探测，否则节点永远显示 unknown，用户不知道换个密钥就好。
// 后者说明机器可能真挂了，再试 API 只是白等一次超时（那个 API 也在
// 同一台机器上）。
//
// 生产实测（2026-10-02）：Stockholm-AWS 的 SSH 报
// `ssh: handshake failed: ssh: unable to authenticate, attempted methods
//
//	[none], no supported methods remain` —— 正是前者。
func TestIsSSHAuthFailure(t *testing.T) {
	authFailures := []string{
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none], no supported methods remain",
		"ssh: unable to authenticate",
		"ssh: no supported methods remain",
		"Permission denied (publickey,password).",
	}
	for _, msg := range authFailures {
		if !isSSHAuthFailure(errFromString(msg)) {
			t.Errorf("应当识别为认证失败: %q", msg)
		}
	}

	networkFailures := []string{
		"dial tcp 1.2.3.4:22: i/o timeout",
		"dial tcp: connect: connection refused",
		"ssh: handshake failed: EOF",
		"dial tcp: lookup example.com: no such host",
		"host key mismatch: expected SHA256:aaa, got SHA256:bbb",
	}
	for _, msg := range networkFailures {
		if isSSHAuthFailure(errFromString(msg)) {
			t.Errorf("不该把网络故障误判成认证失败: %q", msg)
		}
	}

	if isSSHAuthFailure(nil) {
		t.Error("nil 不该被判为认证失败")
	}
}

// 主机密钥不匹配是一个**安全信号**，绝不能被当成"认证失败"而去退回 API。
//
// 那意味着对面可能换了一台机器（或中间有人）。此时的正确动作是停手
// 并提示用户核对，而不是"换个方式继续连"。
func TestHostKeyMismatchIsNotAuthFailure(t *testing.T) {
	msg := "ssh: handshake failed: host key mismatch: expected SHA256:abc, got SHA256:def"
	if isSSHAuthFailure(errFromString(msg)) {
		t.Fatal("主机密钥不匹配不该被判为认证失败 —— 那是安全信号，应当停手")
	}
}

// shortSSHErr 要有长度上限（错误信息可能很长，日志里刷屏）。
func TestShortSSHErrTruncates(t *testing.T) {
	long := errFromString(strings.Repeat("x", 500))
	got := shortSSHErr(long)
	if len(got) > 140 {
		t.Errorf("应当截断，实际长度 %d", len(got))
	}
	if shortSSHErr(nil) != "" {
		t.Error("nil 应返回空串")
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

func errFromString(s string) error { return stringError(s) }

// 2026-11-04：状态探测也要认 PORTAL_TLS_PINS 登记的自签证书（与开户、用量、测速同一套 TLS 规则）。
func TestProbeHonoursPortalTLSPins(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"time":1,"meta":{"listen_port":443}}`))
	}))
	defer server.Close()
	sum := sha256.Sum256(server.Certificate().Raw)
	host := strings.TrimPrefix(server.URL, "https://")

	t.Setenv("PORTAL_TLS_PINS", "")
	if got := probeStatusViaAPIWithDial(context.Background(), server.URL, "key", 5*time.Second, testDial()); got.OK {
		t.Fatal("a self-signed portal without a pin must not be trusted")
	}
	t.Setenv("PORTAL_TLS_PINS", `{"`+host+`":"`+hex.EncodeToString(sum[:])+`"}`)
	if got := probeStatusViaAPIWithDial(context.Background(), server.URL, "key", 5*time.Second, testDial()); !got.OK {
		t.Fatalf("a pinned self-signed portal must probe as reachable, got %+v", got)
	}
}
