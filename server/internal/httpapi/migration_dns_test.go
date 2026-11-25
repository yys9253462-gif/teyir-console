package httpapi

import (
	"strings"
	"testing"
)

func TestMigrationRollbackDoesNotUseMissingOldHostForDNS(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) runMigrateNode(")
	checks := []string{
		"net.ParseIP(oldHost) == nil",
		"if cutover && in.AutoDNS",
		"DNS未回退",
		"请立即手工将 %s 恢复到旧节点 IP %s",
	}
	for _, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("migration rollback must not attempt DNS update with unknown old host; missing %q", want)
		}
	}
	if !strings.Contains(body, "UpsertA(rollbackCtx, domain, oldHost)") {
		t.Fatal("Cloudflare rollback must use the prevalidated old-host snapshot")
	}
}

func TestMigrationTargetLookupDistinguishesDatabaseFailure(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) startMigrateNode(")
	if !strings.Contains(body, "errors.Is(err, pgx.ErrNoRows)") ||
		!strings.Contains(body, "s.databaseError(w, r, err)") {
		t.Fatal("migration target lookup must distinguish a missing host from a database failure")
	}
}
