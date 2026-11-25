package httpapi

import (
	"context"
	"testing"
)

// 2026-11-04：即使与续费交错，finishPendingNodeAction 也不能把已经重新生效的订阅软删。
func TestFinishPendingActionKeepsRenewedSubscription(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db) // enabled, 30 天后到期 = 生效中
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean, next_attempt_at timestamptz, last_error text, attempts int DEFAULT 0, register_revoked_order boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.finishPendingNodeAction(ctx, &subscriptionRecord{ID: fixSub}, true, true)
	var deleted bool
	if err := db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("an active (renewed) subscription must not be soft-deleted by a stale pending action")
	}
	// 真正停用的订阅照常软删。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET enabled=false`); err != nil {
		t.Fatal(err)
	}
	s.finishPendingNodeAction(ctx, &subscriptionRecord{ID: fixSub}, true, true)
	if err := db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("a disabled subscription must still be soft-deleted: %v %v", deleted, err)
	}
}

// 2026-11-06：只有商城撤单来源的待办才登记 store_revoked_orders；订阅中心手动删除走后台补删时不能拉黑订单号
// （买家之后续费、按同一 order_id 重新下单必须照常可用）。
func TestFinishPendingActionRegistersRevokedOrderOnlyForStoreRevoke(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register bool
		want     int
	}{{"manual delete", false, 0}, {"store revoke", true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			db := fixDB(t)
			ctx := context.Background()
			createFixSub(t, db)
			if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET enabled=false`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean, next_attempt_at timestamptz, last_error text, attempts int DEFAULT 0, register_revoked_order boolean NOT NULL DEFAULT false)`); err != nil {
				t.Fatal(err)
			}
			s := fixServer(db)
			s.finishPendingNodeAction(ctx, &subscriptionRecord{ID: fixSub, OrderID: "fixture-order"}, true, tc.register)
			var n int
			if err := db.QueryRow(ctx, `SELECT count(*) FROM store_revoked_orders WHERE order_id='fixture-order'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.want {
				t.Fatalf("revoked order rows: want %d, got %d", tc.want, n)
			}
		})
	}
}
