package httpapi

import (
	"strings"
	"testing"
)

// M17：调封禁服务的口令优先用独立的 BAN_WEBHOOK_TOKEN，未配置才回退 CREDENTIAL_KEY。
func TestBanWebhookTokenPrefersDedicatedToken(t *testing.T) {
	t.Setenv("CREDENTIAL_KEY", "credential-master-key")
	t.Setenv("BAN_WEBHOOK_TOKEN", "dedicated-ban-token")
	if got := banWebhookToken(); got != "dedicated-ban-token" {
		t.Fatalf("配置了 BAN_WEBHOOK_TOKEN 时必须用它，got %q", got)
	}
	t.Setenv("BAN_WEBHOOK_TOKEN", "  ")
	if got := banWebhookToken(); got != "credential-master-key" {
		t.Fatalf("BAN_WEBHOOK_TOKEN 为空时回退 CREDENTIAL_KEY，got %q", got)
	}
}

// 两个调用点都必须走 banWebhookToken，不能再直接把 CREDENTIAL_KEY 放进请求头。
func TestBanWebhookCallersDoNotSendCredentialKey(t *testing.T) {
	src := readSourceFileAbs(t, "telegram_control.go")
	if strings.Contains(src, `"X-Ban-Auth", os.Getenv("CREDENTIAL_KEY")`) {
		t.Error("X-Ban-Auth 不能直接用 CREDENTIAL_KEY")
	}
	if n := strings.Count(src, `"X-Ban-Auth", banWebhookToken()`); n != 2 {
		t.Errorf("封禁与查询两处都应使用 banWebhookToken()，实际 %d 处", n)
	}
}
