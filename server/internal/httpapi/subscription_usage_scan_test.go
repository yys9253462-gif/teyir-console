package httpapi

import (
	"strings"
	"testing"
)

func TestSubscriptionUsageScanFailureDoesNotReturnPartialTotals(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) loadSubscriptionUsage(")
	scan := strings.Index(body, "rows.Scan(&subID, &nodeID, &userID, &used, &started, &quality, &reason, &sampled, &sources, &gaps, &currentOK, &online); err != nil")
	if scan < 0 {
		t.Fatal("missing measured-usage scan error check")
	}
	branchEnd := strings.Index(body[scan:], "\n\t\t}")
	if branchEnd < 0 {
		t.Fatal("missing measured-usage scan failure branch")
	}
	branch := body[scan : scan+branchEnd]
	for _, want := range []string{"rows.Close()", "真实计量读取不完整", "return out"} {
		if !strings.Contains(branch, want) {
			t.Errorf("scan failure must invalidate all partial aggregates; missing %q", want)
		}
	}
	if !strings.Contains(body, "measured usage rows ended incompletely") {
		t.Fatal("iteration failure must not continue into legacy aggregation")
	}
}
