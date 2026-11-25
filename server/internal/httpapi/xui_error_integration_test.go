package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-03：3X-UI 面板接口遇到数据库错误时原来 502 + Postgres 原文
// （表名、SQLSTATE）。fixDB 的 schema 里没有 xui_panels 表，正好制造一次真实的数据库错误。
func TestXUITargetErrorsDoNotLeakDatabaseText(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.readLimiter = newSlidingLimiter(time.Minute, 100)
	for id, want := range map[string]int{
		"not-a-uuid":                           http.StatusNotFound,
		"00000000-0000-0000-0000-0000000e0001": http.StatusInternalServerError,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/xui/panels/"+id+"/inbounds", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.xuiInbounds(rec, req)
		body := rec.Body.String()
		if rec.Code != want {
			t.Errorf("%s: want %d, got %d %s", id, want, rec.Code, body)
		}
		if strings.Contains(body, "SQLSTATE") || strings.Contains(body, "xui_panels") {
			t.Errorf("%s: response leaks database error: %s", id, body)
		}
	}
}

// 2026-11-03：刷新纯净度时「节点不存在」原来返回 502。
func TestRefreshIPPurityUnknownNodeIs404(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-0000000e0002"} {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/"+id+"/ip-purity/refresh", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		s.refreshNodeIPPurity(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d %s", id, rec.Code, rec.Body.String())
		}
	}
}

// 2026-11-03：到期结案与账号对账对不存在的订阅原来分别回 409 / 把数据库故障也说成「订阅不存在」。
func TestSubscriptionHandlersNotFoundIs404(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-0000000e0003"} {
		req := httptest.NewRequest(http.MethodPost, "/api/subscriptions/"+id+"/close-expired", nil)
		req.SetPathValue("id", id)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		rec := httptest.NewRecorder()
		s.closeExpiredSubscriptionHandler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("close-expired %s: want 404, got %d %s", id, rec.Code, rec.Body.String())
		}
		req = httptest.NewRequest(http.MethodPost, "/api/subscriptions/"+id+"/reconcile", nil)
		req.SetPathValue("id", id)
		rec = httptest.NewRecorder()
		s.reconcileSubscriptionHandler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("reconcile %s: want 404, got %d %s", id, rec.Code, rec.Body.String())
		}
	}
	// 数据库故障不能被说成「订阅不存在」。
	if _, err := db.Exec(context.Background(), `ALTER TABLE subscription_tokens RENAME TO subscription_tokens_gone`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/subscriptions/x/reconcile", nil)
	req.SetPathValue("id", "00000000-0000-0000-0000-0000000e0003")
	rec := httptest.NewRecorder()
	s.reconcileSubscriptionHandler(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Errorf("database failure reported as not found: %s", rec.Body.String())
	}
}

// 2026-11-03：3X-UI 探测失败时原来把入站/客户端/在线计数全部写成 0。
func TestXUIProbeFailureKeepsLastCounts(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const panel = "00000000-0000-0000-0000-0000000e0004"
	if _, err := db.Exec(ctx, `CREATE TABLE xui_panels(id uuid PRIMARY KEY,last_status text,last_error text,last_checked_at timestamptz,inbounds_count int,clients_count int,online_count int,updated_at timestamptz);
INSERT INTO xui_panels VALUES ('`+panel+`','ok','',now(),3,12,4,now())`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.recordXUIStatus(ctx, panel, "error", "dial timeout", 0, 0, 0)
	var status string
	var inbounds, clients, online int
	if err := db.QueryRow(ctx, `SELECT last_status,inbounds_count,clients_count,online_count FROM xui_panels`).Scan(&status, &inbounds, &clients, &online); err != nil {
		t.Fatal(err)
	}
	if status != "error" || inbounds != 3 || clients != 12 || online != 4 {
		t.Fatalf("failed probe must only mark status, got %s %d/%d/%d", status, inbounds, clients, online)
	}
	s.recordXUIStatus(ctx, panel, "ok", "", 5, 20, 0)
	if err := db.QueryRow(ctx, `SELECT inbounds_count,clients_count,online_count FROM xui_panels`).Scan(&inbounds, &clients, &online); err != nil {
		t.Fatal(err)
	}
	if inbounds != 5 || clients != 20 || online != 0 {
		t.Fatalf("successful probe must write counts, got %d/%d/%d", inbounds, clients, online)
	}
}
