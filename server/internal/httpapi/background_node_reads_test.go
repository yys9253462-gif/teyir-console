package httpapi

import (
	"strings"
	"testing"
)

func TestBackgroundNodeReadsAbortOnIncompleteRows(t *testing.T) {
	for _, tc := range []struct {
		file string
		sig  string
		scan string
	}{
		{"ippurity.go", "func (s *Server) BackfillIPNative(", "rows.Scan(&p.id, &p.ip); err != nil"},
		{"nodetraffic.go", "func (s *Server) refreshNodeVersions(", "rows.Scan(&r.id, &r.name); err != nil"},
	} {
		body := funcBlock(t, tc.file, tc.sig)
		for _, want := range []string{tc.scan, "rows.Err()", "return"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s must abort on incomplete node reads; missing %q", tc.sig, want)
			}
		}
	}
}
