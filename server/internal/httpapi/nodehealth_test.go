package httpapi

import (
	"strings"
	"testing"
	"time"
)

func TestNodeHealth(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-19 * time.Hour)

	if ok, _ := nodeHealth("online", "", &fresh, now); !ok {
		t.Error("recently checked online node must be healthy")
	}
	if ok, reason := nodeHealth("online", "", &stale, now); ok || !strings.Contains(reason, "19 小时") {
		t.Errorf("stale online node must be flagged with its age, got ok=%v reason=%q", ok, reason)
	}
	if ok, _ := nodeHealth("online", "", nil, now); ok {
		t.Error("never-checked node must not be healthy")
	}
	if ok, reason := nodeHealth("offline", "dial tcp: i/o timeout", &fresh, now); ok || !strings.Contains(reason, "i/o timeout") {
		t.Errorf("offline node must surface status_error, got ok=%v reason=%q", ok, reason)
	}
	if ok, reason := nodeHealth("warning", "", &fresh, now); ok || reason != "不稳定" {
		t.Errorf("warning without error text must still explain itself, got %q", reason)
	}
}
