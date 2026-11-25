package collector

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

// 2026-11-04：在途的探测结果不能写回已软删的节点（deleteNode 已把它置为 offline）。
func TestProbeResultsSkipSoftDeletedNodes(t *testing.T) {
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for PostgreSQL integration")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("collector_fix_%d", time.Now().UnixNano())
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
	const node = "00000000-0000-0000-0000-0000000c0101"
	if _, err := db.Exec(ctx, `CREATE TABLE hysteria_nodes(id uuid PRIMARY KEY, status text, status_source text, status_error text,
consecutive_probe_failures int NOT NULL DEFAULT 0, last_checked_at timestamptz, updated_at timestamptz, deleted_at timestamptz);
INSERT INTO hysteria_nodes(id,status,deleted_at) VALUES('`+node+`','offline',now())`); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), db, 30, nil)
	c.updateNodeStatus(ctx, node, "online", "api", "", "", true)
	c.updateNodeStatus(ctx, node, "offline", "api", "unreachable", "x", false)
	c.markNodeProbeFailed(ctx, node, "x")
	var status string
	var checked *time.Time
	if err := db.QueryRow(ctx, `SELECT status, last_checked_at FROM hysteria_nodes WHERE id=$1::uuid`, node).Scan(&status, &checked); err != nil {
		t.Fatal(err)
	}
	if status != "offline" || checked != nil {
		t.Fatalf("a soft-deleted node must not receive probe results, got status=%s last_checked_at=%v", status, checked)
	}
}

// 2026-11-04：上报的端口已被同主机另一台在役节点占用时不同步（不再每轮撞唯一索引）；空闲端口照常同步。
func TestListenPortSyncSkipsPortTakenOnSameHost(t *testing.T) {
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for PostgreSQL integration")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("collector_port_%d", time.Now().UnixNano())
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
	const host, a, b = "00000000-0000-0000-0000-0000000c0201", "00000000-0000-0000-0000-0000000c0202", "00000000-0000-0000-0000-0000000c0203"
	if _, err := db.Exec(ctx, `CREATE TABLE hysteria_nodes(id uuid PRIMARY KEY, server_id uuid, listen_port int, updated_at timestamptz, deleted_at timestamptz);
CREATE UNIQUE INDEX ON hysteria_nodes (server_id, listen_port) WHERE deleted_at IS NULL;
INSERT INTO hysteria_nodes(id,server_id,listen_port) VALUES('`+a+`','`+host+`',443),('`+b+`','`+host+`',8443)`); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(slog.New(slog.NewTextHandler(io.Discard, nil)), db, 30, nil)
	port := func(id string) int {
		var p int
		if err := db.QueryRow(ctx, `SELECT listen_port FROM hysteria_nodes WHERE id=$1::uuid`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c.syncReportedListenPort(ctx, a, "a", 8443) // 被 b 占用
	if port(a) != 443 {
		t.Fatalf("a port taken by another node on the same host must not be synced, got %d", port(a))
	}
	c.syncReportedListenPort(ctx, a, "a", 9443) // 空闲
	if port(a) != 9443 {
		t.Fatalf("a free reported port must be synced, got %d", port(a))
	}
	c.syncReportedListenPort(ctx, a, "a", 70000) // 越界
	if port(a) != 9443 {
		t.Fatalf("an out-of-range port must be ignored, got %d", port(a))
	}
}
