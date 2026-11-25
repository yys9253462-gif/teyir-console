package executor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClaimQueryDatabaseSerializesServers(t *testing.T) {
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := fmt.Sprintf("executor_claim_%d", time.Now().UnixNano())
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(ctx, `
CREATE TABLE servers(id uuid PRIMARY KEY, host text DEFAULT '', ssh_port int DEFAULT 22, ssh_user text DEFAULT 'root', host_fingerprint text, allow_write_ops bool DEFAULT true, write_units text[] DEFAULT '{}', deleted_at timestamptz);
CREATE TABLE operation_tasks(id uuid PRIMARY KEY,server_id uuid,node_id uuid,status text,created_at timestamptz DEFAULT now(),started_at timestamptz,operation_type text DEFAULT 'health_check',parameters jsonb DEFAULT '{}');
CREATE TABLE host_operation_leases(server_id uuid PRIMARY KEY);
CREATE TABLE server_credentials(server_id uuid,auth_type text,ciphertext bytea,created_at timestamptz);
CREATE TABLE hysteria_nodes(id uuid,listen_port int);
INSERT INTO servers(id) SELECT ('00000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid FROM generate_series(1,5) i;
UPDATE servers SET deleted_at=now() WHERE id='00000000-0000-0000-0000-000000000005';
INSERT INTO operation_tasks(id,server_id,status) VALUES('30000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000005','queued');
INSERT INTO operation_tasks(id,server_id,status) SELECT ('10000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,('00000000-0000-0000-0000-'||lpad(((i+1)/2)::text,12,'0'))::uuid,'queued' FROM generate_series(1,8) i;
INSERT INTO host_operation_leases VALUES('00000000-0000-0000-0000-000000000002');
INSERT INTO operation_tasks(id,server_id,status) VALUES('20000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000003','running');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, claimServersQuery, 4)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	// 2026-11-03：已删除主机（…005）的排队任务也不能被领取。
	if err := rows.Err(); err != nil || len(ids) != 2 {
		t.Fatalf("lease/running/deleted filtering: %v %v", ids, err)
	}
	// 第一事务持锁时，另一领取者不能碰同一主机，即使排队任务各有两条。
	second, err := db.Query(ctx, claimServersQuery, 4)
	if err != nil {
		t.Fatal(err)
	}
	if second.Next() {
		second.Close()
		t.Fatal("another executor acquired the same server")
	}
	second.Close()
	if err := second.Err(); err != nil {
		t.Fatal(err)
	}
	claimed, err := tx.Query(ctx, claimQuery, ids)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	count := 0
	for claimed.Next() {
		values, err := claimed.Values()
		if err != nil {
			t.Fatal(err)
		}
		sid := fmt.Sprint(values[3])
		if seen[sid] {
			t.Fatal("claimed two tasks on one server")
		}
		seen[sid] = true
		count++
	}
	claimed.Close()
	if err := claimed.Err(); err != nil || count != 2 {
		t.Fatalf("claim: count=%d err=%v", count, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = db.Query(ctx, claimServersQuery, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("next batch ignored running tasks")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestClaimQueryOnePerServer 钉住领取规则：同一批每台主机至多一条，
// 且跳过已有 running 任务、或被安装/迁移租约占用的主机。
func TestClaimQueryOnePerServer(t *testing.T) {
	for _, want := range []string{
		"DISTINCT ON (q.server_id)",
		"r.status = 'running'",
		"host_operation_leases l WHERE l.server_id = q.server_id",
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 4",
		"status = 'queued'",
	} {
		if !strings.Contains(claimQuery, want) {
			t.Errorf("claimQuery 缺少 %q", want)
		}
	}
	// 锁必须加在挑选之后，否则单台主机积压的任务会占满候选窗口、饿死其他主机。
	if strings.Index(claimQuery, "FOR UPDATE") < strings.Index(claimQuery, "DISTINCT ON") {
		t.Error("应先按主机去重再加锁")
	}
}
