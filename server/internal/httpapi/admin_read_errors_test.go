package httpapi

import (
	"strings"
	"testing"
)

func TestRetryTaskDistinguishesMissingTaskFromDatabaseFailure(t *testing.T) {
	body := funcBlock(t, "admin.go", "func (s *Server) retryTask(")
	for _, want := range []string{
		"errors.Is(err, pgx.ErrNoRows)",
		"http.StatusNotFound",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("retry task must distinguish a missing task from a database outage; missing %q", want)
		}
	}
}
