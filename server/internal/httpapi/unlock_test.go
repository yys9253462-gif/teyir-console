package httpapi

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParseUnlockResponseNarrowsFields(t *testing.T) {
	raw := []byte(`{"ok":true,"results":[
		{"key":"netflix","name":"Netflix","status":"partial","region":"sg","note":"仅自制剧","extra":"x"},
		{"key":"chatgpt","name":"ChatGPT","status":"weird","region":"","note":""},
		{"key":"","name":"bad","status":"yes"}]}`)
	got, err := parseUnlockResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 results (empty key dropped), got %d", len(got))
	}
	if got[0].Region != "SG" || got[0].Status != "partial" {
		t.Fatalf("netflix parsed wrong: %+v", got[0])
	}
	if got[1].Status != "unknown" {
		t.Fatalf("unknown status must fall back to unknown, got %q", got[1].Status)
	}
}

func TestParseUnlockResponseRejectsEmpty(t *testing.T) {
	if _, err := parseUnlockResponse([]byte(`{"ok":true,"results":[]}`)); err == nil {
		t.Fatal("empty results must be an error, not a silent 'nothing unlocked'")
	}
}

func TestStripURLErrorHidesSecretPath(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://node:11690/secret-token-123/check-version", Err: context.DeadlineExceeded}
	got := stripURLError(err).Error()
	if strings.Contains(got, "secret-token-123") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !errors.Is(stripURLError(err), context.DeadlineExceeded) {
		t.Fatal("underlying cause must be preserved")
	}
}
