package collector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/sshclient"
)

// 验证 SSH 冷却指纹纳入主机、端口、用户、主机密钥指纹与凭据内容，修改任一项都能立即解除冷却。
func TestCredentialDigestCoversConnectionAttributes(t *testing.T) {
	base := target{
		id:          "host-1",
		host:        "203.0.113.10",
		port:        22,
		user:        "root",
		fingerprint: "SHA256:aaa",
		cred:        sshclient.Credential{Type: "ssh_key", Secret: []byte("fake-key-material")},
	}
	baseSum := credentialDigest(base)

	mutants := []struct {
		name string
		mod  func(*target)
	}{
		{"修改主机地址", func(x *target) { x.host = "203.0.113.11" }},
		{"修改端口", func(x *target) { x.port = 2222 }},
		{"修改登录用户", func(x *target) { x.user = "ubuntu" }},
		{"重接受主机指纹", func(x *target) { x.fingerprint = "SHA256:bbb" }},
		{"修改凭据类型", func(x *target) { x.cred.Type = "password" }},
		{"修改凭据内容", func(x *target) { x.cred.Secret = []byte("fake-key-material-2") }},
	}
	for _, tc := range mutants {
		cp := base
		cp.cred.Secret = append([]byte(nil), base.cred.Secret...)
		tc.mod(&cp)
		if credentialDigest(cp) == baseSum {
			t.Errorf("%s 后冷却指纹未变化，修改配置后仍会被旧 10 分钟冷却卡住", tc.name)
		}
	}
}

// 验证 SSH 单机超时后降级 API 探测能拿到独立预算，但进程退出取消时立即终止。
func TestFallbackAPIProbeContextRespectsParentCancel(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	expiredHost, cancelHost := context.WithDeadline(parent, time.Now().Add(-time.Second))
	defer cancelHost()

	probeCtx, stopProbe := fallbackAPIProbeContext(parent, expiredHost)
	defer stopProbe()
	if probeCtx.Err() != nil {
		t.Fatalf("单机超时后降级探测上下文不应立即失效: %v", probeCtx.Err())
	}
	deadline, ok := probeCtx.Deadline()
	if !ok || time.Until(deadline) < 5*time.Second {
		t.Fatalf("降级探测预算过短: ok=%v remaining=%v", ok, time.Until(deadline))
	}

	cancelParent()
	select {
	case <-probeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("父进程取消后降级探测上下文未联动取消")
	}
}

// 验证用量映射 SQL 用正则+CASE 安全转换 UUID，不直接对任意键做 ::uuid 强转拖垮整轮采集。
func TestUsageMappingSQLUsesSafeUUIDGuard(t *testing.T) {
	if strings.Contains(usageMappingSQL, "WHERE key = (key::uuid)::text") {
		t.Fatal("usageMappingSQL 仍直接强转 key::uuid，遇到非法键会导致整轮 22P02 崩溃")
	}
	if !strings.Contains(usageMappingSQL, "CASE") || !strings.Contains(usageMappingSQL, "THEN key::uuid") {
		t.Fatal("usageMappingSQL 必须通过 CASE + 正则前置保护后再转 uuid")
	}
}

// 验证采集器源码守卫：SSH 超时后脱离单机上下文查 nodeHasAPI、进程取消不标离线、预拉取协程有 recover。
func TestCollectorRound2StructuralGuards(t *testing.T) {
	collectorSrc, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(collectorSrc)
	for _, want := range []string{
		"if errors.Is(parent.Err(), context.Canceled)",
		"hostCtx, stopFallback := fallbackAPIProbeContext(parent, hostCtx)",
		"status='online', status_error=''",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("collector.go 缺少关键加固语句: %q", want)
		}
	}

	usageSrc, err := os.ReadFile("usage.go")
	if err != nil {
		t.Fatal(err)
	}
	uText := string(usageSrc)
	for _, want := range []string{
		"if errors.Is(ctx.Err(), context.Canceled)",
		"FROM unnest($3::text[], $4::bigint[])",
		"if r := recover(); r != nil",
	} {
		if !strings.Contains(uText, want) {
			t.Errorf("usage.go 缺少关键加固语句: %q", want)
		}
	}
}
