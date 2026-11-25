package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-06：登录查库出错（这里 users 表不存在）时回 503，不算一次登录失败，也不能报「邮箱或密码错误」。
func TestLoginDatabaseErrorIsNotAFailedLogin(t *testing.T) {
	t.Setenv("TURNSTILE_SITE_KEY", "")
	t.Setenv("TURNSTILE_SECRET", "")
	const testIP = "198.51.100.78"
	reset := func() {
		loginGuardState.mu.Lock()
		delete(loginGuardState.fails, testIP)
		delete(loginGuardState.banned, testIP)
		loginGuardState.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	s := fixServer(fixDB(t))
	s.loginLimiter = newSlidingLimiter(5*time.Minute, 100)
	for i := 0; i < loginFailMax+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"email":"admin@example.invalid","password":"right-password-1"}`))
		req.RemoteAddr = testIP + ":54321"
		rec := httptest.NewRecorder()
		s.login(rec, req)
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "邮箱或密码错误") {
			t.Fatalf("第 %d 次：数据库出错应回 503 而不是「密码错误」，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if !loginGuardState.bannedUntil(testIP, time.Now()).IsZero() {
		t.Fatal("数据库故障期间的登录不能计入失败次数、把管理员自己的 IP 封掉")
	}
}
