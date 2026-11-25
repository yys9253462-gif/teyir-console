package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No application configuration is loaded. The explicit opt-in DSN must point
// to a disposable database; every case owns and drops a synthetic schema.
func repairSubscriptionDB(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("HY2_REPAIR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HY2_REPAIR_TEST_DATABASE_URL required (disposable PostgreSQL only)")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("hy2_repair_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); db.Close() })
	_, err = db.Exec(ctx, `CREATE SCHEMA `+schema+`;
 CREATE TABLE subscription_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), token text NOT NULL UNIQUE,
 label text NOT NULL DEFAULT 'fixture', enabled boolean NOT NULL DEFAULT true,
 expires_at timestamptz, ip_limit int NOT NULL DEFAULT 1, auto_delete boolean NOT NULL DEFAULT false,
 order_id text UNIQUE, user_email text, note text NOT NULL DEFAULT '', active_ips jsonb NOT NULL DEFAULT '[]',
 created_at timestamptz NOT NULL DEFAULT now(), created_by uuid,
 last_accessed_at timestamptz, access_count bigint NOT NULL DEFAULT 0, deleted_at timestamptz,
 node_accounts jsonb NOT NULL DEFAULT '{}', category_id uuid, node_ids uuid[]);
 CREATE TABLE hysteria_nodes(id uuid,name text,api_url text,api_key bytea,domain text,listen_port int,
 updated_at timestamptz,created_at timestamptz,deleted_at timestamptz,in_store boolean,portal_basic_user text,portal_basic_password bytea,portal_path_token text);
 CREATE TABLE audit_logs(user_id uuid,action text,resource_type text,resource_id uuid,ip_address inet,details jsonb);`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../migrations/057_panel_merged_subscription.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 057: %v", err)
	}
	return &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), writeLimiter: newSlidingLimiter(time.Second, 100), publicLimiter: newSlidingLimiter(time.Second, 100)}
}

func repairExec(t *testing.T, s *Server, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func repairSnapshot(t *testing.T, s *Server, query string, args ...any) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(context.Background(), query, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

const repairFirst = "00000000-0000-4000-8000-000000000001"
const repairSecond = "00000000-0000-4000-8000-000000000002"
const repairDeleted = "00000000-0000-4000-8000-000000000003"
const repairToken = "isolated-fixture-subscription-token-only"

func repairFeed(s *Server, token, ip, ua string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/sub/"+token, nil)
	r.SetPathValue("token", token)
	r.RemoteAddr = ip
	r.Header.Set("User-Agent", ua)
	w := httptest.NewRecorder()
	s.subscriptionFeed(w, r)
	return w
}

func TestRepairF05RotateOnlyGETTarget(t *testing.T) {
	s := repairSubscriptionDB(t)
	ctx := context.Background()
	// Reverse insertion order, identical live creation times, and an older deleted
	// row ensure both stable ORDER BY keys and the deletion filter matter.
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,created_at,order_id,user_email,note,ip_limit,active_ips,access_count,last_accessed_at)
 VALUES($1,'second-customer-token-fixture','2020-01-01','order-second','second@example.invalid','keep second',7,'[]',8,now())`, repairSecond)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,created_at,order_id,user_email,note,ip_limit,active_ips,access_count,last_accessed_at,is_panel_merged)
 VALUES($1,$2,'2020-01-01','','first@example.invalid','keep first',2,
 '[{"ip":"198.51.100.1","seen_at":"2026-01-01T00:00:00Z"}]',9,now(),true)`, repairFirst, repairToken)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,created_at,deleted_at,enabled) VALUES($1,'deleted-customer-token-fixture','2010-01-01',now(),false)`, repairDeleted)
	state, err := s.readSubscriptionToken(ctx)
	if err != nil || state.Token != repairToken {
		t.Fatalf("wrong GET target: %+v, %v", state, err)
	}
	others := repairSnapshot(t, s, `SELECT jsonb_agg(to_jsonb(st) ORDER BY id)::text FROM subscription_tokens st WHERE id<>$1::uuid`, repairFirst)
	termsQuery := `SELECT (to_jsonb(st)-'token'-'last_accessed_at'-'access_count'-'active_ips')::text FROM subscription_tokens st WHERE id=$1::uuid`
	terms := repairSnapshot(t, s, termsQuery, repairFirst)
	r := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/api/subscription/rotate", nil)
	r.Header.Set("X-Confirm-Dangerous", "yes")
	w := httptest.NewRecorder()
	s.rotateSubscriptionToken(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", w.Code, w.Body.String())
	}
	var reply struct{ Token, URL, Path string }
	if err = json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Token) != 43 || reply.Token == repairToken || reply.Path != "/sub/"+reply.Token {
		t.Fatal("invalid rotate response")
	}
	after, err := s.readSubscriptionToken(ctx)
	if err != nil || after.Token != reply.Token {
		t.Fatalf("next GET changed target: %+v %v", after, err)
	}
	if got := repairSnapshot(t, s, termsQuery, repairFirst); got != terms {
		t.Fatal("rotation changed creation time or customer terms")
	}
	if got := repairSnapshot(t, s, `SELECT jsonb_agg(to_jsonb(st) ORDER BY id)::text FROM subscription_tokens st WHERE id<>$1::uuid`, repairFirst); got != others {
		t.Fatal("another customer or deleted row changed")
	}
	var count int
	var accessed *time.Time
	var ips string
	if err = s.db.QueryRow(ctx, `SELECT access_count,last_accessed_at,active_ips::text FROM subscription_tokens WHERE id=$1`, repairFirst).Scan(&count, &accessed, &ips); err != nil {
		t.Fatal(err)
	}
	if count != 0 || accessed != nil || ips != "[]" {
		t.Fatal("rotation did not reset usage metadata")
	}
	if _, err = s.loadSubscriptionByToken(ctx, repairToken); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("old token still resolves", err)
	}
	if _, err = s.loadSubscriptionByToken(ctx, reply.Token); err != nil {
		t.Fatal("new token does not resolve", err)
	}
	if got := repairFeed(s, repairToken, "198.51.100.10:1234", "mihomo"); got.Code != 404 {
		t.Fatal("old feed still usable", got.Code)
	}
	if got := repairFeed(s, reply.Token, "198.51.100.10:1234", "mihomo"); got.Code != 200 {
		t.Fatal("new feed failed", got.Code, got.Body.String())
	}
}

// 2026-11-03：只有买家订阅时，面板合并订阅的读取 / 换链接 / 停用都不能落到买家行上。
// 原来按「最早一行」取：商城订单早于首次打开订阅中心时，换新链接让买家的链接 404。
func TestOperatorSubscriptionNeverTargetsBuyerRows(t *testing.T) {
	s := repairSubscriptionDB(t)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,created_at,order_id) VALUES($1,'buyer-token-fixture-must-stay','2020-01-01','order-buyer')`, repairSecond)
	// 2026-11-06：订阅中心手工开给客户的订阅（没有 order_id）同样不能被当成合并订阅。
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,created_at,order_id) VALUES($1,'manual-customer-token-must-stay','2019-01-01',NULL)`, repairFirst)
	manualBefore := repairSnapshot(t, s, `SELECT (to_jsonb(st)-'last_accessed_at')::text FROM subscription_tokens st WHERE id=$1::uuid`, repairFirst)
	before := repairSnapshot(t, s, `SELECT (to_jsonb(st)-'last_accessed_at')::text FROM subscription_tokens st WHERE id=$1::uuid`, repairSecond)
	if _, err := s.readSubscriptionToken(context.Background()); err == nil {
		t.Fatal("a buyer row must not be read as the panel's own subscription")
	}
	r := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/api/subscription/rotate", nil)
	r.Header.Set("X-Confirm-Dangerous", "yes")
	w := httptest.NewRecorder()
	s.rotateSubscriptionToken(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := repairSnapshot(t, s, `SELECT (to_jsonb(st)-'last_accessed_at')::text FROM subscription_tokens st WHERE id=$1::uuid`, repairSecond); got != before {
		t.Fatalf("rotate touched the buyer's subscription:\nbefore %s\nafter  %s", before, got)
	}
	state, err := s.readSubscriptionToken(context.Background())
	if err != nil || state.Token == "buyer-token-fixture-must-stay" || state.Token == "manual-customer-token-must-stay" {
		t.Fatalf("panel subscription must be a separate operator row: %+v %v", state, err)
	}
	if got := repairSnapshot(t, s, `SELECT (to_jsonb(st)-'last_accessed_at')::text FROM subscription_tokens st WHERE id=$1::uuid`, repairFirst); got != manualBefore {
		t.Fatalf("rotate touched a manual customer subscription:\nbefore %s\nafter  %s", manualBefore, got)
	}
}

func TestRepairF05RotationConfirmationAndEmptyTable(t *testing.T) {
	s := repairSubscriptionDB(t)
	r := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/api/subscription/rotate", nil)
	w := httptest.NewRecorder()
	s.rotateSubscriptionToken(w, r)
	if w.Code != 428 {
		t.Fatal("confirmation guard missing", w.Code)
	}
	if got := repairSnapshot(t, s, `SELECT count(*)::text FROM subscription_tokens`); got != "0" {
		t.Fatal("unconfirmed rotate wrote data")
	}
	r.Header.Set("X-Confirm-Dangerous", "yes")
	w = httptest.NewRecorder()
	s.rotateSubscriptionToken(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var reply struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &reply)
	state, err := s.readSubscriptionToken(context.Background())
	if err != nil || reply.Token == "" || reply.Token != state.Token {
		t.Fatal("empty table response not persisted", err)
	}
}

func TestRepairF05PerIDRotationRejectsDeletedAndMissing(t *testing.T) {
	s := repairSubscriptionDB(t)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,deleted_at) VALUES($1,$2,now())`, repairDeleted, repairToken)
	before := repairSnapshot(t, s, `SELECT to_jsonb(st)::text FROM subscription_tokens st`)
	for _, id := range []string{repairDeleted, repairFirst} {
		r := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/api/subscriptions/fixture/rotate", nil)
		r.SetPathValue("id", id)
		r.Header.Set("X-Confirm-Dangerous", "yes")
		w := httptest.NewRecorder()
		s.rotateSubscriptionHandler(w, r)
		if w.Code != 404 {
			t.Fatal("deleted/missing rotate accepted", w.Code)
		}
	}
	if got := repairSnapshot(t, s, `SELECT to_jsonb(st)::text FROM subscription_tokens st`); got != before {
		t.Fatal("deleted row modified")
	}
}

func TestRepairF04UserAgentsAlwaysResolveClientIP(t *testing.T) {
	s := &Server{}
	uas := []string{"mihomo/1.0", "curl/8.10.1", "Go-http-client/1.1", "python-requests/2.32", "okhttp/4.12", "TelegramBot", "MicroMessenger", "HeadlessChrome", "generic-spider", ""}
	for _, ua := range uas {
		t.Run(ua, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/sub/fixture", nil)
			r.RemoteAddr = "198.51.100.10:1234"
			r.Header.Set("User-Agent", ua)
			if got := s.subscriptionClientIP(r); got != "198.51.100.10" {
				t.Fatalf("direct IP=%q", got)
			}
			r.Header.Set("X-Forwarded-For", "203.0.113.20, 172.19.0.2")
			if got := s.subscriptionClientIP(r); got != requestClientIP(r) || got == "" {
				t.Fatalf("proxy IP=%q", got)
			}
		})
	}
}

func TestRepairF04MissingIPFailsClosedAndPreservesAccessReasons(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	cases := []struct {
		name, ip string
		enabled  bool
		expiry   *time.Time
		limit    int
		ok       bool
		reason   string
	}{
		{"limited missing", "", true, nil, 1, false, "ip_unknown"},
		{"limited whitespace", " \t", true, nil, 1, false, "ip_unknown"},
		{"unlimited missing", "", true, nil, 0, true, ""},
		{"expired missing", "", true, &past, 1, false, "expired"},
		{"disabled missing", "", false, nil, 1, false, "disabled"},
		// 2026-11-12：到期先于停用。到期清扫收回账号后会把订阅置为 enabled=false，拒绝原因不能因此变成 disabled。
		{"expired and swept to disabled", "198.51.100.10", false, &past, 1, false, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{} // nil DB also proves these branches cannot write the ledger.
			ok, reason := s.enforceSubscriptionAccess(context.Background(), &subscriptionRecord{Enabled: tc.enabled, ExpiresAt: tc.expiry, IPLimit: tc.limit}, tc.ip)
			if ok != tc.ok || reason != tc.reason {
				t.Fatal(ok, reason)
			}
			if !ok {
				w := httptest.NewRecorder()
				writeSubscriptionRefusal(w, reason)
				if w.Code != 403 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestRepairF04CompleteFeedsChargeCrawlerIPs(t *testing.T) {
	for _, ua := range []string{"curl/8.10.1", "Go-http-client/1.1", "TelegramBot", "okhttp/4.12", "mihomo/1.0", ""} {
		t.Run(ua, func(t *testing.T) {
			s := repairSubscriptionDB(t)
			repairExec(t, s, `INSERT INTO subscription_tokens(id,token,ip_limit) VALUES($1,$2,1)`, repairFirst, repairToken)
			first := repairFeed(s, repairToken, "198.51.100.10:1234", ua)
			if first.Code != 200 {
				t.Fatal(first.Code, first.Body.String())
			}
			ledger := repairSnapshot(t, s, `SELECT active_ips::text FROM subscription_tokens WHERE id=$1`, repairFirst)
			if !strings.Contains(ledger, "198.51.100.10") {
				t.Fatal("successful complete feed did not charge its IP", ledger)
			}
			repeat := repairFeed(s, repairToken, "198.51.100.10:5678", ua)
			if repeat.Code != 200 {
				t.Fatal("known IP should remain allowed", repeat.Code)
			}
			second := repairFeed(s, repairToken, "203.0.113.20:1234", ua)
			if second.Code != 403 || !strings.Contains(second.Body.String(), "ip limit exceeded") {
				t.Fatal("new crawler IP bypassed quota", second.Code, second.Body.String())
			}
			if got := repairSnapshot(t, s, `SELECT active_ips::text FROM subscription_tokens WHERE id=$1`, repairFirst); strings.Contains(got, "203.0.113.20") {
				t.Fatal("rejected IP added to ledger")
			}
		})
	}
}

func TestRepairF04FeedMissingExpiredDisabledAndUnlimited(t *testing.T) {
	s := repairSubscriptionDB(t)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,ip_limit) VALUES($1,$2,1)`, repairFirst, repairToken)
	check := func(ip string, want int, body string) {
		t.Helper()
		w := repairFeed(s, repairToken, ip, "TelegramBot")
		if w.Code != want || !strings.Contains(w.Body.String(), body) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	check("", 403, "client ip unavailable")
	repairExec(t, s, `UPDATE subscription_tokens SET enabled=false`)
	check("", 403, "subscription disabled")
	repairExec(t, s, `UPDATE subscription_tokens SET enabled=true,expires_at=now()-interval '1 hour'`)
	check("", 403, "subscription expired")
	repairExec(t, s, `UPDATE subscription_tokens SET expires_at=NULL,ip_limit=0`)
	check("198.51.100.10:1234", 200, "")
	check("203.0.113.20:1234", 200, "")
	check("", 200, "")
}
