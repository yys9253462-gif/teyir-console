package httpapi

import (
	"strings"
	"testing"
)

func TestMetricsTrendDoesNotReturnPartialSeries(t *testing.T) {
	body := funcBlock(t, "hostextras.go", "func (s *Server) serverMetricsTrend(")
	scan := strings.Index(body, "rows.Scan(&p.T, &p.CPU, &p.Mem, &p.Disk)")
	iteration := strings.Index(body, "if err := rows.Err(); err != nil")
	response := strings.Index(body, `writeJSON(w, http.StatusOK, map[string]any{"days": days, "points": out})`)
	if scan < 0 || iteration < scan || response < iteration {
		t.Fatal("metrics trend must check rows.Err before returning points")
	}
}
