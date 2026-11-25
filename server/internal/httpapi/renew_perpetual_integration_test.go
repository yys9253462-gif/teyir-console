package httpapi

import (
	"context"
	"testing"
	"time"
)

// 2026-11-03：按天续费不能把永久订阅变成有限期；有期限的照常从 max(到期, now) 往后加。
func TestStoreRenewKeepsPerpetualSubscriptions(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET expires_at=NULL WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, storeRenewQuery, nil, 30, 0, fixSub); err != nil {
		t.Fatal(err)
	}
	var exp *time.Time
	if err := db.QueryRow(ctx, `SELECT expires_at FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if exp != nil {
		t.Fatalf("relative renewal turned a perpetual subscription into one expiring %v", exp)
	}
	// 已到期的有限期订阅：从现在开始加。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET expires_at=now()-interval '10 days' WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, storeRenewQuery, nil, 30, 0, fixSub); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT expires_at FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&exp); err != nil || exp == nil {
		t.Fatal(err)
	}
	if d := time.Until(*exp); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expired subscription renewed by 30 days should end ~30 days from now, got %v", d)
	}
}
