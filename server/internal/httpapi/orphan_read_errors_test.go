package httpapi

import (
	"strings"
	"testing"
)

func TestRetryOrphanDistinguishesDatabaseFailureFromMissingRecord(t *testing.T) {
	body := funcBlock(t, "orphan.go", "func (s *Server) retryOrphan(")
	for _, want := range []string{
		"if !errors.Is(err, pgx.ErrNoRows)",
		"if checkErr != nil && !errors.Is(checkErr, pgx.ErrNoRows)",
		"s.databaseError(w, r, checkErr)",
		"http.StatusNotFound",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("orphan retry must not disguise database outages as missing records; missing %q", want)
		}
	}
}

func TestResolveOrphanDistinguishesDatabaseFailureFromMissingRecord(t *testing.T) {
	body := funcBlock(t, "orphan.go", "func (s *Server) resolveOrphan(")
	if !strings.Contains(body, "errors.Is(err, pgx.ErrNoRows)") ||
		!strings.Contains(body, "s.databaseError(w, r, err)") {
		t.Fatal("orphan resolve must distinguish a missing row from a failed database update")
	}
}
