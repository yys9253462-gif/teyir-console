package httpapi

import (
	"strings"
	"testing"
)

func TestHostJobListDoesNotHideIncompleteRows(t *testing.T) {
	body := funcBlock(t, "host_job_journal.go", "func (s *Server) listInstallJobs(")
	for _, want := range []string{
		"if err := rows.Scan(&raw); err != nil",
		"if err := rows.Err(); err != nil",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listInstallJobs must report incomplete task data; missing %q", want)
		}
	}
}
