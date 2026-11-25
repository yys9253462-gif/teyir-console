package collector

import (
	"os"
	"strings"
	"testing"

	"nodeops/server/internal/sshclient"
)

// 2026-11-03：读节点 users/list 的客户端必须拒绝重定向，否则 30x 能把带 Bearer 的请求带到别处。
func TestUsageClientRefusesRedirects(t *testing.T) {
	if usageClient.CheckRedirect == nil {
		t.Fatal("usageClient must refuse redirects")
	}
	if err := usageClient.CheckRedirect(nil, nil); err == nil {
		t.Fatal("usageClient.CheckRedirect must return an error")
	}
}

// 2026-11-03：连不上时必须写明原因，指纹不一致（安全信号）不能和断网长得一样。
func TestSSHDialFailureReasonIsTyped(t *testing.T) {
	mismatch := sshDialFailureReason(&sshclient.HostKeyMismatchError{Expected: "SHA256:a", Got: "SHA256:b"})
	if !strings.Contains(mismatch, "指纹") {
		t.Errorf("host key mismatch must be named: %q", mismatch)
	}
	if got := sshDialFailureReason(errFromString("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]")); !strings.Contains(got, "凭据") {
		t.Errorf("auth failure must be named: %q", got)
	}
	if got := sshDialFailureReason(errFromString("dial tcp 1.2.3.4:22: i/o timeout")); !strings.Contains(got, "连接失败") {
		t.Errorf("network failure must be named: %q", got)
	}
	src, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "status_error=$2, updated_at=now() WHERE id=$1`, t.id, sshDialFailureReason(err))") {
		t.Fatal("dial failure branch must persist the reason into servers.status_error")
	}
}

// 2026-11-03：主机指标采集失败的两个分支都必须写 status_error，不能只累计次数。
func TestMetricsFailuresRecordReason(t *testing.T) {
	src, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if n := strings.Count(text, "SET consecutive_host_failures = consecutive_host_failures + 1,\n       status = CASE WHEN consecutive_host_failures + 1 >= $3 THEN 'warning' ELSE status END,\n       status_error = $4,"); n != 2 {
		t.Fatalf("both metrics-failure branches must write status_error, found %d", n)
	}
	if got := firstNonEmptyString("", "<nil>", "退出码 2"); got != "退出码 2" {
		t.Fatalf("firstNonEmptyString = %q", got)
	}
}

// 2026-11-03：用量采集预算用完时，剩下的订阅必须标为未采到，不能直接 return 留着旧数据。
func TestUsageDeadlineMarksRemainingStale(t *testing.T) {
	src, err := os.ReadFile("usage.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, required := range []string{"usageFetchContext(ctx)", "detachedWriteContext(ctx, 5*time.Second)", "if !cached {", "c.markUsageStale(ctx, j.subID"} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing bounded successful-result/stale handling: %s", required)
		}
	}
}
