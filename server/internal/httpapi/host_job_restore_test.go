package httpapi

import (
	"strings"
	"testing"
)

func TestHostJobRestoreDoesNotSkipUnreadableJournalRows(t *testing.T) {
	body := funcBlock(t, "host_job_journal.go", "func (st *installJobStore) restoreLocked(")
	for _, want := range []string{
		"if err := rows.Scan(&raw); err != nil",
		"if err := json.Unmarshal(raw, &job); err != nil",
		"decode host operation job:",
		"if err := rows.Err(); err != nil",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("restoreLocked must fail closed on unreadable journal rows; missing %q", want)
		}
	}
}
