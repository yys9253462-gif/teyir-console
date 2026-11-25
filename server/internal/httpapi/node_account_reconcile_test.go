package httpapi

import (
	"context"
	"strings"
	"testing"
)

func TestNodeReconcileSucceededRequiresCompleteRun(t *testing.T) {
	ok := nodeReconcileReport{NodesChecked: 1}
	if !nodeReconcileSucceeded(context.Background(), ok) {
		t.Fatal("完整成功的对账应记录完成时间")
	}
	for _, report := range []nodeReconcileReport{
		{},
		{NodesChecked: 1, NodeErrors: map[string]string{"node": "unreachable"}},
		{NodesChecked: 1, Failed: []nodeReconcileAccount{{Error: "delete failed"}}},
		{NodesChecked: 1, Skipped: []nodeReconcileAccount{{Reason: "bulk guard"}}},
	} {
		if nodeReconcileSucceeded(context.Background(), report) {
			t.Fatalf("不完整的对账不能记录完成时间: %+v", report)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if nodeReconcileSucceeded(ctx, ok) {
		t.Fatal("取消的对账不能记录完成时间")
	}
}

func TestNodeReconcileScheduleSkipsOnStateReadFailure(t *testing.T) {
	body := funcBlock(t, "node_account_reconcile.go", "func (s *Server) maybeReconcileNodeAccounts(")
	readFailure := strings.Index(body, "if err != nil && !errors.Is(err, pgx.ErrNoRows)")
	skip := strings.Index(body, "跳过本轮")
	reconcile := strings.Index(body, "s.ReconcileNodeAccounts(runCtx, false)")
	if readFailure < 0 || skip < 0 || reconcile < 0 || readFailure > reconcile || skip > reconcile {
		t.Fatal("failed scheduler-state reads must skip node reconciliation before it contacts node portals")
	}
}

func TestNodeReconcileSchedulerStopsBeforeDangerousWorkOnStateReadError(t *testing.T) {
	body := funcBlock(t, "node_account_reconcile.go", "func (s *Server) maybeReconcileNodeAccounts(")
	readFailure := strings.Index(body, "if err != nil && !errors.Is(err, pgx.ErrNoRows)")
	skip := strings.Index(body, "跳过本轮")
	reconcile := strings.Index(body, "s.ReconcileNodeAccounts(runCtx, false)")
	if readFailure < 0 || skip < 0 || reconcile < 0 || readFailure > reconcile || skip > reconcile {
		t.Fatal("failed scheduler-state reads must skip node reconciliation before it contacts node portals")
	}
}

func TestResidualReason(t *testing.T) {
	const node = "11111111-1111-1111-1111-111111111111"
	if residualReason(nil, node) == "" {
		t.Fatal("订阅行不存在的账号必须算残留")
	}
	live := &reconcileSubState{mapped: map[string]bool{}}
	if residualReason(live, node) != "" {
		t.Fatal("订阅行还在（未软删）时，即使映射缺这台节点也不能删：那是开户缺口，不是残留")
	}
	deletedMapped := &reconcileSubState{softDeleted: true, mapped: map[string]bool{node: true}}
	if residualReason(deletedMapped, node) != "" {
		t.Fatal("软删但映射里还有这台节点：回收流程仍在负责，对账不能抢着删")
	}
	deletedUnmapped := &reconcileSubState{softDeleted: true, mapped: map[string]bool{}}
	if residualReason(deletedUnmapped, node) == "" {
		t.Fatal("软删且映射里没有这台节点的账号必须算残留")
	}
}

func TestSubAccountIDPattern(t *testing.T) {
	ok := "sub_00000000-0000-4000-8000-000000000099"
	if m := subAccountIDPattern.FindStringSubmatch(ok); m == nil || m[1] != strings.TrimPrefix(ok, "sub_") {
		t.Fatalf("合法 sub_ 账号没认出来: %v", m)
	}
	for _, bad := range []string{"hy2_demo0000000001", "sub_abc", "sub_00000000-0000-4000-8000-000000000099x", "xsub_00000000-0000-4000-8000-000000000099"} {
		if subAccountIDPattern.MatchString(bad) {
			t.Fatalf("不该被对账碰到的账号: %s", bad)
		}
	}
}

func TestBulkGuard(t *testing.T) {
	if bulkGuardTripped(1, 1) {
		t.Fatal("少量残留不该触发保护")
	}
	if bulkGuardTripped(20, 25) {
		t.Fatal("恰好 20 个不触发（阈值是大于 20）")
	}
	if !bulkGuardTripped(21, 30) {
		t.Fatal("超过 20 且超过一半必须整台跳过")
	}
	if bulkGuardTripped(21, 100) {
		t.Fatal("超过 20 但不到一半不触发")
	}
}
