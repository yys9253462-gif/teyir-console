package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func expectedBatchFiles() map[string]string {
	out := map[string]string{}
	for _, name := range portalBundleFiles {
		out[name] = strings.Repeat("a", 40)
	}
	return out
}
func TestPortalBatchIDs(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	ids, err := validatePortalBatchIDs([]string{id, id})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	for _, input := range [][]string{nil, {"bad"}, make([]string, 21)} {
		if _, err := validatePortalBatchIDs(input); err == nil {
			t.Fatalf("accepted %v", input)
		}
	}
}
func TestVerifyPortalBundleRequiresEveryFile(t *testing.T) {
	expected := expectedBatchFiles()
	b := portalBundle{Files: expected, MeterInitialized: true, RuntimeRevision: "2026-10-12-real-usage-v2"}
	if err := verifyPortalBundle(b, expected); err != nil {
		t.Fatal(err)
	}
	for _, file := range portalBundleFiles {
		copyFiles := expectedBatchFiles()
		delete(copyFiles, file)
		b.Files = copyFiles
		if verifyPortalBundle(b, expected) == nil {
			t.Fatal("missing", file)
		}
	}
	b.Files = expected
	b.RuntimeRevision = "old"
	if verifyPortalBundle(b, expected) == nil {
		t.Fatal("accepted old running module")
	}
}
func TestPortalBatchTwoRoundsAndVerification(t *testing.T) {
	expected := expectedBatchFiles()
	posts, gets := 0, 0
	call := func(ctx context.Context, method, path string, form map[string]string) (json.RawMessage, error) {
		if method == http.MethodPost {
			posts++
			if path != "do-upgrade" || form["target"] != "portal" {
				t.Fatal("wrong update target")
			}
			return json.RawMessage(`{"ok":true}`), nil
		}
		gets++
		files := expectedBatchFiles()
		if posts == 1 {
			files = map[string]string{"portal.py": expected["portal.py"]}
		}
		b := portalBundle{Files: files, MeterInitialized: true, RuntimeRevision: "2026-10-12-real-usage-v2"}
		b.Measurement.OK = true
		raw, _ := json.Marshal(map[string]any{"ok": true, "portal_bundle": b})
		return raw, nil
	}
	item := portalBatchItem{}
	err := performPortalUpdate(context.Background(), &item, expected, func(string) error { return nil }, call, func(context.Context, time.Duration) error { return nil })
	if err != nil || posts != 2 || gets != 2 || item.Error != "" {
		t.Fatal(err, posts, gets, item.Error)
	}
}
func TestPortalBatchAcceptedDoesNotMeanSuccess(t *testing.T) {
	posts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := func(context.Context, string, string, map[string]string) (json.RawMessage, error) {
		posts++
		return json.RawMessage(`{"ok":true}`), nil
	}
	err := performPortalUpdate(ctx, &portalBatchItem{}, expectedBatchFiles(), func(string) error { return nil }, call, func(context.Context, time.Duration) error { cancel(); return ctx.Err() })
	if err == nil || posts != 1 {
		t.Fatal("accepted falsely as success", err, posts)
	}
}
func TestPortalBatchWaitHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(waitPortalBatch(ctx, time.Hour), context.Canceled) {
		t.Fatal("did not cancel")
	}
}
func TestPortalBatchRejectedUpdateStops(t *testing.T) {
	posts := 0
	call := func(context.Context, string, string, map[string]string) (json.RawMessage, error) {
		posts++
		return json.RawMessage(`{"ok":false}`), nil
	}
	err := performPortalUpdate(context.Background(), &portalBatchItem{}, expectedBatchFiles(), func(string) error { return nil }, call, func(context.Context, time.Duration) error { return nil })
	if err == nil || posts != 1 {
		t.Fatal(err, posts)
	}
}
func TestPortalBatchMeasurementWarning(t *testing.T) {
	b := portalBundle{Files: expectedBatchFiles()}
	call := func(ctx context.Context, method, path string, form map[string]string) (json.RawMessage, error) {
		if method == http.MethodPost {
			return json.RawMessage(`{"ok":true}`), nil
		}
		raw, _ := json.Marshal(map[string]any{"ok": true, "portal_bundle": b})
		return raw, nil
	}
	item := portalBatchItem{}
	err := performPortalUpdate(context.Background(), &item, expectedBatchFiles(), func(string) error { return nil }, call, func(context.Context, time.Duration) error { return nil })
	if err != nil || item.Error == "" {
		t.Fatal("missing measurement warning", err)
	}
}
