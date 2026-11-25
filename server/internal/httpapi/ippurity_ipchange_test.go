package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 2026-11-04：IP 变了以后，旧 IP 的注册国 / 所在国 / 原生判定必须作废，不能留给新 IP。
func TestIPPurityUpsertResetsNativeOnIPChange(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	for _, m := range []string{"034_node_ip_purity.sql", "037_ip_native.sql", "038_purity_score_source.sql"} {
		raw, err := os.ReadFile(filepath.Join("../../migrations", m))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name) VALUES($1::uuid,'n')`, fixNode); err != nil {
		t.Fatal(err)
	}
	upsert := func(ip string) {
		t.Helper()
		if _, err := db.Exec(ctx, upsertIPPuritySQL, fixNode, ip, "datacenter", nil, nil, nil, nil, nil, "", "", "{}", "api", "ipapi", ""); err != nil {
			t.Fatal(err)
		}
	}
	upsert("1.1.1.1")
	if _, err := db.Exec(ctx, `UPDATE node_ip_purity SET reg_country='US', geo_country='US', is_native=true WHERE node_id=$1::uuid`, fixNode); err != nil {
		t.Fatal(err)
	}
	upsert("1.1.1.1")
	var reg string
	var native *bool
	if err := db.QueryRow(ctx, `SELECT reg_country, is_native FROM node_ip_purity WHERE node_id=$1::uuid`, fixNode).Scan(&reg, &native); err != nil {
		t.Fatal(err)
	}
	if reg != "US" || native == nil || !*native {
		t.Fatalf("same IP must keep the native verdict, got reg=%q native=%v", reg, native)
	}
	upsert("8.8.8.8")
	if err := db.QueryRow(ctx, `SELECT reg_country, is_native FROM node_ip_purity WHERE node_id=$1::uuid`, fixNode).Scan(&reg, &native); err != nil {
		t.Fatal(err)
	}
	if reg != "" || native != nil {
		t.Fatalf("a new IP must not inherit the old IP's countries, got reg=%q native=%v", reg, native)
	}
}

// 2026-11-04：批量纯净度检测进行中再触发必须 409（每次都消耗第三方配额，不能叠加）。
func TestRefreshAllIPPurityDoesNotStack(t *testing.T) {
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	refreshAllPurityRunning.Store(true)
	t.Cleanup(func() { refreshAllPurityRunning.Store(false) })
	rec := httptest.NewRecorder()
	s.refreshAllIPPurity(rec, httptest.NewRequest(http.MethodPost, "/api/ip-purity/refresh-all", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second bulk check while one runs must be 409, got %d", rec.Code)
	}
}

// 2026-11-06：降级结果（公网 API / 单 IP 评分失败）不能覆盖上一次的评分，也不能清空 error。
func TestIPPurityPartialResultKeepsPreviousScore(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	for _, m := range []string{"034_node_ip_purity.sql", "037_ip_native.sql", "038_purity_score_source.sql"} {
		raw, err := os.ReadFile(filepath.Join("../../migrations", m))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name) VALUES($1::uuid,'n')`, fixNode); err != nil {
		t.Fatal(err)
	}
	score, proxy := 85, true
	if _, err := db.Exec(ctx, upsertIPPuritySQL, fixNode, "1.1.1.1", "datacenter", nil, &score, &proxy, nil, nil, "AS1", "x", "{}", "api", "ipqs", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, upsertIPPuritySQL, fixNode, "1.1.1.1", "datacenter", nil, nil, nil, nil, nil, "AS1", "x", "{}", "offline", "", "公网 API 查询失败，仅有离线库结果：429"); err != nil {
		t.Fatal(err)
	}
	var got *int
	var gotProxy *bool
	var src, errText string
	if err := db.QueryRow(ctx, `SELECT fraud_score, is_proxy, score_source, error FROM node_ip_purity WHERE node_id=$1::uuid`, fixNode).Scan(&got, &gotProxy, &src, &errText); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != 85 || gotProxy == nil || !*gotProxy || src != "ipqs" {
		t.Fatalf("partial result must keep the previous score, got score=%v proxy=%v source=%q", got, gotProxy, src)
	}
	if errText == "" {
		t.Fatal("partial result must record why the score is stale")
	}
	// 完整成功照常覆盖并清空 error。
	low := 10
	if _, err := db.Exec(ctx, upsertIPPuritySQL, fixNode, "1.1.1.1", "datacenter", nil, &low, nil, nil, nil, "AS1", "x", "{}", "api", "ipqs", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT fraud_score, error FROM node_ip_purity WHERE node_id=$1::uuid`, fixNode).Scan(&got, &errText); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != 10 || errText != "" {
		t.Fatalf("a complete result must overwrite, got score=%v error=%q", got, errText)
	}
}
