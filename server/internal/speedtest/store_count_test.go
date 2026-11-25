package speedtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 2026-11-04：ON CONFLICT DO NOTHING 跳过的行不能算「已写入」。
// 生产上的唯一索引按「同一秒」去重；这里把唯一键收窄到 (node_id, target_key)，让第二次写入必然冲突，
// 只测计数逻辑本身，不依赖时钟。
func TestStoreResultsCountsOnlyInsertedRows(t *testing.T) {
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for PostgreSQL integration")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("speedtest_fix_%d", time.Now().UnixNano())
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); base.Close() })
	if _, err := db.Exec(ctx, `CREATE TABLE node_speedtests(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), node_id uuid NOT NULL, server_id uuid NOT NULL,
target_key text NOT NULL, target_name text NOT NULL, target_host text NOT NULL, carrier text NOT NULL,
icmp_avg_ms double precision, icmp_loss_pct double precision, tcp_connect_ms double precision, download_mbps double precision, upload_mbps double precision,
probe_ok boolean NOT NULL DEFAULT false, probe_error text NOT NULL DEFAULT '', measured_at timestamptz NOT NULL DEFAULT now(), round_id uuid NOT NULL);
CREATE UNIQUE INDEX ON node_speedtests (node_id, target_key)`); err != nil {
		t.Fatal(err)
	}
	p := NewProber(slog.New(slog.NewTextHandler(io.Discard, nil)), db, nil)
	const node, server = "00000000-0000-0000-0000-0000000d0001", "00000000-0000-0000-0000-0000000d0002"
	results := []Result{{TargetKey: "telecom-sh", Name: "上海电信", Host: "1.1.1.1", Carrier: "telecom", ProbeOK: true}}
	first, err := p.storeResults(ctx, server, []string{node}, results, "00000000-0000-0000-0000-0000000d0003")
	if err != nil || first != 1 {
		t.Fatalf("first write: want 1 row, got %d err=%v", first, err)
	}
	second, err := p.storeResults(ctx, server, []string{node}, results, "00000000-0000-0000-0000-0000000d0004")
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("a row skipped by ON CONFLICT must not count as written, got %d", second)
	}
	// One target is rejected after a successful insert: preserve actual count and report failure.
	if _, err := db.Exec(ctx, `ALTER TABLE node_speedtests ADD CHECK (target_key <> 'reject')`); err != nil {
		t.Fatal(err)
	}
	partial := []Result{{TargetKey: "new", Name: "ok", Host: "1.1.1.1", Carrier: "other"}, {TargetKey: "reject", Name: "bad", Host: "1.1.1.1", Carrier: "other"}}
	if n, err := p.storeResults(ctx, server, []string{node}, partial, "00000000-0000-0000-0000-0000000d0006"); n != 1 || err == nil {
		t.Fatalf("partial write must return actual inserted count and error: n=%d err=%v", n, err)
	}
	// Retrying an already-written target must not count its conflict as another insert.
	if n, err := p.storeResults(ctx, server, []string{node}, partial, "00000000-0000-0000-0000-0000000d0006"); n != 0 || err == nil {
		t.Fatalf("partial retry: n=%d err=%v", n, err)
	}
	// 2026-11-06：一行都写不进去（写入出错）必须返回错误，不能是 (0, nil)。
	if _, err := db.Exec(ctx, `ALTER TABLE node_speedtests RENAME TO node_speedtests_gone`); err != nil {
		t.Fatal(err)
	}
	if n, err := p.storeResults(ctx, server, []string{node}, results, "00000000-0000-0000-0000-0000000d0005"); err == nil || n != 0 {
		t.Fatalf("a round whose every write failed must be an error, got n=%d err=%v", n, err)
	}
}
