package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/secrets"
)

// 2026-11-06：商城撤单（delete=true）后，同一 order_id 的重试下单不能再开一份订阅。
func TestRevokedStoreOrderCannotBeRecreated(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()
	const order = "fixture-refunded-order"
	if _, err := db.Exec(ctx, `INSERT INTO store_revoked_orders(order_id, subscription_id) VALUES($1, $2::uuid)`, order, fixSub); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/store/orders", strings.NewReader(`{"order_id":"`+order+`","duration_days":30}`))
	rec := httptest.NewRecorder()
	s.storeCreateOrder(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("撤销过的订单号应回 410，实际 %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE order_id=$1`, order).Scan(&n); err != nil || n != 0 {
		t.Fatalf("不能建出新订阅：n=%d err=%v", n, err)
	}
	// 撤单（delete=true）成功时要登记撤销记录。
	body := funcBlock(t, "store.go", "func (s *Server) storeRevokeOrder(")
	del := strings.Index(body, "UPDATE subscription_tokens SET deleted_at=now()")
	ins := -1
	if del >= 0 {
		ins = strings.LastIndex(body[:del], "INSERT INTO store_revoked_orders")
	}
	if ins < 0 || del < 0 || ins > del {
		t.Fatal("撤单软删前必须在同一事务里登记 store_revoked_orders")
	}
}

// 2026-11-06：订阅已停用且有撤单回收待办时，不带 renewal_id 的续期按可能的重放拒绝（409），
// 待办与停用状态都不动。
func TestRenewWithoutRenewalIDRefusedWhileRevokePending(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean, next_attempt_at timestamptz, last_error text, attempts int DEFAULT 0, register_revoked_order boolean NOT NULL DEFAULT false)`,
		`UPDATE subscription_tokens SET enabled=false WHERE id='` + fixSub + `'`,
		`INSERT INTO subscription_pending_node_actions(subscription_id, action, then_delete, next_attempt_at, last_error) VALUES('` + fixSub + `','revoke',false,now(),'x')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(30 * 24 * time.Hour).Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/store/renew", strings.NewReader(`{"order_id":"fixture-order","expires_at":`+strconv.FormatInt(future, 10)+`}`))
	rec := httptest.NewRecorder()
	s.storeRenewOrder(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("撤单回收进行中、不带 renewal_id 的续期应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	var enabled bool
	var pending int
	if err := db.QueryRow(ctx, `SELECT enabled, (SELECT count(*) FROM subscription_pending_node_actions) FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&enabled, &pending); err != nil {
		t.Fatal(err)
	}
	if enabled || pending != 1 {
		t.Fatalf("订阅不能被重新启用、回收待办不能被删：enabled=%v pending=%d", enabled, pending)
	}
	// 2026-11-06：相对续期（带 renewal_id）原来能绕过这个闸门 —— 而它恰恰是商城重发的形态。
	rel := httptest.NewRequest(http.MethodPost, "/api/store/renew", strings.NewReader(`{"order_id":"fixture-order","duration_days":30,"renewal_id":"retry-1"}`))
	recRel := httptest.NewRecorder()
	s.storeRenewOrder(recRel, rel)
	if recRel.Code != http.StatusConflict {
		t.Fatalf("撤单回收进行中、带 renewal_id 的相对续期同样应回 409，实际 %d %s", recRel.Code, recRel.Body.String())
	}
	if err := db.QueryRow(ctx, `SELECT enabled, (SELECT count(*) FROM subscription_pending_node_actions) FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&enabled, &pending); err != nil {
		t.Fatal(err)
	}
	if enabled || pending != 1 {
		t.Fatalf("相对续期也不能把订阅重新启用 / 删掉回收待办：enabled=%v pending=%d", enabled, pending)
	}
}

// 2026-11-06：撤销过的订单号也不能经「接管手工订阅」挂上去。
func TestStoreAdoptRefusesRevokedOrder(t *testing.T) {
	body := funcBlock(t, "store_adopt.go", "func (s *Server) storeAdopt(")
	check := strings.Index(body, "FROM store_revoked_orders WHERE order_id=$1")
	attach := strings.Index(body, "UPDATE subscription_tokens SET order_id=$2")
	if check < 0 || attach < 0 || check > attach {
		t.Fatal("storeAdopt 关联订单号之前必须检查 store_revoked_orders")
	}
}

// 2026-11-06：发送测试消息前先检查 Chat ID 与 Token，缺了直接说缺什么（400）。
func TestTelegramTestRequiresChatAndToken(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.purityKeys = ippurity.NewKeyStore(nil)
	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.testTelegram(rec, httptest.NewRequest(http.MethodPost, "/api/settings/telegram/test", nil))
		return rec
	}
	if rec := call(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Chat ID") {
		t.Fatalf("没有 Chat ID 应回 400 并说明，实际 %d %s", rec.Code, rec.Body.String())
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO system_settings(key,value) VALUES($1,'123456')`, telegramChatSettingKey); err != nil {
		t.Fatal(err)
	}
	if rec := call(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Token") {
		t.Fatalf("没有 Token 应回 400 并说明，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 2026-11-06：订阅早已被清理时撤单（delete=true）仍要登记订单号，之后重放的下单不能重新开户。
func TestRevokeOfGoneOrderStillRegistersIt(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()
	const order = "fixture-purged-then-refunded"
	req := httptest.NewRequest(http.MethodPost, "/api/store/revoke", strings.NewReader(`{"order_id":"`+order+`","delete":true}`))
	rec := httptest.NewRecorder()
	s.storeRevokeOrder(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoking a gone order stays 404, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM store_revoked_orders WHERE order_id=$1`, order).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a revoke of a gone order must be recorded: n=%d err=%v", n, err)
	}
	create := httptest.NewRecorder()
	s.storeCreateOrder(create, httptest.NewRequest(http.MethodPost, "/api/store/orders", strings.NewReader(`{"order_id":"`+order+`","duration_days":30}`)))
	if create.Code != http.StatusGone {
		t.Fatalf("a replayed create after the revoke must be 410, got %d", create.Code)
	}
}

// 2026-11-06：同一 order_id 幂等命中「已过期 / 已停用」的行时，不能回 200 ok:true（商城会当成已交付，
// 而响应里没有任何可用的链接）。回 409 让它走 renew 或换单号。
func TestCreateOrderIdempotentHitOnDeadRowIsConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		key  string
	}{{"expired", `UPDATE subscription_tokens SET expires_at=now()-interval '1 hour' WHERE id=$1::uuid`, "expired"},
		{"disabled", `UPDATE subscription_tokens SET enabled=false WHERE id=$1::uuid`, "disabled"}} {
		t.Run(tc.name, func(t *testing.T) {
			db := fixDB(t)
			createFixSub(t, db)
			if _, err := db.Exec(context.Background(), tc.sql, fixSub); err != nil {
				t.Fatal(err)
			}
			s := fixServer(db)
			rec := httptest.NewRecorder()
			s.storeCreateOrder(rec, httptest.NewRequest(http.MethodPost, "/api/store/orders",
				strings.NewReader(`{"order_id":"fixture-order","duration_days":30}`)))
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s row: want 409, got %d %s", tc.name, rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body[tc.key] != true || body["ok"] == true {
				t.Fatalf("the response must name the reason and not claim success: %s", rec.Body.String())
			}
			// 不能顺手建出新订阅。
			var n int
			if err := db.QueryRow(context.Background(), `SELECT count(*) FROM subscription_tokens WHERE order_id='fixture-order'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("no second row may be created: n=%d err=%v", n, err)
			}
		})
	}
}

// failingPortal 对任何请求回 500，用来让节点回收失败。
type failingPortal struct{}

func (failingPortal) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"boom"}`)), Header: make(http.Header)}, nil
}

// 2026-11-06：撤单（delete=true）的订单号必须在**回收结果出来之前**落盘。
// 原来只在「所有节点都撤干净」的事务里写：一台节点不可达就只登记待办，订单号只活在订阅行上；
// 待办执行时若行已被清理，待办作废，blacklist 永远不落 → 同一 order_id 的下单重放会重新开户（退款后货又发）。
func TestRevokeBlacklistsOrderBeforeContactsNodes(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'n.example.test',443)`, fixNode, key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=$2::jsonb WHERE id=$1::uuid`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_x","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: failingPortal{}}
	t.Cleanup(func() { portalClient = old })

	rec := httptest.NewRecorder()
	s.storeRevokeOrder(rec, httptest.NewRequest(http.MethodPost, "/api/store/revoke",
		strings.NewReader(`{"order_id":"fixture-order","delete":true}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("partial revoke must report 502, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM store_revoked_orders WHERE order_id='fixture-order'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refunded order id must be blacklisted even though revocation is pending: n=%d err=%v", n, err)
	}
	// 重放的下单请求必须被 410 挡住（哪怕行后来被清理）。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET deleted_at=now() WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRecorder()
	s.storeCreateOrder(create, httptest.NewRequest(http.MethodPost, "/api/store/orders",
		strings.NewReader(`{"order_id":"fixture-order","duration_days":30}`)))
	if create.Code != http.StatusGone {
		t.Fatalf("a replayed create after refund must be 410, got %d %s", create.Code, create.Body.String())
	}
}
