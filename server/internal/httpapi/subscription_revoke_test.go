package httpapi

import (
	"strings"
	"testing"
)

func TestSubscriptionRevokeOutcomeRequiresEveryNode(t *testing.T) {
	if !(subscriptionRevokeOutcome{}).OK() {
		t.Fatal("empty legacy mapping should not require node-side revocation")
	}
	if (subscriptionRevokeOutcome{Revoked: 1, Failed: []subscriptionRevokeFailure{{NodeID: "node-a", UserID: "sub_x", Reason: "timeout"}}}).OK() {
		t.Fatal("any node-side failure must make the outcome unsuccessful")
	}
}

func TestSubscriptionRevokePersistsEachConfirmedNodeDeletion(t *testing.T) {
	body := funcBlock(t, "provisioning.go", "func (s *Server) revokeSubscriptionAccounts(")
	callAt := strings.Index(body, `"/users/delete"`)
	persistAt := strings.Index(body, "UPDATE subscription_tokens")
	countAt := -1
	if callAt >= 0 {
		if i := strings.Index(body[callAt:], "outcome.Revoked++"); i >= 0 {
			countAt = callAt + i
		}
	}
	if callAt < 0 || persistAt < 0 || countAt < 0 || !(callAt < persistAt && persistAt < countAt) {
		t.Fatal("a confirmed node deletion must persist removal of its node_accounts mapping before counting success")
	}
	// 已删节点的移交路径：只有 handOffDeletedNodeAccount 确认映射已摘除（返回 true）才计成功。
	handOffAt := strings.Index(body, "s.handOffDeletedNodeAccount(")
	firstCount := strings.Index(body, "outcome.Revoked++")
	if handOffAt < 0 || firstCount < handOffAt {
		t.Fatal("deleted-node hand-off must be confirmed before counting success")
	}
	helper := funcBlock(t, "provisioning.go", "func (s *Server) handOffDeletedNodeAccount(")
	recordAt := strings.Index(helper, "s.recordOrphanAccounts(")
	checkAt := strings.Index(helper, "node_accounts,'{}'::jsonb) ? $1")
	if recordAt < 0 || checkAt < 0 || recordAt > checkAt {
		t.Fatal("hand-off must record the orphan, then verify the mapping is gone")
	}
}

func TestStoreRevokeDoesNotReportSuccessOrDeleteMappingOnFailure(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) storeRevokeOrder(")
	checkAt := strings.Index(body, "if !outcome.OK()")
	statusAt := strings.Index(body, "http.StatusBadGateway")
	deletedAt := strings.Index(body, "SET deleted_at=now()")
	if checkAt < 0 || statusAt < 0 || deletedAt < 0 {
		t.Fatal("store revoke must check failures, return a non-200 status and defer deletion")
	}
	if !(checkAt < statusAt && checkAt < deletedAt) {
		t.Fatal("failure handling must occur before success response or deleted_at update")
	}
	if !strings.Contains(body, "\"ok\": false") || !strings.Contains(body, "\"failed\": outcome.Failed") {
		t.Fatal("partial revocation response must truthfully include failed node/user details")
	}
}

func TestExpiredPurgeRetainsRecordWhenNodeRevokeFails(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) purgeSubscriptionOutcome(")
	checkAt := strings.Index(body, "if !outcome.OK()")
	deleteAt := strings.Index(body, "DELETE FROM subscription_tokens")
	if checkAt < 0 || deleteAt < 0 || checkAt > deleteAt {
		t.Fatal("purge must keep subscription_tokens and node_accounts when any node revoke fails")
	}
	failureBlock := body[checkAt:deleteAt]
	if !strings.Contains(failureBlock, "return") || !strings.Contains(failureBlock, "retained row") {
		t.Fatal("failed revoke path must log and return before deleting the retry mapping")
	}
}
