package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

// A local protocol fixture verifies real SSH command exit handling without a
// production connection or dependence on Docker being installed on the runner.
func hostOpsSSHFixture(t *testing.T) *ssh.Client {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		server, chans, reqs, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(reqs)
		for request := range chans {
			channel, requests, err := request.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer channel.Close()
				for req := range requests {
					if req.Type != "exec" {
						req.Reply(false, nil)
						continue
					}
					var payload struct{ Command string }
					if ssh.Unmarshal(req.Payload, &payload) != nil {
						req.Reply(false, nil)
						return
					}
					req.Reply(true, nil)
					switch payload.Command {
					case "ok":
						channel.Write([]byte("{\"id\":\"fixture\"}\n"))
					case "fail":
						channel.Stderr().Write([]byte("Docker daemon unavailable"))
					case "hang":
						time.Sleep(300 * time.Millisecond)
					}
					status := uint32(0)
					if payload.Command == "fail" {
						status = 1
					}
					channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					return
				}
			}()
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func TestHostOpsRemoteCommandExitAndCancellation(t *testing.T) {
	client := hostOpsSSHFixture(t)
	raw, err := runHostRead(context.Background(), client, "ok")
	if err != nil || !strings.Contains(raw, "fixture") {
		t.Fatalf("%q %v", raw, err)
	}
	if _, err := runHostRead(context.Background(), client, "fail"); err == nil || !strings.Contains(err.Error(), "Docker daemon unavailable") {
		t.Fatalf("nonzero exit not surfaced: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := runHostRead(ctx, client, "hang"); err != context.DeadlineExceeded {
		t.Fatalf("timeout=%v", err)
	}
}

func TestHostOpsDockerParsingMinimizesFields(t *testing.T) {
	rows, err := parseContainers(`{"id":"abc","name":"app","image":"nginx","state":"running","status":"Up","ports":"80/tcp","Env":["SECRET=hidden"]}`)
	if err != nil || len(rows) != 1 || rows[0].ID != "abc" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	data, _ := json.Marshal(rows)
	if strings.Contains(string(data), "SECRET") {
		t.Fatal("sensitive inspect field leaked")
	}
	empty, err := parseContainers("")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("empty successful Docker list must be []")
	}
	for _, raw := range []string{"not-json", `{"name":"missing id"}`} {
		if _, err := parseContainers(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	projects, err := parseCompose(`[{"Name":"stack","Status":"running(1)","ConfigFiles":"/srv/compose.yml","Env":"secret"}]`)
	if err != nil || len(projects) != 1 || projects[0].ConfigFiles != "/srv/compose.yml" {
		t.Fatalf("%v %v", projects, err)
	}
	data, _ = json.Marshal(projects)
	if strings.Contains(string(data), "secret") {
		t.Fatal("compose leaked unknown field")
	}
	if _, err = parseCompose("missing plugin"); err == nil {
		t.Fatal("accepted command error")
	}
}
func TestHostOpsPaths(t *testing.T) {
	for _, v := range []string{"relative", "/bad\x00file", strings.Repeat("/", 4097)} {
		if _, err := remotePath(v); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	if got, err := remotePath("/etc/../tmp/a"); err != nil || got != "/tmp/a" {
		t.Fatalf("%s %v", got, err)
	}
}
func TestHostOpsBoundedOutput(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil {
		t.Fatal("limit ignored")
	}
	if b.Len() != 4 {
		t.Fatal("overflow appended")
	}
}
func TestTerminalOriginAndQuery(t *testing.T) {
	for _, tt := range []struct {
		origin string
		want   bool
	}{{"http://example.com", true}, {"", false}, {"http://evil.example", false}, {"https://example.com", false}, {"http://u@example.com", false}, {"http://example.com/path", false}, {"http://example.com?token=x", false}} {
		r := httptest.NewRequest("GET", "http://example.com/api/servers/x/terminal", nil)
		r.Header.Set("Origin", tt.origin)
		if terminalOriginAllowed(r) != tt.want {
			t.Errorf("origin %q", tt.origin)
		}
	}
	r := httptest.NewRequest("GET", "http://example.com/api/servers/x/terminal?cols=80&rows=24", nil)
	if !terminalQueryAllowed(r) {
		t.Fatal("dimensions rejected")
	}
	for _, q := range []string{"token=x", "access_token=x", "authorization=x", "cookie=x", "cols=80&cols=90", "cols=%ZZ", "cols=80;token=x"} {
		r.URL.RawQuery = q
		if terminalQueryAllowed(r) {
			t.Fatalf("accepted %s", q)
		}
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Origin", "https://example.com")
	if !terminalOriginAllowed(r) {
		t.Fatal("reverse proxy HTTPS rejected")
	}
}
func TestTerminalGateLimitsAndIdempotentCleanup(t *testing.T) {
	var gate terminalSessionGate
	a, ok := gate.acquire("u", "h")
	if !ok {
		t.Fatal("first denied")
	}
	b, ok := gate.acquire("u", "h")
	if !ok {
		t.Fatal("second denied")
	}
	if _, ok = gate.acquire("u", "h2"); ok {
		t.Fatal("user limit ignored")
	}
	if _, ok = gate.acquire("u2", "h"); ok {
		t.Fatal("host limit ignored")
	}
	a()
	a()
	b()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := gate.acquire("u", "h"); ok {
				release()
			}
		}()
	}
	wg.Wait()
	if gate.total != 0 || len(gate.users) != 0 || len(gate.hosts) != 0 {
		t.Fatal("sessions leaked")
	}
	for i := 0; i < 16; i++ {
		release, ok := gate.acquire(string(rune('a'+i)), string(rune('a'+i)))
		if !ok {
			t.Fatal("global limit too low")
		}
		defer release()
	}
	if _, ok := gate.acquire("extra", "extra"); ok {
		t.Fatal("global limit ignored")
	}
}
func TestTerminalCookieAndAuth(t *testing.T) {
	signer := auth.NewSigner("test-secret-that-is-long-enough", time.Hour)
	server := &Server{signer: signer, sessionUser: func(_ context.Context, id string) (string, string, int64, error) {
		return id, "user@example.invalid", 0, nil
	}}
	admin, _ := signer.Issue("admin", "admin@example.invalid", "admin")
	viewer, _ := signer.Issue("viewer", "viewer@example.invalid", "viewer")
	r := httptest.NewRequest("POST", "https://example.com/api/auth/terminal-session", nil)
	r.Header.Set("Authorization", "Bearer "+admin)
	w := httptest.NewRecorder()
	server.authenticated(server.requireAdmin(server.terminalCookie))(w, r)
	if w.Code != 200 {
		t.Fatalf("cookie status=%d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/servers/" {
		t.Fatalf("cookie=%v", cookies)
	}
	called := 0
	handler := server.terminalAuthenticated(server.requireAdmin(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	request := func(token, origin, query string) int {
		req := httptest.NewRequest("GET", "http://example.com/api/servers/x/terminal"+query, nil)
		req.Header.Set("Origin", origin)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: terminalCookieName, Value: token})
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	if code := request(admin, "http://example.com", ""); code != 204 || called != 1 {
		t.Fatalf("admin %d calls %d", code, called)
	}
	for _, tt := range []struct {
		token, origin, query string
		status               int
	}{{viewer, "http://example.com", "", 403}, {"", "http://example.com", "", 401}, {admin, "http://evil.example", "", 403}, {admin, "http://example.com", "?token=x", 400}, {"expired", "http://example.com", "", 401}} {
		if code := request(tt.token, tt.origin, tt.query); code != tt.status {
			t.Errorf("got %d want %d", code, tt.status)
		}
	}
	if called != 1 {
		t.Fatal("denied request reached handler")
	}
}
func TestTerminalRequiresUpgradeBeforeSSH(t *testing.T) {
	server := &Server{}
	r := httptest.NewRequest("GET", "http://example.com/api/servers/x/terminal", nil)
	w := httptest.NewRecorder()
	server.remoteTerminal(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	for _, dims := range [][2]int{{0, 24}, {80, 0}, {501, 24}, {80, 201}, {-1, 24}} {
		if terminalDimensions(dims[0], dims[1]) {
			t.Fatal("invalid dimensions accepted")
		}
	}
}
