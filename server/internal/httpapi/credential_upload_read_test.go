package httpapi

import (
	"strings"
	"testing"
)

func TestCredentialUploadDistinguishesMissingHostFromDatabaseFailure(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) uploadCredential(")
	for _, want := range []string{
		"errors.Is(err, pgx.ErrNoRows)",
		"http.StatusNotFound",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("credential upload must not report database outages as a missing host; missing %q", want)
		}
	}
}
