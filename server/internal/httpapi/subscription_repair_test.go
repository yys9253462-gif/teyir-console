package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nodeops/server/internal/auth"
	"nodeops/server/internal/secrets"
)

func TestSubscriptionRepairFailureIdentity(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		results := []subscriptionStatusResult{
			{nodeID: "gone", name: "same", err: "deleted", gone: true},
			{nodeID: "live", name: "same", err: "unavailable"},
			{nodeID: "other", name: "same [live]", err: "unavailable"},
		}
		if reverse {
			results[0], results[2] = results[2], results[0]
		}
		out := collectSubscriptionStatusResults(results)
		if len(out.Failed) != 3 || out.onlyGone() {
			t.Fatalf("lost a live failure: %+v", out)
		}
	}
	out := collectSubscriptionStatusResults([]subscriptionStatusResult{{nodeID: "1", name: "same", err: "deleted", gone: true}, {nodeID: "2", name: "same", err: "deleted", gone: true}})
	if len(out.Failed) != 2 || !out.onlyGone() {
		t.Fatalf("gone-only classification: %+v", out)
	}
	if (subscriptionStateOutcome{Failed: map[string]string{"same": "unknown"}, Gone: []string{"same"}}).onlyGone() {
		t.Fatal("display counts cannot prove deletion")
	}
}

func TestSubscriptionRepairCompensationBudget(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, done := subscriptionCompensationContext(parent)
	defer done()
	if ctx.Err() != nil {
		t.Fatal("request cancellation discarded compensation")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		t.Fatal("missing independent compensation deadline")
	}
	done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("cancel must release budget")
	}
}

func TestSubscriptionRepairColdBuildCoalesces(t *testing.T) {
	cache := &subscriptionCache{}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	build := &subscriptionBuild{Nodes: []subscriptionNode{{ID: "verified"}}}
	resolve := func(ctx context.Context) *subscriptionBuild {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-ctx.Done():
			t.Error("shared build canceled")
		}
		return build
	}
	owner, cancel := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() { _, err := cache.load(owner, "key", resolve); ownerDone <- err }()
	<-entered
	cancel()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter did not cancel: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.load(context.Background(), "key", resolve)
			if err != nil || got != build {
				t.Errorf("shared result %p %v", got, err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected one shared build, got %d", calls.Load())
	}
}

func TestSubscriptionRepairInvalidationAndStale(t *testing.T) {
	cache := &subscriptionCache{}
	entered, release := make(chan struct{}), make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = cache.load(context.Background(), "key", func(context.Context) *subscriptionBuild {
			close(entered)
			<-release
			return &subscriptionBuild{Nodes: []subscriptionNode{{ID: "old"}}}
		})
	}()
	<-entered
	cache.mu.Lock()
	cache.gen++
	cache.build = nil
	cache.key = ""
	cache.mu.Unlock()
	fresh := &subscriptionBuild{Nodes: []subscriptionNode{{ID: "fresh"}}}
	if _, err := cache.load(context.Background(), "key", func(context.Context) *subscriptionBuild { return fresh }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-oldDone
	cache.mu.Lock()
	if cache.build != fresh {
		t.Error("old generation overwrote fresh cache")
	}
	cache.storedAt = time.Now().Add(-2 * time.Minute)
	cache.mu.Unlock()
	refreshStarted, finish := make(chan struct{}), make(chan struct{})
	got, err := cache.load(context.Background(), "key", func(context.Context) *subscriptionBuild { close(refreshStarted); <-finish; return fresh })
	if err != nil || got != fresh {
		t.Fatal("stale cache must be served immediately")
	}
	<-refreshStarted
	cache.mu.Lock()
	var done chan struct{}
	for _, f := range cache.inflight {
		done = f.done
	}
	cache.mu.Unlock()
	close(finish)
	<-done
}

func TestSubscriptionRepairBuildFailureVersusEmptyScope(t *testing.T) {
	s := fixServer(nil)
	rec := &subscriptionRecord{NodeIDs: []string{"wanted"}, nodeAccounts: map[string]subscriptionNodeAccount{"wanted": {UserID: "u", Password: "p"}, "outside": {UserID: "u", Password: "p"}}}
	for _, tc := range []struct {
		name, id string
		wantErr  bool
	}{
		{"failed candidate", "wanted", true}, {"out of scope", "outside", false}, {"genuinely empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := &subscriptionBuild{}
			if tc.id != "" {
				build.Excluded = []subscriptionExcluded{{NodeID: tc.id, Reason: "probe unavailable"}}
			}
			_, err := s.subscriptionNodesForChecked(context.Background(), build.Nodes, rec, build.Excluded)
			if errors.Is(err, errSubscriptionTemporarilyEmpty) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// Uses only the existing disposable-schema fixture. No production DSN is read.
func TestSubscriptionRepairMergedPatchLockAndRetry(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	for _, sql := range []string{
		`ALTER TABLE subscription_tokens ADD COLUMN IF NOT EXISTS is_panel_merged boolean DEFAULT false`,
		`UPDATE subscription_tokens SET is_panel_merged=true,order_id=NULL,expires_at=NULL,node_accounts='{"00000000-0000-0000-0000-000000000102":{"user_id":"fixture"}}'`,
		`CREATE TABLE subscription_pending_node_actions(subscription_id uuid PRIMARY KEY,action text,then_delete boolean,next_attempt_at timestamptz,last_error text,register_revoked_order boolean)`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	// A node mapping with no portal binding creates a deterministic live failure.
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name) VALUES($1::uuid,'live')`, fixNode); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	requestCtx := ctx
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/subscription", strings.NewReader(body))
		r = r.WithContext(context.WithValue(requestCtx, claimsKey, auth.Claims{Subject: fixHost, Role: "admin"}))
		w := httptest.NewRecorder()
		s.updateSubscription(w, r)
		return w
	}
	_, release, err := s.lockSubscription(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	w := patch(`{"enabled":false}`)
	release()
	if w.Code != http.StatusConflict {
		t.Fatalf("mutation bypassed lock: %d %s", w.Code, w.Body.String())
	}
	for i := 0; i < 2; i++ {
		w = patch(`{"enabled":false}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"node_state_ok":false`) {
			t.Fatalf("disable retry: %d %s", w.Code, w.Body.String())
		}
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, fixSub).Scan(&count); err != nil || count != 1 {
		t.Fatalf("disable compensation: %d %v", count, err)
	}
	w = patch(`{"label":"preserved"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "node_state_ok") {
		t.Fatalf("label-only action: %s", w.Body.String())
	}
	var enabled bool
	var label string
	if err := db.QueryRow(ctx, `SELECT enabled,label FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&enabled, &label); err != nil || enabled || label != "preserved" {
		t.Fatalf("field overwrite: %v %q %v", enabled, label, err)
	}
	for i := 0; i < 2; i++ {
		w = patch(`{"enabled":true}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"node_state_ok":false`) {
			t.Fatalf("enable retry: %d %s", w.Code, w.Body.String())
		}
	}
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("enable compensation: %q %v", status, err)
	}
	// Cancel the actual handler request inside the node call, after enabled=false
	// has committed. The only durable retry must survive that cancellation.
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if err != nil {
		t.Fatal(err)
	}
	s.box = box
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET api_url='https://8.8.8.8:8443',api_key=$1 WHERE id=$2::uuid`, key, fixNode); err != nil {
		t.Fatal(err)
	}
	var cancelRequest context.CancelFunc
	requestCtx, cancelRequest = context.WithCancel(ctx)
	defer cancelRequest()
	oldClient := portalClient
	portalClient = &http.Client{Transport: revokeTestTransport{onDelete: cancelRequest, status: 503, body: `{"ok":false,"error":"unavailable"}`}}
	defer func() { portalClient = oldClient }()
	w = patch(`{"enabled":false}`)
	if requestCtx.Err() == nil || w.Code != 200 || !strings.Contains(w.Body.String(), `"node_state_ok":false`) {
		t.Fatalf("request was not canceled inside node action: %d %s err=%v", w.Code, w.Body.String(), requestCtx.Err())
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid AND action='disable'`, fixSub).Scan(&count); err != nil || count != 1 {
		t.Fatalf("canceled handler lost disable compensation: %d %v", count, err)
	}

}

func TestSubscriptionRepairQueueWriteHasIndependentTimeout(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `LOCK TABLE subscription_sync_jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = fixServer(db).queueSubscriptionSyncBounded(parent, fixSub)
	elapsed := time.Since(started)
	if err == nil || elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("independent bounded write: elapsed=%s err=%v", elapsed, err)
	}
}
