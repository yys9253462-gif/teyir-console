package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-12：到期时间极远（超过约 292 年，Sub 饱和）时剩余天数不能回绕成负数。
func TestDaysLeftDoesNotOverflow(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	far := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		expiry *time.Time
		want   int
	}{
		{"perpetual", nil, -1},
		{"already expired", at(-time.Hour), 0},
		{"exactly now", at(0), 0},
		{"one nanosecond", at(time.Nanosecond), 1},
		{"one hour", at(time.Hour), 1},
		{"exactly one day", at(24 * time.Hour), 1},
		{"one day and a bit", at(24*time.Hour + time.Second), 2},
		{"ten years", at(3650 * 24 * time.Hour), 3650},
	} {
		if got := (&subscriptionRecord{ExpiresAt: tc.expiry}).daysLeft(now); got != tc.want {
			t.Errorf("%s: daysLeft = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := (&subscriptionRecord{ExpiresAt: &far}).daysLeft(now); got <= 0 {
		t.Fatalf("a subscription valid until year 9999 reported %d days left; it must stay positive", got)
	}
}

// 2026-11-12：公开订阅端点的令牌形状校验。NUL 与非法 UTF-8 必须在进数据库之前就按「不存在」处理。
func TestPlausibleSubscriptionToken(t *testing.T) {
	good := strings.Repeat("a", 43)
	for _, token := range []string{good, strings.Repeat("a", 20), strings.Repeat("a", 128), "hy2_adopted-token.with~other*chars"} {
		if !plausibleSubscriptionToken(token) {
			t.Errorf("%q must be accepted", token)
		}
	}
	for name, token := range map[string]string{
		"too short":     strings.Repeat("a", 19),
		"too long":      strings.Repeat("a", 129),
		"space":         good[:20] + " " + good[:20],
		"tab":           good[:20] + "\t" + good[:20],
		"slash":         good[:20] + "/" + good[:20],
		"NUL byte":      good[:20] + "\x00" + good[:20],
		"invalid UTF-8": good[:20] + "\xff" + good[:20],
	} {
		if plausibleSubscriptionToken(token) {
			t.Errorf("%s: %q must be rejected", name, token)
		}
	}
}

// 两个公开端点对坏令牌都回 404，而且不访问数据库（Server 没有 db：访问就会 panic）。
func TestPublicSubscriptionEndpointsRejectMalformedTokensWithoutDatabase(t *testing.T) {
	s := &Server{publicLimiter: newSlidingLimiter(time.Minute, 1000)}
	for name, token := range map[string]string{
		"NUL byte":      strings.Repeat("a", 20) + "\x00",
		"invalid UTF-8": strings.Repeat("a", 20) + "\xff",
	} {
		for endpoint, handler := range map[string]http.HandlerFunc{"feed": s.subscriptionFeed, "view": s.subscriptionView} {
			r := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/sub/x", nil)
			r.SetPathValue("token", token)
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s: got %d, want 404", endpoint, name, w.Code)
			}
		}
	}
}

// 2026-11-12：域名拼不成合法 URL 时 checkDeliveryBase 必须返回错误，而不是拿着 nil 请求去发（空指针 panic）。
func TestCheckDeliveryBaseRejectsUnbuildableURL(t *testing.T) {
	err := checkDeliveryBase(context.Background(), "https://a%b.example.com")
	if err == nil || !strings.Contains(err.Error(), "域名格式不对") {
		t.Fatalf("an unbuildable URL must be reported as a bad domain, got %v", err)
	}
}

// 2026-11-12：后台补做「停用」时，失败的只剩已删除节点的残留映射就结束待办，不再每小时重试。
func TestPendingDisableStopsWhenOnlyDeletedNodesRemain(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	for _, sql := range []string{
		`CREATE TABLE subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean NOT NULL DEFAULT false, attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(), last_error text NOT NULL DEFAULT '', register_revoked_order boolean NOT NULL DEFAULT false)`,
		// 订阅已停用；映射里只挂着一台已软删的节点。
		`UPDATE subscription_tokens SET enabled=false, node_accounts='{"` + fixNode + `":{"user_id":"sub_fixture","password":"p"}}'`,
		`INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES('` + fixNode + `','gone',now())`,
		`INSERT INTO subscription_pending_node_actions(subscription_id, action) VALUES('` + fixSub + `','disable')`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.runPendingNodeAction(ctx, fixSub, pendingActionDisable, false, false)

	var pending int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, fixSub).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("a disable action whose only failures are deleted nodes must be closed, not retried forever")
	}
	// 映射本身保留：回收流程仍要靠它把账号移交待补删。
	var mapped bool
	if err := db.QueryRow(ctx, `SELECT node_accounts ? $2 FROM subscription_tokens WHERE id=$1::uuid`, fixSub, fixNode).Scan(&mapped); err != nil || !mapped {
		t.Fatalf("the residual mapping must be kept for the revoke path: mapped=%v err=%v", mapped, err)
	}
}

// 对照：映射里还有在役节点没切成时，待办必须留着继续重试。
func TestPendingDisableKeepsRetryingLiveFailures(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	for _, sql := range []string{
		`CREATE TABLE subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean NOT NULL DEFAULT false, attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(), last_error text NOT NULL DEFAULT '', register_revoked_order boolean NOT NULL DEFAULT false)`,
		`UPDATE subscription_tokens SET enabled=false, node_accounts='{"` + fixNode + `":{"user_id":"sub_fixture","password":"p"}}'`,
		// 在役但没有绑定节点 API：确定性的「该切没切成」。
		`INSERT INTO hysteria_nodes(id,name) VALUES('` + fixNode + `','live')`,
		`INSERT INTO subscription_pending_node_actions(subscription_id, action) VALUES('` + fixSub + `','disable')`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.runPendingNodeAction(ctx, fixSub, pendingActionDisable, false, false)

	var attempts int
	if err := db.QueryRow(ctx, `SELECT attempts FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, fixSub).Scan(&attempts); err != nil {
		t.Fatalf("a live failure must keep the pending action: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("the failed attempt must be recorded for backoff, attempts=%d", attempts)
	}
}
