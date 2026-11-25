package httpapi

import (
	"strings"
	"testing"
)

func TestBlockCheckListDoesNotHideProbeConfigurationReadFailures(t *testing.T) {
	body := funcBlock(t, "blockcheck.go", "func (s *Server) listBlockChecks(")
	for _, want := range []string{
		"id, err := s.cnProbeServerID(r.Context())",
		"if err != nil {\n\t\ts.databaseError(w, r, err)",
		"case errors.Is(err, pgx.ErrNoRows):\n\t\t\tprobeMissing = true",
		`"probe_missing": probeMissing`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block-check list must not hide invalid probe configuration; missing %q", want)
		}
	}
}

func TestBlockCheckListDoesNotReturnPartialNodeRows(t *testing.T) {
	body := funcBlock(t, "blockcheck.go", "func (s *Server) listBlockChecks(")
	errCheck := strings.Index(body, "if err := rows.Err(); err != nil")
	response := strings.Index(body, "writeJSON(w, http.StatusOK")
	if errCheck < 0 || response < 0 || errCheck > response {
		t.Fatal("block-check list must report iteration errors instead of returning partial results")
	}
}

func TestBlockCheckDoesNotReportPartialTargetLists(t *testing.T) {
	body := funcBlock(t, "blockcheck.go", "func (s *Server) runBlockChecks(")
	for _, want := range []string{
		"probeID, err := s.cnProbeServerID(ctx)",
		"读取国内探测点配置失败",
		"rows.Scan(&t.nodeID, &t.ip, &apiURL); err != nil",
		"rows.Err(); err != nil",
		"return 0, err",
		"if net.ParseIP(t.ip) == nil",
		"url.Parse(apiURL)",
		"u.Scheme == \"\" || u.Hostname() == \"\"",
		"的节点 API 地址格式不对",
		"的节点 API 端口不合法",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("block-check target list must distinguish bad data from read failure; missing %q", want)
		}
	}
}
