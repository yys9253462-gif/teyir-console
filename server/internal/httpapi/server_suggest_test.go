package httpapi

import (
	"strings"
	"testing"
)

func TestServerNameSuggestionRequiresCompleteExistingNameList(t *testing.T) {
	body := funcBlock(t, "server_suggest.go", "func (s *Server) suggestServerName(")
	for _, want := range []string{
		"rows.Scan(&n); err != nil",
		"rows.Err(); err != nil",
		"nextNumberedNodeName(base, existing)",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("server-name suggestion must reject an incomplete existing-name list; missing %q", want)
		}
	}
}
