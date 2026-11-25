package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStoreSignature(t *testing.T) {
	secret := "shop-secret-123456"
	now := time.Unix(1_800_000_000, 0)
	mk := func(method, path, body string, ts time.Time, tamper func(sig string) string) (bool, error) {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		tsRaw := strconv.FormatInt(ts.Unix(), 10)
		sig := storeSign(secret, storeSignatureMessage(tsRaw, method, path, []byte(body)))
		if tamper != nil {
			sig = tamper(sig)
		}
		r.Header.Set("X-Store-Timestamp", tsRaw)
		r.Header.Set("X-Store-Signature", sig)
		return verifyStoreSignature(r, secret, now)
	}

	if signed, err := mk("POST", "/api/store/orders", `{"order_id":"a1"}`, now, nil); !signed || err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// 同一请求再发一次 = 重放
	if _, err := mk("POST", "/api/store/orders", `{"order_id":"a1"}`, now, nil); err == nil {
		t.Fatal("replayed signature must be rejected")
	}
	if _, err := mk("POST", "/api/store/orders", `{"order_id":"a2"}`, now.Add(-6*time.Minute), nil); err == nil {
		t.Fatal("stale timestamp must be rejected")
	}
	if _, err := mk("POST", "/api/store/orders", `{"order_id":"a3"}`, now, func(s string) string { return "00" + s[2:] }); err == nil {
		t.Fatal("tampered signature must be rejected")
	}
	// 请求体被改：用 a4 签名，实际发 a5
	r := httptest.NewRequest("POST", "/api/store/orders", bytes.NewBufferString(`{"order_id":"a5"}`))
	tsRaw := strconv.FormatInt(now.Unix(), 10)
	r.Header.Set("X-Store-Timestamp", tsRaw)
	r.Header.Set("X-Store-Signature", storeSign(secret, storeSignatureMessage(tsRaw, "POST", "/api/store/orders", []byte(`{"order_id":"a4"}`))))
	if _, err := verifyStoreSignature(r, secret, now); err == nil {
		t.Fatal("body change must invalidate the signature")
	}
	// 没带签名头 = unsigned，交给「强制签名」开关决定
	plain := httptest.NewRequest("GET", "/api/store/orders/x", nil)
	if signed, err := verifyStoreSignature(plain, secret, now); signed || err != nil {
		t.Fatalf("unsigned request should pass through as unsigned: %v %v", signed, err)
	}
}

func TestStoreSignatureBodyStillReadable(t *testing.T) {
	secret := "shop-secret-123456"
	now := time.Now()
	body := `{"order_id":"keep"}`
	r := httptest.NewRequest("POST", "/api/store/orders", bytes.NewBufferString(body))
	tsRaw := strconv.FormatInt(now.Unix(), 10)
	r.Header.Set("X-Store-Timestamp", tsRaw)
	r.Header.Set("X-Store-Signature", storeSign(secret, storeSignatureMessage(tsRaw, "POST", "/api/store/orders", []byte(body))))
	if _, err := verifyStoreSignature(r, secret, now); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	if buf.String() != body {
		t.Fatalf("handler must still see the body, got %q", buf.String())
	}
}

func TestLoginHistoryReadErrorsAreVisible(t *testing.T) {
	body := funcBlock(t, "loginsecurity.go", "func (s *Server) myLogins(")
	for _, want := range []string{
		"rows.Scan(&it.IP, &it.UserAgent, &it.FirstSeen, &it.LastSeen, &it.Count); err != nil",
		"if err := rows.Err(); err != nil",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("myLogins must report incomplete login history instead of returning an empty list; missing %q", want)
		}
	}
}

func TestLoginGuardBans(t *testing.T) {
	g := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < loginFailMax-1; i++ {
		if g.fail("1.2.3.4", now) {
			t.Fatal("banned too early")
		}
	}
	if !g.fail("1.2.3.4", now) {
		t.Fatal("should ban on the 5th failure")
	}
	if g.bannedUntil("1.2.3.4", now).IsZero() {
		t.Fatal("ban not recorded")
	}
	if !g.bannedUntil("1.2.3.4", now.Add(loginBanFor+time.Second)).IsZero() {
		t.Fatal("ban must expire")
	}
	// 窗口外的旧失败不累计
	g2 := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
	for i := 0; i < loginFailMax-1; i++ {
		g2.fail("5.6.7.8", now.Add(-loginFailWindow-time.Minute))
	}
	if g2.fail("5.6.7.8", now) {
		t.Fatal("failures outside the window must not count")
	}
}

func TestLoginTurnstileRejectWithoutDBAuditAndSilentBan(t *testing.T) {
	t.Setenv("TURNSTILE_SITE_KEY", "FAKE-SITE-KEY-TEST-ONLY")
	t.Setenv("TURNSTILE_SECRET", "FAKE-SECRET-KEY-TEST-ONLY")

	// 重置被测攻击 IP 的内存封禁状态
	const testIP = "198.51.100.77"
	loginGuardState.mu.Lock()
	delete(loginGuardState.fails, testIP)
	delete(loginGuardState.banned, testIP)
	loginGuardState.mu.Unlock()
	t.Cleanup(func() {
		loginGuardState.mu.Lock()
		delete(loginGuardState.fails, testIP)
		delete(loginGuardState.banned, testIP)
		loginGuardState.mu.Unlock()
	})

	s := &Server{
		loginLimiter: newSlidingLimiter(5*time.Minute, 100),
	}

	// 前 loginFailMax (5) 次无 token 爆破请求：均应在查库前返回 403，且在第 5 次触发内存封禁
	for i := 0; i < loginFailMax; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"email":"admin@example.invalid","password":"FAKE_PASSWORD_123456"}`))
		req.RemoteAddr = testIP + ":54321"
		rec := httptest.NewRecorder()
		s.login(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("第 %d 次无令牌请求应返回 403，实际 %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	// 第 6 次请求：因为已被 loginGuardState 静默封禁，应在入口直接返回 429
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"email":"admin@example.invalid","password":"FAKE_PASSWORD_123456"}`))
	req.RemoteAddr = testIP + ":54321"
	rec := httptest.NewRecorder()
	s.login(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("连续 %d 次未过人机验证后应触发内存封禁返回 429，实际 %d: %s", loginFailMax, rec.Code, rec.Body.String())
	}

	// 源码契约守卫：确保 verifyTurnstile 失败分支不会重新引入 auditLoginFailure 写库调用
	raw, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读取 server.go 失败: %v", err)
	}
	src := string(raw)
	// 登录分支现在是 `if !validLoginService(...) && !s.verifyTurnstile(...)`（程序登录服务密钥
	// 只免人机验证），所以按 `!s.verifyTurnstile(` 定位，再从它所在的 if 行开始检查分支体。
	idx := strings.Index(src, "!s.verifyTurnstile(")
	if idx >= 0 {
		idx = strings.LastIndex(src[:idx], "if ")
	}
	if idx < 0 {
		t.Fatal("未找到 verifyTurnstile 分支")
	}
	end := strings.Index(src[idx:], "return")
	if end < 0 {
		t.Fatal("verifyTurnstile 分支未找到 return")
	}
	branch := src[idx : idx+end]
	if strings.Contains(branch, "auditLoginFailure") {
		t.Fatal("verifyTurnstile 失败分支严禁调用 auditLoginFailure 写库，否则无令牌分布式爆破将写爆 audit_logs")
	}
	if !strings.Contains(branch, "loginGuardState.fail") {
		t.Fatal("verifyTurnstile 失败分支必须调用 loginGuardState.fail 静默封禁高频攻击 IP")
	}
}
