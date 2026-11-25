package httpapi

import (
	"strings"
	"testing"
)

func TestAlertReconcileDoesNotResolveWhenRevokeEvidenceReadFails(t *testing.T) {
	body := funcBlock(t, "manage.go", "func (s *Server) ReconcileAlerts(")
	for _, want := range []string{
		"if xerr != nil {\n\t\treturn xerr",
		"if err := exhaustedRows.Scan(&sID, &nID, &attempts); err != nil",
		"if err := exhaustedRows.Err(); err != nil",
		"if err := lingerRows.Scan(&sID, &nID); err != nil",
		"if err := lingerRows.Err(); err != nil",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("alert reconciliation must abort on incomplete revoke evidence; missing %q", want)
		}
	}
}
