package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFixSignatureV2BindsQueryAndNonce(t *testing.T) {
	now := time.Now()
	ts := fmt.Sprint(now.Unix())
	secret := "fixture-v2-secret"
	makeR := func(path, nonce string) *http.Request {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Store-Timestamp", ts)
		r.Header.Set("X-Store-Signature-Version", "2")
		r.Header.Set("X-Store-Nonce", nonce)
		r.Header.Set("X-Store-Signature", storeSign(secret, storeSignatureMessageV2(ts, "GET", r.URL.Path, r.URL.Query().Encode(), nonce, nil)))
		return r
	}
	a := makeR("/api/store/orders?order_id=a", strings.Repeat("a", 32))
	b := makeR("/api/store/orders?order_id=b", strings.Repeat("b", 32))
	if a.Header.Get("X-Store-Signature") == b.Header.Get("X-Store-Signature") {
		t.Fatal("same-second signatures collide")
	}
	if _, err := verifyStoreSignature(a, secret, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyStoreSignature(b, secret, now); err != nil {
		t.Fatal(err)
	}
	changed := makeR("/api/store/orders?order_id=original", strings.Repeat("c", 32))
	changed.URL.RawQuery = "order_id=changed"
	if _, err := verifyStoreSignature(changed, secret, now); err == nil {
		t.Fatal("query tampering accepted")
	}
	if _, err := verifyStoreSignature(a, secret, now); err == nil {
		t.Fatal("nonce replay accepted")
	}
}
func TestFixTelegramAcceptedAndMoney(t *testing.T) {
	result := telegramFriendlyResult(telegramAction{}, 202, []byte(`{"job_id":"fixture"}`))
	if strings.Contains(result, "已完成") || !strings.Contains(result, "受理") {
		t.Fatal(result)
	}
	result = telegramFriendlyResult(telegramAction{}, 200, []byte(`{"nodes":[{"node_name":"东京","revenue":{"USD":31.23},"cost":{"USD":10.11},"profit":{"USD":21.12}}]}`))
	for _, v := range []string{"31.23", "10.11", "21.12", "USD"} {
		if !strings.Contains(result, v) {
			t.Fatal("missing money", v, result)
		}
	}
	result = telegramFriendlyResult(telegramAction{}, 200, []byte(`{"checked":false,"supported":false}`))
	if strings.Contains(result, "已完成") {
		t.Fatal(result)
	}
}
func TestFixTelegramUnlockScoped(t *testing.T) {
	raw := []byte(`{"items":[{"node_id":"a","results":[{"name":"Netflix","status":"yes"}]},{"node_id":"b","results":[{"name":"OtherProvider","status":"no"}]}]}`)
	out := telegramFriendlyResult(telegramAction{Path: "/api/unlock?node_id=a"}, 200, raw)
	if !strings.Contains(out, "Netflix") || strings.Contains(out, "OtherProvider") {
		t.Fatal(out)
	}
}
func TestFixWizardCatalogCoversRegisteredRoutes(t *testing.T) {
	raw, err := os.ReadFile("telegram_forms.json")
	if err != nil {
		t.Fatal(err)
	}
	var forms map[string]telegramForm
	if json.Unmarshal(raw, &forms) != nil {
		t.Fatal("invalid catalog")
	}
	if len(forms) < 130 {
		t.Fatal("incomplete catalog")
	}
	form := forms["POST /api/subscriptions"]
	if len(form.Fields) == 0 {
		t.Fatal("subscription form missing")
	}
	body := map[string]any{}
	setTelegramField(body, "credential.secret", "fixture-value")
	if body["credential"].(map[string]any)["secret"] != "fixture-value" {
		t.Fatal(body)
	}
}
func TestFixMigrationEndpointAlwaysFollowsBusinessDomain(t *testing.T) {
	for _, base := range []string{"https://1.2.3.4:8443", "https://old-api.test:8443"} {
		got, err := migrationEndpoint(base, "5.6.7.8", "node.test")
		if err != nil || got != "https://node.test:8443" {
			t.Fatal(got, err)
		}
	}
}
func TestFixMigrationPreparationBeforeCutover(t *testing.T) {
	raw, err := os.ReadFile("migratenode.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	ready := strings.Index(s, "meta, err := fetchPortalMeta(readyCtx")
	cut := strings.Index(s, "UpsertARecords(ctx, domain, newHost)")
	if ready < 0 || cut < ready {
		t.Fatal("DNS cut before readiness")
	}
	if !strings.Contains(s, "rollback()") || !strings.Contains(s, "committed = true") {
		t.Fatal("missing rollback")
	}
}
func TestFixScheduleFailureTracking(t *testing.T) {
	state := &scheduleRunState{}
	ctx := context.WithValue(context.Background(), scheduleRunKey{}, state)
	scheduleFailed(ctx)
	if state.Failed != 1 {
		t.Fatal(state)
	}
}
func TestFixAccountReadbackFailsClosed(t *testing.T) {
	if verifyAccountExpiry([]byte(`{"user":{"expires_at":100}}`), 200) == nil {
		t.Fatal("wrong expiry accepted")
	}
	if verifyAccountExpiry([]byte(`{"user":{"expires_at":200}}`), 200) != nil {
		t.Fatal("correct expiry rejected")
	}
}
