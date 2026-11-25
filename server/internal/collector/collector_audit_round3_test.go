package collector

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 验证 /proc/loadavg 规范化：正常、多空白、非法文本都能稳定产出合法 JSON 数组。
func TestFormatLoadAverageJSON(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.12 0.34 0.56", "[0.12,0.34,0.56]"},
		{"  0.50\t  1.25   2.00 ", "[0.5,1.25,2]"},
		{"", "[]"},
		{"0.10 NaN 0.20", "[]"},
		{"-0.1 0.2 0.3", "[]"},
		{"not-a-number", "[]"},
	}
	for _, tc := range cases {
		got := formatLoadAverageJSON(tc.in)
		if got != tc.want {
			t.Errorf("formatLoadAverageJSON(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
		var parsed []float64
		if err := json.Unmarshal([]byte(got), &parsed); err != nil {
			t.Errorf("formatLoadAverageJSON(%q) 产出非合法 JSON %q: %v", tc.in, got, err)
		}
	}
}

// 验证 SSH 冷却映射会自动回收已过期条目与已删除主机条目，防止长期运行内存泄漏。
func TestPruneSSHRetryRemovesExpiredAndInactive(t *testing.T) {
	now := time.Now()
	c := NewCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 30, nil)
	c.sshRetry["keep"] = sshRetryEntry{retryAfter: now.Add(5 * time.Minute)}
	c.sshRetry["expired"] = sshRetryEntry{retryAfter: now.Add(-time.Second)}
	c.sshRetry["deleted"] = sshRetryEntry{retryAfter: now.Add(5 * time.Minute)}

	c.pruneSSHRetry(map[string]struct{}{"keep": {}, "expired": {}}, now)
	if _, ok := c.sshRetry["keep"]; !ok {
		t.Fatal("活跃且未过期的冷却记录不应被清理")
	}
	if _, ok := c.sshRetry["expired"]; ok {
		t.Fatal("已过期的冷却记录应被清理")
	}
	if _, ok := c.sshRetry["deleted"]; ok {
		t.Fatal("已不在活跃主机集合中的冷却记录应被清理")
	}
}

// 验证 fetchPortalUsers 明确拒绝 3xx 重定向、403 拒绝访问、非 200 状态码及伪造 ok=true，
// 同时兼容老节点 404 返回 JSON error 的提示。
func TestFetchPortalUsersRejectsRedirectAndBadHTTPStatus(t *testing.T) {
	orig := usageClient.Transport
	usageClient.Transport = &http.Transport{DialContext: testDial(), DisableKeepAlives: true}
	defer func() { usageClient.Transport = orig }()

	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"重定向", http.StatusFound, `{"ok":true,"users":[]}`, "重定向"},
		{"无权限403", http.StatusForbidden, `forbidden`, "403"},
		{"网关502网页", http.StatusBadGateway, `<html>502</html>`, "HTTP 502"},
		{"异常500伪造ok", http.StatusInternalServerError, `{"ok":true,"users":[]}`, "HTTP 500"},
		{"老节点404带JSON错误", http.StatusNotFound, `{"ok":false,"error":"API endpoint not found"}`, "API endpoint not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "http://127.0.0.1/redirect")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := fetchPortalUsers(context.Background(), srv.URL, "fake-api-key")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s 期望包含 %q 的错误，实际: %v", tc.name, tc.want, err)
			}
		})
	}
}

// 验证 JSON null 字面量不会穿透到 jsonb 字段或阻断信封缺口回填。
func TestNormalizeJSONAndEnvelopeNullGaps(t *testing.T) {
	raw := `{"ok":true,"measurement_gaps":[{"source":"reality"}],"users":[{"user_id":"a","measurement_gaps":null},{"user_id":"b","measurement_gaps":[]}]}`
	var env portalUsageEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	applyEnvelopeGaps(env.Users, env.MeasurementGaps)
	if string(env.Users[0].MeasurementGaps) != `[{"source":"reality"}]` {
		t.Fatalf("用户侧 measurement_gaps 为 null 时应回填信封缺口，实际 %s", env.Users[0].MeasurementGaps)
	}
	if string(env.Users[1].MeasurementGaps) != `[]` {
		t.Fatalf("用户侧显式空数组不应被覆盖，实际 %s", env.Users[1].MeasurementGaps)
	}
	if string(normalizeJSONObject(json.RawMessage(` null `))) != `{}` {
		t.Fatal("null 对象应兜底为 {}")
	}
	if string(normalizeJSONArray(json.RawMessage(` null `))) != `[]` {
		t.Fatal("null 数组应兜底为 []")
	}
}

// 验证上游上下文已取消时，prefetchNodeUsers 绝不再派生新的并发抓取协程。
func TestPrefetchNodeUsersSkipsWhenContextAlreadyCanceled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"users":[]}`)
	}))
	defer srv.Close()

	orig := usageClient.Transport
	usageClient.Transport = &http.Transport{DialContext: testDial(), DisableKeepAlives: true}
	defer func() { usageClient.Transport = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Collector{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	perNode := map[string]map[string]portalUserUsage{}
	failures := map[string]string{}
	c.prefetchNodeUsers(ctx, []usageNodeTarget{{nodeID: "n1", apiURL: srv.URL}}, perNode, failures)
	if hits.Load() != 0 || len(perNode) != 0 || len(failures) != 0 {
		t.Fatalf("已取消上下文不应派生预取请求: hits=%d perNode=%v failures=%v", hits.Load(), perNode, failures)
	}
}

// 验证旧游标文件损坏（非法 JSON）时，远端快照脚本容错降级为 bootstrap=null 而不拖垮整次流量采集。
func TestRemoteSnapshotToleratesCorruptLegacyCursor(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("本机无 python，跳过内嵌快照脚本容错测试")
	}
	script := strings.SplitN(trafficSnapshotScript, "\n", 2)[1]
	script = strings.TrimSuffix(script, "METERPY\n")
	harness := `import io,sys
from unittest.mock import patch,Mock
script=sys.argv[1]
def read_text(path):
 return 'secret' if str(path)=='/fixture-secret' else '{corrupt-json'
def urlopen(*args,**kwargs):
 response=Mock()
 response.__enter__=Mock(return_value=io.StringIO('{"u":{"tx":10,"rx":20}}'))
 response.__exit__=Mock(return_value=False)
 return response
with patch('pathlib.Path.read_text',read_text),patch('pathlib.Path.exists',return_value=True),patch('subprocess.check_output',return_value=b'epoch-b'),patch('urllib.request.urlopen',urlopen),patch('sys.argv',['probe','/fixture-secret','http://example.invalid']),patch('time.time_ns',return_value=999):
 exec(compile(script,'remote-probe','exec'))
`
	out, err := exec.Command(python, "-I", "-c", harness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("损坏旧游标不应导致快照脚本崩溃: %v %s", err, out)
	}
	var snapshot trafficSnapshot
	if err := json.Unmarshal([]byte(parseKeyValues(string(out))["TRAFFIC"]), &snapshot); err != nil {
		t.Fatalf("解析快照输出失败: %v (%s)", err, out)
	}
	if snapshot.Bootstrap != nil || snapshot.Counters["u"].Tx != 10 || snapshot.Counters["u"].Rx != 20 {
		t.Fatalf("期望 bootstrap=nil 且保留当前流量计数，实际: %+v", snapshot)
	}
}

// 验证采集器 Start 同步登记后紧接取消与 Wait，无需任何 sleep 即可确定等待循环真正退出且无竞态。
func TestCollectorStartAndWaitExitsWithoutSleepOrRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		var exited atomic.Bool
		c := NewCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 1, nil)
		c.runBody = func(ctx context.Context) {
			<-ctx.Done()
			exited.Store(true)
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.Start(ctx)
		cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Wait()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 轮采集器取消后 Wait 超时未返回", i)
		}
		if !exited.Load() {
			t.Fatalf("第 %d 轮 Wait 在后台循环真正退出前抢跑返回", i)
		}
	}
}

// 验证兼容旧调用方式（go c.Run(ctx) 紧接 cancel 与 Wait）在高频并发下保持零竞态且不阻塞。
func TestCollectorConcurrentRunAndWaitNoRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := NewCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 1, nil)
		c.runBody = func(ctx context.Context) {
			<-ctx.Done()
		}
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			c.Run(ctx)
		}()
		cancel()
		c.Wait()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 轮并发 Run 取消后未退出", i)
		}
	}
}

