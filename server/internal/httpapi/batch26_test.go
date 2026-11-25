package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-06：改角色、删账号、立即运行巡检都要走写限流（原来这三个漏了）。
func TestRemainingWriteHandlersAreRateLimited(t *testing.T) {
	for _, c := range []struct{ file, fn string }{
		{"admin.go", "func (s *Server) updateUser("},
		{"admin.go", "func (s *Server) deleteUser("},
		{"scheduler.go", "func (s *Server) runScheduleNow("},
	} {
		if !strings.Contains(funcBlock(t, c.file, c.fn), "s.enforceWriteLimit(w, claims.Subject)") {
			t.Errorf("%s 必须走写限流", c.fn)
		}
	}
}

// 2026-11-06：合并订阅的停用 / 启用（会联动所有节点账号）只给管理员，与换新链接同一门槛。
func TestLegacySubscriptionUpdateIsAdminOnly(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret", JWTTTL: time.Minute})
	s.sessionUser = func(context.Context, string) (string, string, int64, error) {
		return "operator", "op@example.test", 0, nil
	}
	token, err := s.signer.Issue("user-op", "op@example.test", "operator")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/subscription", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("运维账号停用合并订阅应回 403，实际 %d %s", rec.Code, rec.Body.String())
	}
}
