package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"nodeops/server/internal/auth"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramPrivateOwner(t *testing.T) {
	for _, v := range []struct {
		cfg, chat, sender string
		ok                bool
	}{
		{"123456", "123456", "123456", true}, {"123456", "999999", "999999", false},
		{"-123456", "-123456", "123456", false}, {"123456", "123456", "999999", false}, {"", "123456", "123456", false},
	} {
		if got := telegramPrivateOwner(v.cfg, v.chat, v.sender); got != v.ok {
			t.Fatalf("identity %+v got %v", v, got)
		}
	}
}
func TestTelegramParseAPI(t *testing.T) {
	for _, text := range []string{`/api POST /api/subscriptions {"label":"带 空格", "days":30}`, `/api@my_bot GET /api/nodes?limit=5`, `/api DELETE /api/nodes/uuid`} {
		a, err := parseTelegramAPI(text)
		if err != nil {
			t.Fatal(err)
		}
		if a.Path == "" {
			t.Fatal("empty path")
		}
		if len(a.Body) > 0 && !json.Valid(a.Body) {
			t.Fatal("lost JSON")
		}
	}
	for _, text := range []string{"/api", "/api GET https://evil.test/api/nodes", "/api TRACE /api/nodes", "/api POST /api/nodes not-json", "/api GET //evil.test/api/nodes", "/api GET /api/nodes/{id}", "/api GET /api/nodes\\bad"} {
		if _, err := parseTelegramAPI(text); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
func TestTelegramPathParameters(t *testing.T) {
	p, err := telegramFillPath("/api/nodes/{id}/users/{uid}", map[string]string{"id": "n1", "uid": "u1"})
	if err != nil || p != "/api/nodes/n1/users/u1" {
		t.Fatal(p, err)
	}
	for _, v := range []string{"", "../bad", "x?admin=1", "x#fragment", "..", "x\\y"} {
		if _, err := telegramFillPath("/api/nodes/{id}", map[string]string{"id": v}); err == nil {
			t.Fatal("accepted path injection", v)
		}
	}
}
func TestTelegramConfirmationPolicy(t *testing.T) {
	for _, a := range []telegramAction{{Method: "DELETE", Path: "/api/nodes/n1"}, {Method: "POST", Path: "/api/tasks"}, {Method: "GET", Path: "/api/nodes/n1/api"}, {Method: "GET", Path: "/api/nodes/n1/outbounds/probe"}, {Method: "GET", Path: "/api/servers/n1/terminal"}} {
		if !telegramNeedsConfirm(a) {
			t.Fatal("missing confirmation", a)
		}
	}
	if telegramNeedsConfirm(telegramAction{Method: "GET", Path: "/api/nodes"}) {
		t.Fatal("read requires unnecessary confirmation")
	}
}
func TestTelegramPendingBoundSingleUse(t *testing.T) {
	now := time.Now()
	st := &telegramControlState{Pending: map[string]telegramPending{"nonce": {Chat: "123", Sender: "123", Until: now.Add(time.Minute)}}}
	if _, ok := st.takePending("nonce", "123", "999", now); ok {
		t.Fatal("other sender consumed")
	}
	var n atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := st.takePending("nonce", "123", "123", now); ok {
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("executed %d times", n.Load())
	}
	st.Pending["stale"] = telegramPending{Chat: "123", Sender: "123", Until: now}
	if _, ok := st.takePending("stale", "123", "123", now); ok {
		t.Fatal("expiry boundary accepted")
	}
}
func TestTelegramRedactsNestedSecrets(t *testing.T) {
	out := telegramCleanJSON([]byte(`{"password":"credential-fixture","items":[{"api_key":"key-fixture","access_token":"access-fixture","label":"客户"}]}`))
	for _, v := range []string{"credential-fixture", "key-fixture", "access-fixture"} {
		if strings.Contains(out, v) {
			t.Fatal("leaked", v)
		}
	}
	if !strings.Contains(out, "客户") {
		t.Fatal("removed normal value")
	}
	if strings.Contains(telegramCleanJSON([]byte("plain-private-output")), "plain-private-output") {
		t.Fatal("raw response leaked")
	}
}
func TestTelegramCatalogOnlyRegisteredPanelRoutes(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	noop := func(http.ResponseWriter, *http.Request) {}
	s.panelRoute("GET /api/nodes", noop)
	s.panelRoute("DELETE /api/nodes/{id}", noop)
	s.mux.HandleFunc("POST /api/login", noop)
	for _, v := range []struct {
		m, p string
		ok   bool
	}{{"GET", "/api/nodes", true}, {"DELETE", "/api/nodes/id", true}, {"POST", "/api/login", false}, {"POST", "/api/store/orders", false}, {"GET", "/api/unknown", false}} {
		if s.telegramAllowed(telegramAction{Method: v.m, Path: v.p}) != v.ok {
			t.Fatal(v)
		}
	}
}
func TestTelegramUsesExistingAuthentication(t *testing.T) {
	signer := auth.NewSigner("fixture-secret", time.Minute)
	s := &Server{mux: http.NewServeMux(), signer: signer, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), sessionUser: func(context.Context, string) (string, string, int64, error) { return "admin", "a@example.test", 2, nil }}
	calls := 0
	s.panelRoute("GET /api/example", s.authenticated(func(w http.ResponseWriter, r *http.Request) {
		calls++
		c, _ := claimsFrom(r.Context())
		if c.Subject != "user-1" {
			t.Fatal("lost subject")
		}
		w.WriteHeader(200)
	}))
	for _, v := range []struct {
		version int64
		status  int
	}{{1, 401}, {2, 200}} {
		token, _ := signer.IssueVersion("user-1", "a@example.test", "admin", v.version)
		req := httptest.NewRequest("GET", "/api/example", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != v.status {
			t.Fatalf("want %d got %d", v.status, rec.Code)
		}
	}
	if calls != 1 {
		t.Fatal("revoked session reached handler")
	}
}

type tgTestTransport struct{ methods *[]string }

func (t tgTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.methods = append(*t.methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
}
func TestTelegramInstallsCommandsAndMenu(t *testing.T) {
	old := telegramHTTP
	defer func() { telegramHTTP = old }()
	methods := []string{}
	telegramHTTP = &http.Client{Transport: tgTestTransport{&methods}}
	if err := (&Server{}).telegramInstallMenu(context.Background(), "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "setMyCommands,setChatMenuButton" {
		t.Fatal(methods)
	}
	if len(telegramCommands()) != 11 { // 2026-10-20: +bans, +ip
		t.Fatal("missing commands")
	}
}
func TestTelegramEmptyCommandDoesNotPanic(t *testing.T) {
	if tgCommand("   ") != "" {
		t.Fatal("empty command")
	}
}

// 2026-11-04：百分号编码不能绕过确认（路由按解码后的路径匹配）；refresh=1 也算有副作用。
func TestTelegramConfirmationSeesDecodedPath(t *testing.T) {
	for _, p := range []string{"/api/nodes/n1/%61pi", "/api/nodes/n1/outbounds/pr%6Fbe", "/api/nodes/capabilities?refresh=1", "/api/portal-update-status?refresh=1", "/api/nodes/%zz"} {
		if !telegramNeedsConfirm(telegramAction{Method: "GET", Path: p}) {
			t.Errorf("GET %s must require confirmation", p)
		}
	}
	if telegramNeedsConfirm(telegramAction{Method: "GET", Path: "/api/nodes/capabilities"}) {
		t.Error("a plain cached read must not require confirmation")
	}
}
