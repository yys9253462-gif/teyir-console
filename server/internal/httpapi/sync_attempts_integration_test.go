package httpapi

import (
	"context"
	"errors"
	"testing"
)

// 2026-11-03：attempts 原来成功也 +1、永不清零 ⇒ 成功同步过多次的订阅，第一次真失败就退避 15 分钟。
func TestSyncAttemptsResetOnSuccess(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	if _, err := db.Exec(ctx, `INSERT INTO subscription_sync_jobs(subscription_id,attempts) VALUES ($1::uuid,40)
ON CONFLICT(subscription_id) DO UPDATE SET attempts=40`, fixSub); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.finishSubscriptionSyncStatus(fixSub, nil, "")
	var attempts int
	if err := db.QueryRow(ctx, `SELECT attempts FROM subscription_sync_jobs`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("successful sync must reset attempts, got %d", attempts)
	}
	s.finishSubscriptionSyncStatus(fixSub, errors.New("node down"), "")
	var backoff float64
	if err := db.QueryRow(ctx, `SELECT attempts, extract(epoch from next_attempt - now()) FROM subscription_sync_jobs`).Scan(&attempts, &backoff); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || backoff > 60 {
		t.Fatalf("first failure after success must back off ~30s, got attempts=%d backoff=%.0fs", attempts, backoff)
	}
}
