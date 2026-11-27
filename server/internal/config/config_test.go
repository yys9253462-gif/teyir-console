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

// 验证配置加载拒绝纯空白关键字段与非法可信代理网段，避免非法配置穿透到路由初始化时触发 panic。
func TestLoadValidatesTrustedProxyCIDRsAndWhitespaceSecrets(t *testing.T) {
	setBase := func() {
		t.Setenv("DATABASE_URL", " postgres://u:p@db:5432/app ")
		t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
		t.Setenv("SEED_ADMIN_EMAIL", " admin@example.invalid ")
		t.Setenv("SEED_ADMIN_PASSWORD", "fixture-password-123")
		t.Setenv("CREDENTIAL_KEY", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= ")
		t.Setenv("PORT", " 9090 ")
		t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1/32,\t10.0.0.0/8\n192.168.0.0/16")
	}
	setBase()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("合法带空白配置应加载成功: %v", err)
	}
	if cfg.Port != 9090 || cfg.DatabaseURL != "postgres://u:p@db:5432/app" || cfg.SeedAdminEmail != "admin@example.invalid" || cfg.CredentialKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Fatalf("配置项未正确裁剪首尾空白: %+v", cfg)
	}
	if len(cfg.TrustedProxyCIDRs) != 3 || cfg.TrustedProxyCIDRs[1] != "10.0.0.0/8" || cfg.TrustedProxyCIDRs[2] != "192.168.0.0/16" {
		t.Fatalf("可信代理网段切分异常: %+v", cfg.TrustedProxyCIDRs)
	}

	// 非法 CIDR 必须在配置加载阶段返回错误，而非留给路由层 panic。
	setBase()
	t.Setenv("TRUSTED_PROXY_CIDRS", "127.0.0.1")
	if _, err := Load(); err == nil {
		t.Fatal("缺少掩码的 TRUSTED_PROXY_CIDRS 必须在配置加载阶段报错")
	}

	// 32 个纯空格不得绕过 JWT_SECRET 强度检查。
	setBase()
	t.Setenv("JWT_SECRET", "                                ")
	if _, err := Load(); err == nil {
		t.Fatal("纯空格 JWT_SECRET 必须被拒绝")
	}

	// 纯空白必填字段不得通过校验。
	for _, envKey := range []string{"DATABASE_URL", "SEED_ADMIN_EMAIL", "CREDENTIAL_KEY"} {
		setBase()
		t.Setenv(envKey, "   \t  ")
		if _, err := Load(); err == nil {
			t.Fatalf("纯空白 %s 必须被拒绝", envKey)
		}
	}
}
