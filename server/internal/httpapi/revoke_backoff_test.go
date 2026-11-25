package httpapi

import (
	"strings"
	"testing"
)

// 2026-10-23：首尔离线后，过期订阅的回收每 2 分钟拨一次死节点、刷一条 ERROR，
// 并占住订阅锁。这里守住三件事：退避中的节点不再拨号、成功后清掉退避、
// 删除接口在回收不完整时不再回 200。
func TestRevokeSkipsBackedOffNodesButKeepsThemPending(t *testing.T) {
	body := funcBlock(t, "provisioning.go", "func (s *Server) revokeSubscriptionAccounts(")
	due := strings.Index(body, "s.revokeBackoffDue(ctx, rec.ID)")
	dial := strings.Index(body, `"/users/delete"`)
	if due < 0 || dial < 0 || due > dial {
		t.Fatal("must consult the backoff table before dialling any node")
	}
	skip := body[due:dial]
	if !strings.Contains(skip, "outcome.Failed = append") || !strings.Contains(skip, "continue") {
		t.Fatal("a backed-off node must be reported as failed (mapping kept), not silently treated as revoked")
	}
	if strings.Count(body, "s.recordRevokeBackoff(") < 2 || strings.Count(body, "s.clearRevokeBackoff(") < 2 {
		t.Fatal("both failure paths must record backoff and both success paths must clear it")
	}
}

func TestBackoffReadErrorsAbortRevocation(t *testing.T) {
	rec := funcBlock(t, "provisioning.go", "func (s *Server) recordRevokeBackoff(")
	if !strings.Contains(rec, "LEAST(interval '1 hour'") {
		t.Fatal("backoff must be capped at one hour so a recovered node is revoked promptly")
	}
	due := funcBlock(t, "provisioning.go", "func (s *Server) revokeBackoffDue(")
	for _, want := range []string{"return nil, err", "rows.Scan(&nodeID, &until); err != nil", "rows.Err()"} {
		if !strings.Contains(due, want) {
			t.Errorf("backoff lookup must reject incomplete state; missing %q", want)
		}
	}
	body := funcBlock(t, "provisioning.go", "func (s *Server) revokeSubscriptionAccounts(")
	lookup := strings.Index(body, "deferred, backoffErr := s.revokeBackoffDue(ctx, rec.ID)")
	dial := strings.Index(body, `"/users/delete"`)
	if lookup < 0 || dial < 0 || lookup >= dial || !strings.Contains(body[lookup:dial], "if backoffErr != nil") {
		t.Fatal("a backoff lookup failure must stop before any node delete request")
	}
}

func TestDeleteSubscriptionReportsIncompleteRevoke(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) deleteSubscriptionHandler(")
	if !strings.Contains(body, "s.purgeSubscriptionOutcome(") || !strings.Contains(body, "http.StatusBadGateway") {
		t.Fatal("delete must surface a failed node revoke instead of answering ok:true")
	}
}

func TestAllBackedOff(t *testing.T) {
	if allBackedOff(subscriptionRevokeOutcome{}) {
		t.Fatal("no failures is not 'all backed off'")
	}
	if !allBackedOff(subscriptionRevokeOutcome{Failed: []subscriptionRevokeFailure{{Reason: "节点上次回收失败，退避中，10-23 12:00 后重试"}}}) {
		t.Fatal("backed-off failures should be recognised")
	}
	if allBackedOff(subscriptionRevokeOutcome{Failed: []subscriptionRevokeFailure{{Reason: "退避中"}, {Reason: "dial tcp: i/o timeout"}}}) {
		t.Fatal("a real dial failure must still be logged")
	}
}
