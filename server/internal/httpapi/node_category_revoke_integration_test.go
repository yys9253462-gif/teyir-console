package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

type userDeleteRecorder struct {
	mu    sync.Mutex
	users []string
}

func (r *userDeleteRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var in struct {
		UserID string `json:"user_id"`
	}
	if req.Body == nil || !strings.HasSuffix(req.URL.Path, "/users/delete") {
		// 其它调用（如补开户的 GET）不是回收，照常回 ok。
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
	}
	_ = json.NewDecoder(req.Body).Decode(&in)
	r.mu.Lock()
	r.users = append(r.users, in.UserID)
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

// 2026-11-03：把节点移出分类时，只回收「按该分类圈定、且没有按节点直接圈定」的订阅
// 在这台节点上的账号；不圈定的订阅、按节点手选的订阅都不受影响。
func TestRemovingNodeFromCategoryRevokesScopedAccounts(t *testing.T) {
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
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	const catA = "00000000-0000-0000-0000-0000000c0011"
	const subScoped = "00000000-0000-0000-0000-0000000c0021"
	const subAll = "00000000-0000-0000-0000-0000000c0022"
	const subPicked = "00000000-0000-0000-0000-0000000c0023"
	acc := func(uid string) string {
		return `{"` + fixNode + `":{"user_id":"` + uid + `","password":"p"}}`
	}
	for _, q := range []string{
		`INSERT INTO node_categories(id, slug, name) VALUES ('` + catA + `', 'fixture-ca', 'A')`,
		`INSERT INTO servers(id) VALUES ('` + fixHost + `')`,
		`INSERT INTO node_category_members(category_id, node_id) SELECT '` + catA + `', '` + fixNode + `' WHERE false`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, fixNode, key, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO node_category_members(category_id, node_id) VALUES ($1::uuid, $2::uuid)`, catA, fixNode); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []struct {
		id, uid, order string
		cat            any
		nodeIDs        any
	}{
		{subScoped, "sub_scoped", "o1", catA, nil},
		{subAll, "sub_all", "o2", nil, nil},
		{subPicked, "sub_picked", "o3", catA, []string{fixNode}},
	} {
		if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id, token, order_id, category_id, node_ids, node_accounts) VALUES ($1::uuid, $2, $3, $4, $5::uuid[], $6::jsonb)`,
			sub.id, "token-"+sub.uid+"-long-enough", sub.order, sub.cat, sub.nodeIDs, acc(sub.uid)); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.box = box
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	old := portalClient
	rec := &userDeleteRecorder{}
	portalClient = &http.Client{Transport: rec}
	t.Cleanup(func() { portalClient = old })

	req := httptest.NewRequest(http.MethodPut, "/api/nodes/"+fixNode+"/categories", strings.NewReader(`{"category_ids":[]}`))
	req.SetPathValue("id", fixNode)
	w := httptest.NewRecorder()
	s.nodeSetCategories(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body.String())
	}
	if strings.Join(rec.users, ",") != "sub_scoped" {
		t.Fatalf("only the category-scoped subscription's account may be revoked, got %v", rec.users)
	}
	for id, want := range map[string]bool{subScoped: false, subAll: true, subPicked: true} {
		var has bool
		if err := db.QueryRow(ctx, `SELECT node_accounts ? $2 FROM subscription_tokens WHERE id=$1::uuid`, id, fixNode).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if has != want {
			t.Errorf("subscription %s mapping present=%v, want %v", id, has, want)
		}
	}
}

// 2026-11-04：提交同一分类的大写 UUID 时，节点仍在分类里，不能被当成「移出」而回收账号。
func TestSetCategoriesNormalizesIDsBeforeDiff(t *testing.T) {
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
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	const catA = "00000000-0000-0000-0000-0000000c0011"
	const subScoped = "00000000-0000-0000-0000-0000000c0021"
	for _, q := range []string{
		`INSERT INTO node_categories(id, slug, name) VALUES ('` + catA + `', 'fixture-ca', 'A')`,
		`INSERT INTO servers(id) VALUES ('` + fixHost + `')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, fixNode, key, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO node_category_members(category_id, node_id) VALUES ($1::uuid, $2::uuid)`, catA, fixNode); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id, token, order_id, category_id, node_accounts) VALUES ($1::uuid, 'token-sub-scoped-long-enough', 'o1', $2, $3::jsonb)`,
		subScoped, catA, `{"`+fixNode+`":{"user_id":"sub_scoped","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	old := portalClient
	rec := &userDeleteRecorder{}
	portalClient = &http.Client{Transport: rec}
	t.Cleanup(func() { portalClient = old })

	req := httptest.NewRequest(http.MethodPut, "/api/nodes/"+fixNode+"/categories", strings.NewReader(`{"category_ids":["`+strings.ToUpper(catA)+`"]}`))
	req.SetPathValue("id", fixNode)
	w := httptest.NewRecorder()
	s.nodeSetCategories(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body.String())
	}
	rec.mu.Lock()
	revoked := append([]string(nil), rec.users...)
	rec.mu.Unlock()
	if len(revoked) != 0 {
		t.Fatalf("the node is still in the category; nothing may be revoked, got %v", revoked)
	}
	req = httptest.NewRequest(http.MethodPut, "/api/nodes/"+fixNode+"/categories", strings.NewReader(`{"category_ids":["not-a-uuid"]}`))
	req.SetPathValue("id", fixNode)
	w = httptest.NewRecorder()
	s.nodeSetCategories(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed category id must be 400, got %d", w.Code)
	}
}
