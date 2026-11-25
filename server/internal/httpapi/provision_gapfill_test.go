package httpapi

import (
	"context"
	"strings"
	"testing"
)

// 新节点必须主动给存量订阅开户：新建节点、重新上架、每小时兜底三处都要排队。
func TestProvisionGapFillWired(t *testing.T) {
	if body := funcBlock(t, "server.go", "func (s *Server) createNode("); !strings.Contains(body, "queueProvisioningGapsAsync(") {
		t.Fatal("新建节点后没有给存量订阅排队开户")
	}
	if body := funcBlock(t, "admin.go", "func (s *Server) updateNode("); !strings.Contains(body, "queueProvisioningGapsAsync(") {
		t.Fatal("节点重新上架后没有给存量订阅排队开户")
	}
	if body := funcBlock(t, "node_account_reconcile.go", "func (s *Server) StartNodeAccountReconcile("); !strings.Contains(body, "queueProvisioningGaps(") {
		t.Fatal("缺少每小时的开户兜底")
	}
	body := funcBlock(t, "provision_gapfill.go", "func (s *Server) queueProvisioningGapsOnce(")
	// 判据与拉订阅时的 JIT 同源，执行走同步队列（持锁 + 退避），不另写一套开户逻辑。
	for _, want := range []string{"needsAccountProvisioning(", "queueSubscriptionSync("} {
		if !strings.Contains(body, want) {
			t.Fatalf("gap-fill 应调用 %s", want)
		}
	}
}

// 2026-11-04：进行中时到达的触发不能被丢弃，跑完要补跑一轮。
func TestGapFillTriggerDuringRunIsNotLost(t *testing.T) {
	gapFillInFlight.Lock()
	gapFillRerun.Store(false)
	s := &Server{}
	if n := s.queueProvisioningGaps(context.Background(), "fixture"); n != 0 {
		t.Fatalf("a concurrent call must not run, got %d", n)
	}
	gapFillInFlight.Unlock()
	if !gapFillRerun.Load() {
		t.Fatal("a trigger that arrives while a run is in flight must be recorded for a rerun")
	}
	gapFillRerun.Store(false)
	body := funcBlock(t, "provision_gapfill.go", "func (s *Server) queueProvisioningGaps(")
	if !strings.Contains(body, "if !gapFillRerun.Load() || ctx.Err() != nil {") {
		t.Fatal("the running call must loop again when a rerun was requested")
	}
}

// 2026-11-04：在「最后一次检查 rerun」与「解锁」之间到来的触发也不能丢（解锁后要再看一次）。
func TestGapFillTriggerInUnlockWindowIsNotLost(t *testing.T) {
	runs := 0
	oldOnce, oldHook := gapFillOnce, gapFillBeforeUnlock
	gapFillOnce = func(*Server, context.Context, string) int { runs++; return 0 }
	fired := false
	gapFillBeforeUnlock = func() {
		if !fired {
			fired = true
			gapFillRerun.Store(true) // 模拟另一个 goroutine 此刻 TryLock 失败、只置位 rerun
		}
	}
	t.Cleanup(func() { gapFillOnce, gapFillBeforeUnlock = oldOnce, oldHook; gapFillRerun.Store(false) })
	gapFillRerun.Store(false)
	(&Server{}).queueProvisioningGaps(context.Background(), "fixture")
	if runs != 2 {
		t.Fatalf("a trigger in the unlock window must cause one more run, got %d runs", runs)
	}
}
