package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nodeops/server/internal/secrets"
)

func TestSubscriptionStateFailureMessageMatchesAction(t *testing.T) {
	out := subscriptionStateOutcome{Failed: map[string]string{"fixture": "unavailable"}}
	enabled := describeSubscriptionStateOutcome(out, true)
	if strings.Contains(enabled, "仍然可用") || strings.Contains(enabled, "改用删除") || !strings.Contains(enabled, "未确认恢复") {
		t.Fatal("enable failure gives misleading instructions:", enabled)
	}
	disabled := describeSubscriptionStateOutcome(out, false)
	if !strings.Contains(disabled, "尚未确认撤销") || !strings.Contains(disabled, "可能仍可用") {
		t.Fatal("disable failure must preserve uncertainty:", disabled)
	}
}

func TestSubscriptionStateRequiresConfirmedTarget(t *testing.T) {
	s := repairSubscriptionDB(t)
	ctx := context.WithValue(context.Background(), migrationRequestKey{}, true)
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	s.box = box
	cipher, err := box.Seal([]byte(testPortalAPIKey))
	if err != nil {
		t.Fatal(err)
	}
	var response string
	var code int
	var expectedActive bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users/set_status" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			UserID string `json:"user_id"`
			Active bool   `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.UserID != "fixture_user" || input.Active != expectedActive {
			t.Errorf("wrong request payload: %+v, %v", input, err)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(response))
	}))
	defer remote.Close()
	old := portalClient
	portalClient = remote.Client()
	t.Cleanup(func() { portalClient = old })
	repairExec(t, s, `INSERT INTO hysteria_nodes(id,name,api_url,api_key) VALUES($1,'fixture-node',$2,$3)`, repairFirst, remote.URL, cipher)
	rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{repairFirst: {UserID: "fixture_user"}}}
	cases := []struct {
		name, body      string
		code            int
		active, success bool
	}{
		{"disable confirmed", `{"ok":true,"status":"disabled"}`, 200, false, true},
		{"enable confirmed", `{"ok":true,"status":"active"}`, 200, true, true},
		{"disable wrong state", `{"ok":true,"status":"active"}`, 200, false, false},
		{"enable wrong state", `{"ok":true,"status":"disabled"}`, 200, true, false},
		{"missing state", `{"ok":true}`, 200, false, false},
		{"non-success transport", `{"ok":true,"status":"disabled"}`, 503, false, false},
		{"absent account cannot prove Xray revoked", `{"ok":false,"error":"User not found"}`, 404, false, false},
		{"absent enable account", `{"ok":false,"error":"User not found"}`, 404, true, false},
		{"old endpoint", `{"ok":false,"error":"API endpoint not found"}`, 404, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, code, expectedActive = tc.body, tc.code, tc.active
			out := s.applySubscriptionNodeStatus(ctx, rec, tc.active)
			if out.allOK() != tc.success {
				t.Fatalf("wrong success: %+v", out)
			}
			if tc.success && (len(out.Applied) != 1 || out.Applied[0] != "fixture-node") {
				t.Fatalf("missing applied node: %+v", out)
			}
			if !tc.success && out.Failed["fixture-node"] == "" {
				t.Fatalf("missing failure: %+v", out)
			}
		})
	}
}

func TestSubscriptionFailuresRetainReadableNodeName(t *testing.T) {
	s := repairSubscriptionDB(t)
	ctx := context.WithValue(context.Background(), migrationRequestKey{}, true)
	repairExec(t, s, `INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES($1,'已删除测试节点',now())`, repairFirst)
	rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{repairFirst: {UserID: "fixture_user"}}}
	for name, outcome := range map[string]subscriptionStateOutcome{
		"status":     s.applySubscriptionNodeStatus(ctx, rec, false),
		"attributes": s.syncSubscriptionNodeAttributes(ctx, rec),
	} {
		if outcome.Failed["已删除测试节点"] == "" || outcome.Failed[repairFirst] != "" {
			t.Fatalf("%s lost readable identity: %+v", name, outcome)
		}
	}
}

func TestPortalCallRejectsHTTPFailureDespiteOKEnvelope(t *testing.T) {
	for _, code := range []int{200, 201, 207, 400, 429, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer remote.Close()
			old := portalClient
			portalClient = remote.Client()
			t.Cleanup(func() { portalClient = old })
			_, err := portalCall(context.Background(), remote.URL, testPortalAPIKey, http.MethodGet, "/users/list", nil)
			if (err == nil) != (code >= 200 && code < 300) {
				t.Fatalf("status=%d error=%v", code, err)
			}
		})
	}
}
