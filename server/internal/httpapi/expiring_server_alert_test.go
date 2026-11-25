package httpapi

import (
	"strings"
	"testing"
)

func TestExpiringServerAlertReadErrorsAbortReconciliation(t *testing.T) {
	helper := funcBlock(t, "hostextras.go", "func (s *Server) expiringServerConditions(")
	for _, want := range []string{
		"func (s *Server) expiringServerConditions(ctx context.Context) ([]alertCondition, error)",
		"if err := rows.Scan(&id, &days); err != nil",
		"if err := rows.Err(); err != nil",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("expiring server alerts must propagate incomplete reads; missing %q", want)
		}
	}
	reconcile := funcBlock(t, "manage.go", "func (s *Server) ReconcileAlerts(")
	if !strings.Contains(reconcile, "expiring, err := s.expiringServerConditions(ctx)") || !strings.Contains(reconcile, "conditions = append(conditions, expiring...)") {
		t.Fatal("alert reconciliation must add expiry conditions only after a successful read")
	}
}
