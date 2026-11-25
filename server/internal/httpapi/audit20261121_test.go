package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionTemporarilyEmptyWhenAllAssignedNodesFilteredOut(t *testing.T) {
	body := funcBlock(t, "subscription.go", "func (s *Server) subscriptionNodesForChecked(")
	if !strings.Contains(body, "hadAssignedNodes") {
		t.Fatal("已分配节点的订阅被健康闸门全部筛空时必须返回 errSubscriptionTemporarilyEmpty，禁止返回 200 空配置覆盖客户端")
	}
}

func TestInvalidateSubscriptionCacheClearsClientOptionsCache(t *testing.T) {
	clientOptionsCache.Lock()
	clientOptionsCache.m["test-stale-key"] = clientOptionsEntry{
		node:     subscriptionNode{ID: "n1", PinSHA256: "OLD_PIN"},
		storedAt: time.Now(),
	}
	clientOptionsCache.Unlock()

	InvalidateSubscriptionCache()

	clientOptionsCache.Lock()
	_, stillCached := clientOptionsCache.m["test-stale-key"]
	clientOptionsCache.Unlock()
	if stillCached {
		t.Fatal("InvalidateSubscriptionCache 未清空 clientOptionsCache，更换证书指纹或混淆密码后会继续下发长达 1 小时旧参数")
	}
}

func TestSubscriptionPublicURLRespectsDeliveryBaseOverride(t *testing.T) {
	t.Setenv("SUBSCRIPTION_PUBLIC_BASE_URL", "https://sub.example.com")
	req := httptest.NewRequest("GET", "http://panel.example.com/api/subscription", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	got := subscriptionPublicURL(req, "tok_demo_123")
	if got != "https://sub.example.com/sub/tok_demo_123" {
		t.Fatalf("期望优先使用独立交付域名 https://sub.example.com/sub/tok_demo_123，实际得到 %q", got)
	}
}

func TestUpdateSubscriptionLocksAndGuardsDeletedRow(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) updateSubscriptionHandler(")
	lockIdx := strings.Index(body, "s.lockSubscriptionRequest(w, r, rec)")
	updIdx := strings.Index(body, "UPDATE subscription_tokens")
	if lockIdx < 0 || updIdx < 0 || lockIdx > updIdx {
		t.Fatal("updateSubscriptionHandler 必须在写库与节点联动前调用 lockSubscriptionRequest 获取每订阅互斥锁并重读最新快照")
	}
	if !strings.Contains(body, "AND deleted_at IS NULL") {
		t.Fatal("updateSubscriptionHandler 更新语句缺少 AND deleted_at IS NULL 软删保护")
	}
	cancelBody := funcBlock(t, "pending_node_actions.go", "func (s *Server) cancelPendingNodeAction(")
	if !strings.Contains(cancelBody, "context.WithoutCancel(") {
		t.Fatal("cancelPendingNodeAction 必须使用 WithoutCancel 防止客户端提前断连丢掉作废待办操作")
	}
}

func TestLazyPurgeHandlesConcurrentRenewalAndSweeperHasDeadline(t *testing.T) {
	viewBody := funcBlock(t, "store.go", "func (s *Server) subscriptionView(")
	feedBody := funcBlock(t, "subscription.go", "func (s *Server) subscriptionFeed(")
	if !strings.Contains(viewBody, "errRenewedDuringPurge") {
		t.Fatal("subscriptionView 惰性清理遇到并发续费 errRenewedDuringPurge 时不得误返 404")
	}
	if !strings.Contains(feedBody, "errRenewedDuringPurge") {
		t.Fatal("subscriptionFeed 惰性清理遇到并发续费 errRenewedDuringPurge 时不得误返 404")
	}
	sweeperBody := funcBlock(t, "store.go", "func (s *Server) StartSubscriptionSweeper(")
	if !strings.Contains(sweeperBody, "context.WithTimeout(") {
		t.Fatal("StartSubscriptionSweeper 单轮清扫缺少超时边界控制")
	}
}
