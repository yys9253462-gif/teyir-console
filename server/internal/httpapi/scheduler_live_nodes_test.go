package httpapi

import (
	"strings"
	"testing"
)

func TestScheduledFleetChecksAbortOnIncompleteNodeList(t *testing.T) {
	helper := funcBlock(t, "scheduler.go", "func (s *Server) liveNodeIDs(")
	for _, want := range []string{
		"(map[string]string, error)",
		"if err := rows.Scan(&id, &name); err != nil",
		"if err := rows.Err(); err != nil",
		"return nil, err",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("liveNodeIDs must reject partial fleet data; missing %q", want)
		}
	}
	for _, sig := range []string{
		"func (s *Server) scheduledUnlock(",
		"func (s *Server) scheduledPurity(",
	} {
		body := funcBlock(t, "scheduler.go", sig)
		if !strings.Contains(body, "nodes, err := s.liveNodeIDs(ctx)") || !strings.Contains(body, "if err != nil {") {
			t.Errorf("%s must abort after a failed fleet read", sig)
		}
	}
}
