package httpapi

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 用真实的路由注册（New）检查：敏感接口一个都不能出现在 Telegram 可调用列表里。
// 以后谁新加了一个账号 / 凭据 / 密钥类接口而忘了加进黑名单，只要命名落在这些路径下，这里就会挡住。
func TestTelegramCannotReachSensitiveRoutes(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	if len(s.telegramControl.Routes) < 50 {
		t.Fatalf("expected the real route catalog, got only %d routes", len(s.telegramControl.Routes))
	}
	for _, r := range s.telegramControl.Routes {
		if telegramForbidden(r.Pattern) {
			t.Errorf("forbidden route is reachable from Telegram: %s", r.Pattern)
		}
		path := strings.SplitN(r.Pattern, " ", 2)[1]
		for _, bad := range []string{"/api/users", "/api/auth/", "purity-keys", "/settings/telegram", "store-key", "/credential", "/fingerprint", "/terminal", "/files", "/write-ops", "/alerts/webhook"} {
			if strings.Contains(path, bad) {
				t.Errorf("sensitive route leaked into Telegram catalog: %s", r.Pattern)
			}
		}
	}

	denied := []telegramAction{
		{Method: "POST", Path: "/api/users"},
		{Method: "POST", Path: "/api/users/abc/password"},
		{Method: "DELETE", Path: "/api/users/abc"},
		{Method: "POST", Path: "/api/auth/change-password"},
		{Method: "PUT", Path: "/api/settings/purity-keys"},
		{Method: "PUT", Path: "/api/settings/telegram"},
		{Method: "POST", Path: "/api/subscriptions/store-key"},
		{Method: "POST", Path: "/api/servers/abc/credential"},
		{Method: "POST", Path: "/api/servers/abc/fingerprint/accept"},
		{Method: "PUT", Path: "/api/alerts/webhook"},
	}
	for _, a := range denied {
		if s.telegramAllowed(a) {
			t.Errorf("Telegram must not be allowed to call %s %s", a.Method, a.Path)
		}
	}
	// 日常运维仍可用
	for _, a := range []telegramAction{{Method: "GET", Path: "/api/nodes"}, {Method: "GET", Path: "/api/servers"}, {Method: "GET", Path: "/api/subscriptions"}} {
		if !s.telegramAllowed(a) {
			t.Errorf("everyday route should stay available: %s %s", a.Method, a.Path)
		}
	}
}

func TestScheduleRetryWaitsMatchCap(t *testing.T) {
	if len(scheduleRetryWaits) != scheduleMaxRetries {
		t.Fatalf("retry waits (%d) must match the retry cap (%d)", len(scheduleRetryWaits), scheduleMaxRetries)
	}
	for i := 1; i < len(scheduleRetryWaits); i++ {
		if scheduleRetryWaits[i] <= scheduleRetryWaits[i-1] {
			t.Fatal("retry waits must back off")
		}
	}
}

// 2026-11-03：菜单上的快捷按钮必须都能真正调用，否则每次点都只回一句「不是已登记的接口」。
func TestTelegramQuickButtonsAreReachable(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	for key, path := range telegramQuickPaths {
		if !s.telegramAllowed(telegramAction{Method: "GET", Path: path}) {
			t.Errorf("quick button ux:q:%s points at %s, which Telegram is not allowed to call", key, path)
		}
	}
	src, err := os.ReadFile("telegram_ui.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`"ux:q:([a-z]+)"`).FindAllStringSubmatch(string(src), -1) {
		if telegramQuickPaths[m[1]] == "" {
			t.Errorf("menu button ux:q:%s has no entry in telegramQuickPaths", m[1])
		}
	}
}

// 2026-11-04：写入节点 / 3X-UI 访问凭据的接口不能从 Telegram 调；同路径的只读接口照常可用。
func TestTelegramCannotWriteNodeOrPanelCredentials(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	for _, a := range []telegramAction{
		{Method: "POST", Path: "/api/nodes"},
		{Method: "POST", Path: "/api/nodes/preview"},
		{Method: "POST", Path: "/api/nodes/00000000-0000-0000-0000-000000000001/api"},
		{Method: "POST", Path: "/api/xui/panels"},
		{Method: "POST", Path: "/api/xui/panels/preview"},
	} {
		if s.telegramAllowed(a) {
			t.Errorf("Telegram must not be allowed to call %s %s", a.Method, a.Path)
		}
	}
	for _, a := range []telegramAction{{Method: "GET", Path: "/api/nodes"}, {Method: "GET", Path: "/api/xui/panels"}} {
		if !s.telegramAllowed(a) {
			t.Errorf("read route should stay available: %s %s", a.Method, a.Path)
		}
	}
}

// 2026-11-04：队列满时被放弃的更新要告诉用户（按钮回调要 answerCallbackQuery），不能静默丢弃。
func TestTelegramBusyQueueNotifiesUser(t *testing.T) {
	src, err := os.ReadFile("telegram.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, want := range []string{
		"dispatch := func(fn func(), quick bool, onBusy func()) {",
		"go run(onBusy)",
		`"answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": busyNotice`,
		"_ = s.sendTelegram(ctx, chatID, busyNotice)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("telegram.go missing %q", want)
		}
	}
}

// 2026-11-04：200 + node_state_ok=false / failed_nodes>0 也是「没全部生效」，不能当成功。
func TestTelegramPartialOutcomeDetection(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"enabled":false,"node_state_ok":false,"node_state_message":"部分节点停用失败"}`: true,
		`{"enabled":false,"node_state_ok":true}`:                                  false,
		`{"ok_nodes":3,"failed_nodes":1}`:                                         true,
		`{"ok_nodes":3,"failed_nodes":0}`:                                         false,
		`{"failed":[{"user_id":"u"}]}`:                                            true,
		`{"failed":{}}`:                                                           false,
		`{"status":"ok"}`:                                                         false,
		// 2026-11-06：计数形式的失败同样算部分失败。
		`{"ok":3,"failed":2,"errors":["x"]}`:    true,
		`{"ok":3,"failed":0}`:                   false,
		`{"status":"deleted","failed_count":2}`: true,
		`{"ok":1,"errors":["timeout"]}`:         true,
	} {
		got, note := telegramPartialOutcome([]byte(raw))
		if got != want {
			t.Errorf("%s: partial=%v, want %v", raw, got, want)
		}
		if got && strings.Contains(raw, "node_state_message") && note == "" {
			t.Errorf("%s: the node message must be surfaced", raw)
		}
	}
	body := funcBlock(t, "telegram_control.go", "func (s *Server) telegramRun(")
	if !strings.Contains(body, "rec.Code != 202 && !partial {") {
		t.Fatal("a partial outcome must not get the ✅ prefix")
	}
}
