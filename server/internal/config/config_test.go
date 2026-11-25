package config

import "testing"

// 2026-11-04：布尔开关接受 1/0/TRUE 等写法，写错则启动失败，而不是静默按「关」处理。
func TestEnvBool(t *testing.T) {
	for raw, want := range map[string]bool{"1": true, "TRUE": true, "true": true, "0": false, "false": false, " True ": true} {
		t.Setenv("FIXTURE_BOOL", raw)
		got, err := envBool("FIXTURE_BOOL", !want)
		if err != nil || got != want {
			t.Errorf("envBool(%q) = %v,%v want %v", raw, got, err, want)
		}
	}
	t.Setenv("FIXTURE_BOOL", "")
	if got, err := envBool("FIXTURE_BOOL", true); err != nil || !got {
		t.Error("unset must use the fallback")
	}
	t.Setenv("FIXTURE_BOOL", "yes-please")
	if _, err := envBool("FIXTURE_BOOL", false); err == nil {
		t.Error("garbage must be rejected")
	}
}

func TestLoadRejectsBadSecurityToggle(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SEED_ADMIN_EMAIL", "a@b.c")
	t.Setenv("SEED_ADMIN_PASSWORD", "fixture-password-123")
	t.Setenv("CREDENTIAL_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("VIEWER_RESTRICT_SUBSCRIPTIONS", "1")
	cfg, err := Load()
	if err != nil || !cfg.ViewerRestrictSubscriptions {
		t.Fatalf("VIEWER_RESTRICT_SUBSCRIPTIONS=1 must enable the restriction: %v %+v", err, cfg.ViewerRestrictSubscriptions)
	}
	t.Setenv("VIEWER_RESTRICT_SUBSCRIPTIONS", "on")
	if _, err := Load(); err == nil {
		t.Fatal("an unparseable security toggle must fail startup")
	}
}
