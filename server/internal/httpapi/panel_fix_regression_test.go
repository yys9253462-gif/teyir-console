package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"nodeops/server/internal/auth"
	"strings"
	"testing"
	"time"
)

func TestAccountSyncPreservesUsageAndDisabledState(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour)
	for _, enabled := range []bool{true, false} {
		rec := &subscriptionRecord{ID: "example", Enabled: enabled, ExpiresAt: &exp, IPLimit: 3}
		calls := []string{}
		err := syncAccount(rec, "node", now, func(method, path string, p map[string]any) error {
			calls = append(calls, method+" "+path)
			if method == http.MethodPost {
				if path != "/users/update" {
					t.Fatal("已有用户不得create")
				}
				for _, key := range []string{"used_bytes", "limit_bytes", "created_at", "traffic_gb", "duration_days"} {
					if _, ok := p[key]; ok {
						t.Fatalf("不能覆盖 %s", key)
					}
				}
				want := "disabled"
				if enabled {
					want = "active"
				}
				if p["status"] != want {
					t.Fatal(p)
				}
				if p["expires_at"] != exp.Unix() || p["ip_limit"] != 3 {
					t.Fatal(p)
				}
			}
			return nil
		})
		if err != nil || len(calls) != 2 || !strings.HasPrefix(calls[0], "GET ") {
			t.Fatal(err, calls)
		}
	}
}

func TestAccountSyncMissingExpiredNeverCreates(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	for _, rec := range []*subscriptionRecord{{ID: "expired", Enabled: true, ExpiresAt: &past}, {ID: "disabled", Enabled: false}} {
		calls := 0
		err := syncAccount(rec, "node", now, func(method, path string, p map[string]any) error { calls++; return errors.New("User not found") })
		if err == nil || calls != 1 {
			t.Fatal("到期/停用缺失用户不允许复活", err, calls)
		}
	}
}

func TestAccountSyncOldPortalNeverFallsBackToCreate(t *testing.T) {
	calls := 0
	err := syncAccount(&subscriptionRecord{ID: "active", Enabled: true}, "node", time.Now(), func(method, path string, p map[string]any) error {
		calls++
		return errors.New("API endpoint not found")
	})
	if err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestAccountSyncMissingActiveCreatesOnlyWithAtomicGuard(t *testing.T) {
	calls := 0
	err := syncAccount(&subscriptionRecord{ID: "active", Enabled: true}, "node", time.Now(), func(method, path string, p map[string]any) error {
		calls++
		if calls == 1 {
			return errors.New("User not found")
		}
		if path != "/users/create" || p["create_only"] != true {
			t.Fatal(path, p)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestExpiredAttributesNeverActive(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	p := subscriptionAttributes(&subscriptionRecord{Enabled: true, ExpiresAt: &past, IPLimit: 0}, now)
	if p["status"] != "disabled" || p["expires_at"] != past.Unix() || p["ip_limit"] != 0 {
		t.Fatal(p)
	}
}

func TestSessionRevocationLegacyResetDemotionDeletion(t *testing.T) {
	signer := auth.NewSigner("regression-test-secret", time.Hour)
	role := "admin"
	version := int64(0)
	deleted := false
	s := &Server{signer: signer, sessionUser: func(context.Context, string) (string, string, int64, error) {
		if deleted {
			return "", "", 0, errors.New("deleted")
		}
		return role, "x@example.invalid", version, nil
	}}
	legacy, _ := signer.Issue("id", "x@example.invalid", "admin")
	verify := func(token string) (auth.Claims, error) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return s.authenticate(r)
	}
	if _, err := verify(legacy); err != nil {
		t.Fatal("旧token版本0兼容", err)
	}
	role = "viewer"
	c, err := verify(legacy)
	if err != nil || c.Role != "viewer" {
		t.Fatal("当前角色优先", c, err)
	}
	version++
	if _, err := verify(legacy); err == nil {
		t.Fatal("重置后旧token必须撤销")
	}
	current, _ := signer.IssueVersion("id", "x@example.invalid", "viewer", version)
	if _, err := verify(current); err != nil {
		t.Fatal(err)
	}
	deleted = true
	if _, err := verify(current); err == nil {
		t.Fatal("删除后token必须撤销")
	}
}

func TestExpirySweepDoesNotDisableFailedMappings(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) sweepExpiredSubscriptions(")
	if !strings.Contains(body, "COALESCE(node_accounts,'{}'::jsonb)='{}'::jsonb") {
		t.Fatal("仅确认清空账号映射后才关闭enabled；失败行须留在原候选入口")
	}
}

func TestExplicitSameStateAndQuotaEditsRetry(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) updateSubscriptionHandler(")
	if strings.Contains(body, "if enabled != rec.Enabled {") {
		t.Fatal("同值停用请求必须可以重试")
	}
	if !strings.Contains(body, "input.Enabled != nil") || !strings.Contains(body, "syncSubscriptionNodeAttributes") {
		t.Fatal("显式状态/期限/IP限额必须同步")
	}
}

func TestAllPasswordMutationPathsBumpVersion(t *testing.T) {
	for _, file := range []string{"admin.go", "manage.go"} {
		src := funcBlock(t, file, map[string]string{"admin.go": "func (s *Server) resetUserPassword(", "manage.go": "func (s *Server) changePassword("}[file])
		if !strings.Contains(src, "token_version=token_version+1") {
			t.Fatal(file, "密码更新必须原子撤销旧版本")
		}
	}
}

// 2026-11-04：同步已有账号时沿用映射里的口令；只有映射里没有时才用确定性推导值。
func TestAccountSyncKeepsMappedPassword(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		mapped, want string
	}{
		{"legacy-working-password", "legacy-working-password"},
		{"", provisionAccountPassword(&subscriptionRecord{ID: "example"}, "node")},
	} {
		rec := &subscriptionRecord{ID: "example", Enabled: true, ExpiresAt: &exp}
		if tc.mapped != "" {
			rec.nodeAccounts = map[string]subscriptionNodeAccount{"node": {UserID: "sub_example", Password: tc.mapped}}
		}
		var sent any
		err := syncAccount(rec, "node", time.Now(), func(method, path string, p map[string]any) error {
			if method == http.MethodPost {
				sent = p["password"]
			}
			return nil
		})
		if err != nil || sent != tc.want {
			t.Fatalf("mapped=%q: want password %q, sent %v (err=%v)", tc.mapped, tc.want, sent, err)
		}
	}
}
