package httpapi

import (
	"strings"
	"testing"
)

func TestCategoryScopeRejectsIncompleteMembershipReads(t *testing.T) {
	body := funcBlock(t, "provisioning.go", "func (s *Server) filterProvisionCandidates(")
	for _, want := range []string{
		"rows.Scan(&id); err != nil",
		"return nil, err",
		"rows.Err()",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("category provisioning must reject incomplete membership data; missing %q", want)
		}
	}
	if !strings.Contains(body, "if err = rows.Err(); err != nil") {
		t.Fatal("category membership iteration errors must be returned before candidate filtering")
	}
}
