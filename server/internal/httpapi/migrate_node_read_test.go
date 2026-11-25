package httpapi

import (
	"strings"
	"testing"
)

func TestMigrateNodeSurfacesCredentialLookupErrors(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) startMigrateNode(")
	query := strings.Index(body, "server_credentials WHERE server_id=$1::uuid")
	checked := strings.Index(body, ".Scan(&oldHasCred); err != nil")
	noCredential := strings.Index(body, "if !oldHasCred")
	if query < 0 || checked < 0 || noCredential < 0 || checked > noCredential {
		t.Fatal("migration must surface credential lookup errors before checking the no-credential case")
	}
	if !strings.Contains(body[checked:noCredential], "s.databaseError(w, r, err)") {
		t.Fatal("credential query failures must return as database errors, not be reported as missing credentials")
	}
}
