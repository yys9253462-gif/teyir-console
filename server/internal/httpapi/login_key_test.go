package httpapi

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 2026-11-06：IPv6 按 /64 计数，同一 /64 里换地址不能绕过登录限流与封禁；IPv4 仍按单个地址。
func TestLoginKeyBucketsIPv6By64(t *testing.T) {
	a, b := loginKey("2001:db8:1:2::1"), loginKey("2001:db8:1:2:ffff::9")
	if a != b {
		t.Fatalf("two addresses in one /64 must share a key: %q vs %q", a, b)
	}
	if loginKey("2001:db8:1:3::1") == a {
		t.Fatal("a different /64 must get a different key")
	}
	if loginKey("203.0.113.7") != "203.0.113.7" || loginKey("203.0.113.8") == loginKey("203.0.113.7") {
		t.Fatal("IPv4 stays per-address")
	}
	g := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < loginFailMax; i++ {
		g.fail(loginKey(fmt.Sprintf("2001:db8:9:9::%x", i+1)), now)
	}
	if g.bannedUntil(loginKey("2001:db8:9:9::beef"), now).IsZero() {
		t.Fatal("rotating addresses inside one /64 must still trigger the ban")
	}
}

// 2026-11-06：改密接口猜当前密码要计次封禁，不能只靠写限流。
func TestChangePasswordCountsWrongCurrentPassword(t *testing.T) {
	body := funcBlock(t, "manage.go", "func (s *Server) changePassword(")
	verify := strings.Index(body, "if !auth.VerifyPassword(hash, input.Current) {")
	ban := strings.Index(body, "loginGuardState.bannedUntil(guardKey")
	fail := strings.Index(body, "loginGuardState.fail(guardKey")
	if verify < 0 || ban < 0 || fail < 0 || ban > verify || fail < verify {
		t.Fatal("changePassword must refuse while locked out and count every wrong current password")
	}
	if !strings.Contains(body, `"change_password_failed"`) {
		t.Fatal("a wrong current password must be audited")
	}
}
