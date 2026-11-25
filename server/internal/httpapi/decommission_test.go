package httpapi

import (
	"os"
	"strings"
	"testing"
)

// 勾选「已永久停用」时：先登记（留痕）再结案；不勾选时行为不变（仍返回 207）。
func TestDeleteNodeDecommissionedResolvesAfterRecording(t *testing.T) {
	body := funcBlock(t, "manage.go", "func (s *Server) deleteNode(")
	recordAt := strings.Index(body, "s.recordOrphanAccounts(")
	resolveAt := strings.Index(body, "节点已永久停用，删除时直接结案")
	if recordAt < 0 || resolveAt < 0 || recordAt > resolveAt {
		t.Fatal("decommissioned delete must record orphans before resolving them")
	}
	if !strings.Contains(body, `r.URL.Query().Get("decommissioned") == "1"`) {
		t.Fatal("decommission must be an explicit opt-in flag")
	}
	if !strings.Contains(body, "deleted_with_revoke_failures") || !strings.Contains(body, "http.StatusMultiStatus") {
		t.Fatal("without the flag, unrevoked accounts must still surface as 207")
	}
}

func TestResolveOrphanIsGuarded(t *testing.T) {
	body := funcBlock(t, "orphan.go", "func (s *Server) resolveOrphan(")
	for _, want := range []string{`X-Confirm-Dangerous`, "resolved_at IS NULL", `s.audit(`} {
		if !strings.Contains(body, want) {
			t.Errorf("resolveOrphan missing %q", want)
		}
	}
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"POST /api/orphans/{id}/resolve", s.authenticated(s.requireAdmin(s.resolveOrphan))`) {
		t.Fatal("resolve route must be admin-only")
	}
}
