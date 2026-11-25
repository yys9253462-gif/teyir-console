package httpapi

import (
	"strings"
	"testing"
)

func TestMatchServerReadErrorsArePropagated(t *testing.T) {
	helper := funcBlock(t, "nodeapi.go", "func (s *Server) matchServer(")
	for _, want := range []string{"return \"\", \"\", err", "rows.Scan(&id, &name, &host); err != nil", "rows.Err()"} {
		if !strings.Contains(helper, want) {
			t.Errorf("matchServer must propagate incomplete database reads; missing %q", want)
		}
	}
	for _, sig := range []string{
		"func (s *Server) ensureHostForImportedNode(",
		"func (s *Server) previewNodeAPI(",
	} {
		body := funcBlock(t, "nodeapi.go", sig)
		if !strings.Contains(body, "s.matchServer(") || !strings.Contains(body, "查询已有主机失败") && !strings.Contains(body, "databaseError") {
			t.Errorf("%s must not treat a lookup failure as no existing host", sig)
		}
	}
}
