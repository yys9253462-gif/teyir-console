package httpapi

import (
	"context"
	"testing"
)

// 2026-11-03：待办登记失败必须报告给调用方 —— 原来函数不返回错误，
// 调用方照样回「已登记、后台会重试」，而这条订阅再也没有任何路径会去补做。
func TestQueuePendingNodeActionReportsFailure(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	// fixDB 里没有 subscription_pending_node_actions 表：INSERT 必然失败。
	if err := s.queuePendingNodeAction(context.Background(), fixSub, pendingActionRevoke, false, "fixture"); err == nil {
		t.Fatal("a failed queue write must be returned to the caller")
	}
	if _, err := db.Exec(context.Background(), `CREATE TABLE subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean, next_attempt_at timestamptz, last_error text, register_revoked_order boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	// 客户端已断开的 ctx 也要能登记成功（这是唯一的补做线索）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.queuePendingNodeAction(ctx, fixSub, pendingActionRevoke, false, "fixture"); err != nil {
		t.Fatalf("queue write must not depend on the client connection: %v", err)
	}
}
