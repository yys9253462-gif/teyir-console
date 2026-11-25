package httpapi

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"os"
	"testing"
	"time"
)

// 仅在显式登记的隔离数据库运行，绝不默认修改生产库。
func TestIsolatedUsageAndLeaseRegression(t *testing.T) {
	if os.Getenv("NODEOPS_ISOLATED_TEST_DB") != "1" {
		t.Skip("requires explicit isolated PostgreSQL")
	}
	schema := fmt.Sprintf("nodeops_regression_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA ` + schema)
	defer db.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	exec(`CREATE TABLE subscription_measured_usage(subscription_id uuid,node_id uuid,user_id text,measured_bytes bigint,started_at timestamptz,quality_ok bool,error text,collected_at timestamptz,sources jsonb,gaps jsonb,current_ok bool,online_devices bigint)`)
	exec(`CREATE TABLE subscription_usage(subscription_id uuid,node_id uuid,user_id text,used_bytes bigint,limit_bytes bigint,ok bool)`)
	exec(`CREATE TABLE hysteria_nodes(id uuid,name text,deleted_at timestamptz)`)
	exec(`CREATE TABLE subscription_online_ips(subscription_id uuid,node_id uuid,ip text,last_seen bigint,collected_at timestamptz)`)
	exec(`CREATE TABLE portal_update_batches(id uuid,status text,updated_at timestamptz,lease_expires_at timestamptz)`)
	sub := "11111111-1111-4111-8111-111111111111"
	n1 := "22222222-2222-4222-8222-222222222222"
	n2 := "33333333-3333-4333-8333-333333333333"
	rec := subscriptionRecord{ID: sub, nodeAccounts: map[string]subscriptionNodeAccount{n1: {UserID: "u1"}, n2: {UserID: "u2"}}}
	s := &Server{db: db, logger: slog.Default()}
	read := func() subscriptionUsageAgg { return s.loadSubscriptionUsage(ctx, []subscriptionRecord{rec})[sub] }
	if got := read(); got.UsedBytes != nil || got.CurrentHealthy {
		t.Fatalf("unknown became zero/healthy: %+v", got)
	}
	sources := fmt.Sprintf(`{"hysteria":{"ok":true,"sampled_at":%q}}`, time.Now().UTC().Format(time.RFC3339Nano))
	for i, node := range []string{n1, n2} {
		used := int64(i+2) * 1048576
		exec(`INSERT INTO subscription_measured_usage VALUES($1,$2,$3,$4,now(),true,'',now(),$5::jsonb,'[]',true,1)`, sub, node, fmt.Sprintf("u%d", i+1), used, sources)
		// 模拟旧投影仍把真实值记成历史：真实聚合必须仍只计算一次。
		exec(`INSERT INTO subscription_usage VALUES($1,$2,$3,$4,$5,true)`, sub, node, fmt.Sprintf("u%d", i+1), used, (i+1)*100)
	}
	got := read()
	if got.UsedBytes == nil || *got.UsedBytes != 5*1048576 || got.LimitBytes == nil || *got.LimitBytes != 300 || got.HysteriaOnlineDevices == nil || *got.HysteriaOnlineDevices != 2 {
		t.Fatalf("wrong totals/limits: %+v", got)
	}
	exec(`UPDATE subscription_measured_usage SET measured_bytes=0`)
	if got = read(); got.UsedBytes == nil || *got.UsedBytes != 0 {
		t.Fatal("measured zero not retained")
	}
	// 2026-11-03：一台节点的配额采集失败（ok=false 不在结果里）时，总上限只能是「未知」，
	// 不能把另一台的 200 当成订阅总上限。
	exec(`UPDATE subscription_usage SET ok=false WHERE node_id=$1`, n1)
	if got = read(); got.LimitBytes != nil {
		t.Fatalf("partial limit presented as total: %d", *got.LimitBytes)
	}
	// 2026-11-06：采集器现在会在没采到时把行标 ok=false（不动 used_bytes）。历史用量是累计值，照常计入。
	if got = read(); got.LegacyBytes != 5*1048576 {
		t.Fatalf("legacy history dropped when one node's latest round failed: %d", got.LegacyBytes)
	}
	exec(`UPDATE subscription_usage SET ok=true`)
	exec(`UPDATE subscription_usage SET limit_bytes=0 WHERE node_id=$1`, n1)
	if got = read(); got.LimitBytes == nil || *got.LimitBytes != 0 {
		t.Fatal("unlimited lost")
	}
	exec(`UPDATE subscription_measured_usage SET quality_ok=false,gaps='[{"reason":"restart"}]'`)
	if got = read(); got.OK || !got.CurrentHealthy || got.HistoricalGaps != 2 {
		t.Fatalf("historical gap treated as current failure: %+v", got)
	}
	exec(`UPDATE subscription_measured_usage SET collected_at=now()-interval '2 minutes' WHERE node_id=$1`, n1)
	if read().CurrentHealthy {
		t.Fatal("stale node hidden by other fresh node")
	}
	// 2026-11-03：一台节点陈旧时 Hy2 连接数只能是「未知」，不能把另一台的数当总数。
	if got = read(); got.HysteriaOnlineDevices != nil {
		t.Fatalf("partial Hy2 online count presented as total: %d of %d nodes", got.OnlineCountNodes, got.ExpectedNodes)
	}
	exec(`INSERT INTO hysteria_nodes VALUES($1,'node',null)`, n1)
	exec(`INSERT INTO subscription_online_ips VALUES($1,$2,'8.8.8.8',extract(epoch from now())::bigint-200,now())`, sub, n1)
	ips, err := s.loadSubscriptionOnlineIPs(ctx, []subscriptionRecord{rec})
	if err != nil || len(ips[sub]) != 0 {
		t.Fatal("stale online IP visible", err)
	}
	exec(`UPDATE subscription_online_ips SET last_seen=extract(epoch from now())::bigint`)
	ips, err = s.loadSubscriptionOnlineIPs(ctx, []subscriptionRecord{rec})
	if err != nil || len(ips[sub]) != 1 {
		t.Fatal("fresh recent-auth IP lost", err)
	}
	exec(`INSERT INTO portal_update_batches VALUES($1,'running',now(),now()-interval '1 second')`, sub)
	exec(`INSERT INTO portal_update_batches VALUES($1,'running',now(),now()+interval '3 minutes')`, n1)
	s.expirePortalBatches(ctx)
	var old, fresh string
	db.QueryRow(ctx, `SELECT status FROM portal_update_batches WHERE id=$1`, sub).Scan(&old)
	db.QueryRow(ctx, `SELECT status FROM portal_update_batches WHERE id=$1`, n1).Scan(&fresh)
	if old != "interrupted" || fresh != "running" {
		t.Fatal("lease recovery interrupted active task", old, fresh)
	}
	t.Log("PASS: unknown vs zero, cross-node real-only totals, legacy non-inclusion, quotas/unlimited, current health vs historical gaps, stale IP TTL, expired/fresh leases")
}
