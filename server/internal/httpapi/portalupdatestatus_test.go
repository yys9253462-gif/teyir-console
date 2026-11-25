package httpapi

import (
	"os"
	"strings"
	"testing"
)

func TestPortalUpdateStatusDoesNotHideIncompleteNodeLists(t *testing.T) {
	src, err := os.ReadFile("portalupdatestatus.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyForTest(string(src), "func (s *Server) checkPortalUpdateStatus")
	for _, want := range []string{
		"if err := rows.Scan(&n.NodeID, &n.Name); err != nil",
		"if err := rows.Err(); err != nil",
		"node list read failed:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("portal update status must expose an incomplete node list; missing %q", want)
		}
	}
}

func TestComparePortalFiles(t *testing.T) {
	expected := map[string]string{}
	files := map[string]string{}
	for i, name := range portalBundleFiles {
		sha := string(rune('a'+i)) + "000000000000000000000000000000000000000"
		expected[name] = sha
		files[name] = sha
	}
	type bundle = struct {
		Files map[string]string `json:"files"`
	}

	if out, ok := comparePortalFiles(&bundle{Files: files}, expected); ok == nil || !*ok || len(out) != 0 {
		t.Errorf("identical files must be up to date, got ok=%v out=%v", ok, out)
	}

	changed := map[string]string{}
	for k, v := range files {
		changed[k] = v
	}
	changed["portal.py"] = "ffff"
	if out, ok := comparePortalFiles(&bundle{Files: changed}, expected); ok == nil || *ok || len(out) != 1 || out[0] != "portal.py" {
		t.Errorf("one changed file must be reported, got ok=%v out=%v", ok, out)
	}

	if out, ok := comparePortalFiles(nil, expected); ok == nil || *ok || len(out) != len(portalBundleFiles) {
		t.Errorf("old portal without fingerprints must need every file, got ok=%v out=%v", ok, out)
	}

	if _, ok := comparePortalFiles(&bundle{Files: files}, nil); ok != nil {
		t.Error("without GitHub fingerprints there must be no verdict")
	}
}
