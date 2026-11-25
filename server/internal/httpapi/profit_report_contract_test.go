package httpapi

import (
	"strings"
	"testing"
)

func TestProfitReportDoesNotReturnPartialDatabaseResults(t *testing.T) {
	body := funcBlock(t, "reports.go", "func (s *Server) profitReport(")
	for _, want := range []string{
		"if err := rows.Scan(&p.NodeID, &p.NodeName, &p.ServerID, &p.ServerName); err != nil",
		"if err := rows.Err(); err != nil",
		"if err := crow.Scan(&sid, &cost, &cur); err != nil",
		"if err := srow.Scan(&id, &monthly, &cur, &nodeIDs, &validNodeAccounts); err != nil",
		"if err := urow.Scan(&sid, &nid, &measured, &qualityOK, &collectedAt); err != nil",
		"if err := crow.Err(); err != nil",
		"if err := srow.Err(); err != nil",
		"if err := urow.Err(); err != nil",
		".Scan(&withRevenue); err != nil",
		".Scan(&allSubs); err != nil",
		"var validNodeAccounts bool",
		"jsonb_typeof(t.node_accounts) = 'object'",
		"if !validNodeAccounts",
		"node_accounts must be a JSON object",
		"if err := lrows.Scan(&sub.ID, &sub.Label, &sub.OrderID, &sub.Monthly, &sub.Currency, &sub.ExpiresAt); err != nil",
		"if err := lrows.Err(); err != nil",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profitReport must reject partial database results; missing %q", want)
		}
	}
}
