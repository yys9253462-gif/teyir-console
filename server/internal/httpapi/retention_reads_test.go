package httpapi

import (
	"strings"
	"testing"
)

func TestRetentionFootprintDoesNotSkipFailedRows(t *testing.T) {
	body := funcBlock(t, "retention.go", "func (s *Server) logRetentionFootprint(")
	if !strings.Contains(body, "if err := rows.Scan(&name, &bytes, &live, &dead); err != nil") ||
		!strings.Contains(body, "footprint row scan failed") ||
		!strings.Contains(body, "if err := rows.Err(); err != nil") {
		t.Fatal("retention footprint must report incomplete table statistics instead of silently omitting rows")
	}
}
