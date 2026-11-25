package httpapi

import (
	"strings"
	"testing"
)

func TestLoginServiceAccountScope(t *testing.T) {
	key := strings.Repeat("k", 48)
	t.Setenv("LOGIN_SERVICE_KEY", key)
	t.Setenv("LOGIN_SERVICE_EMAIL", "machine@example.invalid")
	for _, tc := range []struct {
		email, key string
		want       bool
	}{
		{"machine@example.invalid", key, true},
		{"other@example.invalid", key, false},
		{"machine@example.invalid", "", false},
		{"machine@example.invalid", key + "x", false},
		{"machine@example.invalid", strings.Repeat("x", 1025), false},
	} {
		if got := validLoginService(tc.email, tc.key); got != tc.want {
			t.Errorf("scope mismatch: got %v want %v", got, tc.want)
		}
	}
	t.Setenv("LOGIN_SERVICE_KEY", "short")
	if validLoginService("machine@example.invalid", "short") {
		t.Fatal("weak key accepted")
	}
	t.Setenv("LOGIN_SERVICE_KEY", key)
	t.Setenv("LOGIN_SERVICE_EMAIL", "")
	t.Setenv("SEED_ADMIN_EMAIL", "")
	if validLoginService("machine@example.invalid", key) {
		t.Fatal("missing account scope accepted")
	}
	t.Setenv("SEED_ADMIN_EMAIL", "machine@example.invalid")
	if !validLoginService("machine@example.invalid", key) {
		t.Fatal("explicit seed account fallback failed")
	}
}
