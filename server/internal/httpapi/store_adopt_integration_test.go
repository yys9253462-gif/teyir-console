package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2026-11-04：手工订阅用着未上架节点时不能直接关联订单（关联后会变成买家订阅、这些节点被移出）。
func TestStoreAdoptRefusesSubscriptionUsingUnlistedNodes(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,in_store) VALUES('`+fixNode+`','自用-东京',false);
INSERT INTO subscription_tokens(id,token,order_id,expires_at,node_accounts) VALUES('`+fixSub+`','manual-subscription-token-fixture-0001',NULL,now()+interval '30 days',
 jsonb_build_object('`+fixNode+`', jsonb_build_object('user_id','sub_x','password','p')))`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	adopt := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/store/adopt", strings.NewReader(`{"token":"manual-subscription-token-fixture-0001","order_id":"OPS-prdord_fixture01"}`))
		rec := httptest.NewRecorder()
		s.storeAdopt(rec, req)
		return rec
	}
	if rec := adopt(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "自用-东京") {
		t.Fatalf("adopting a subscription that uses unlisted nodes must be refused with their names, got %d %s", rec.Code, rec.Body.String())
	}
	var order *string
	_ = db.QueryRow(ctx, `SELECT order_id FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&order)
	if order != nil && *order != "" {
		t.Fatal("refused adoption must not set order_id")
	}
	// 节点上架后可以关联。
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET in_store=true`); err != nil {
		t.Fatal(err)
	}
	if rec := adopt(); rec.Code != http.StatusOK {
		t.Fatalf("adoption with only listed nodes must succeed, got %d %s", rec.Code, rec.Body.String())
	}
}
