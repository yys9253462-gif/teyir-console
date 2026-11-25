package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 2026-11-03：分类只被「已软删的节点 / 已软删的订阅」引用时必须能删。
// 原来：节点计数不排除软删节点 ⇒ 误报 409；订阅外键没有 ON DELETE 动作 ⇒ DELETE 报 23503 → 500。
// 这里用迁移 026 的真实建表语句（含外键），在 PostgreSQL 上走 categoryDelete。
func TestCategoryDeleteIgnoresSoftDeletedReferences(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("../../migrations", "026_node_categories.sql"))
	if err != nil {
		t.Fatal(err)
	}
	// fixDB 的 subscription_tokens 已带 category_id（没有外键，迁移里的 ADD COLUMN IF NOT EXISTS 会跳过）；
	// 跑完迁移后按 026 的定义补上同样的外键，让「软删订阅引用分类」能真实复现 23503。
	if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS node_category_members`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 026: %v", err)
	}
	if _, err := db.Exec(ctx, `ALTER TABLE subscription_tokens ADD CONSTRAINT subscription_tokens_category_fk FOREIGN KEY (category_id) REFERENCES node_categories(id)`); err != nil {
		t.Fatal(err)
	}
	const cat = "00000000-0000-0000-0000-0000000c0001"
	for _, q := range []string{
		`INSERT INTO node_categories(id, slug, name) VALUES ('` + cat + `', 'fixture-a', 'AI')`,
		`INSERT INTO servers(id) VALUES ('` + fixHost + `')`,
		`INSERT INTO hysteria_nodes(id, name, server_id, deleted_at) VALUES ('` + fixNode + `', 'gone', '` + fixHost + `', now())`,
		`INSERT INTO node_category_members(category_id, node_id) VALUES ('` + cat + `', '` + fixNode + `')`,
		`INSERT INTO subscription_tokens(id, token, order_id, category_id, deleted_at) VALUES ('` + fixSub + `', 'fixture-token-for-customer-only', 'refunded-order', '` + cat + `', now())`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodDelete, "/api/categories/"+cat, nil)
	req.SetPathValue("id", cat)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.categoryDelete(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("category referenced only by soft-deleted rows must be deletable, got %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var left int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM node_categories WHERE id=$1::uuid`, cat).Scan(&left); err != nil || left != 0 {
		t.Fatalf("category still present: %d %v", left, err)
	}

	// 在役订阅仍在用时照旧拒绝。
	const cat2 = "00000000-0000-0000-0000-0000000c0002"
	if _, err := db.Exec(ctx, `INSERT INTO node_categories(id, slug, name) VALUES ('`+cat2+`', 'fixture-b', '跨境');
		UPDATE subscription_tokens SET deleted_at = NULL, category_id = '`+cat2+`'`); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/categories/"+cat2, nil)
	req.SetPathValue("id", cat2)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec = httptest.NewRecorder()
	s.categoryDelete(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a live subscription still using the category must block deletion, got %d", rec.Code)
	}
}

// 2026-11-03：停用的分类不再接受商城新订单（原来照样解析成功，前端的「商城新订单不能再选它」从未生效）。
func TestStoreOrderRejectsDisabledCategory(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("../../migrations", "026_node_categories.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS node_category_members`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 026: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO node_categories(slug, name, enabled) VALUES ('fixture-off', 'Off', false)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	req := httptest.NewRequest(http.MethodPost, "/api/store/orders", strings.NewReader(`{"order_id":"fixture-new-order","duration_days":30,"node_category":"fixture-off"}`))
	rec := httptest.NewRecorder()
	s.storeCreateOrder(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "已停用") {
		t.Fatalf("new order on a disabled category must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE order_id='fixture-new-order'`).Scan(&n)
	if n != 0 {
		t.Fatal("refused order must not create a subscription")
	}
}

// 2026-11-03：新订单 expires_at 已过去必须 400，不能建一条一出生就到期的订阅并回 200。
func TestStoreOrderRejectsPastExpiry(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	past := time.Now().Add(-time.Hour).Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/store/orders", strings.NewReader(`{"order_id":"fixture-past","expires_at":`+strconv.FormatInt(past, 10)+`}`))
	rec := httptest.NewRecorder()
	s.storeCreateOrder(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("past expires_at must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM subscription_tokens WHERE order_id='fixture-past'`).Scan(&n)
	if n != 0 {
		t.Fatal("refused order must not create a row")
	}
}
