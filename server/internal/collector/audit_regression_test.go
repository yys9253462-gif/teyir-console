package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"nodeops/server/internal/secrets"
)

func TestDetachedWriteIsBoundedAfterCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := detachedWriteContext(parent, 30*time.Millisecond)
	defer stop()
	if ctx.Err() != nil {
		t.Fatal("parent cancellation must allow bounded failure recording")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached write has no short deadline")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatal(ctx.Err())
	}
}

func TestUsageFetchReservesPersistenceBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	fetch, stop := usageFetchContext(ctx)
	defer stop()
	fd, _ := fetch.Deadline()
	pd, _ := ctx.Deadline()
	if gap := pd.Sub(fd); gap < 30*time.Millisecond || gap > 60*time.Millisecond {
		t.Fatalf("no useful write reserve: %v", gap)
	}
}

func auditCollectorDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for isolated PostgreSQL integration")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// CI and local runs use different isolated databases. Refuse only names that
	// do not look disposable; every table below is created in a unique schema.
	if !strings.Contains(cfg.ConnConfig.Database, "test") && !strings.Contains(cfg.ConnConfig.Database, "audit") {
		t.Fatalf("refusing non-test database %q for destructive audit tests", cfg.ConnConfig.Database)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("collector_audit_%d", time.Now().UnixNano())
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		base.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		base.Close()
	})
	return db
}

func TestUsageProjectionPreservesInvalidMappingSemantics(t *testing.T) {
	db := auditCollectorDB(t)
	ctx := context.Background()
	_, err := db.Exec(ctx, `CREATE TABLE subscription_tokens(id uuid,node_accounts jsonb,deleted_at timestamptz);
 CREATE TABLE servers(id uuid,name text,deleted_at timestamptz);
 CREATE TABLE hysteria_nodes(id uuid,server_id uuid,api_url text,api_key bytea,deleted_at timestamptz);
 INSERT INTO servers VALUES('00000000-0000-0000-0000-000000000001','test',NULL);
 INSERT INTO hysteria_nodes VALUES('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001','http://example.invalid','k',NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	const node = "00000000-0000-0000-0000-000000000002"
	for _, tc := range []struct {
		account, extra string
		want           bool
	}{
		{`{"user_id":"u","password":"unused"}`, ``, true},
		{`{"USER_ID":"u"}`, ``, true},
		{`{"user_id":null}`, ``, false},
		{`null`, ``, false},
		{`{"user_id":123}`, ``, false},
		{`{"user_id":"u"}`, `,"00000000-0000-0000-0000-000000000003":123`, false},
		{`{"user_id":"u"}`, `,"00000000-0000-0000-0000-000000000003":{"user_id":false}`, false},
		{`{"user_id":"u"}`, `,"00000000-0000-0000-0000-000000000003":null`, true},
	} {
		_, err := db.Exec(ctx, `TRUNCATE subscription_tokens;`)
		if err != nil {
			t.Fatal(err)
		}
		raw := `{"` + node + `":` + tc.account + tc.extra + `}`
		if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens VALUES('00000000-0000-0000-0000-000000000004',$1::jsonb,NULL)`, raw); err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query(ctx, usageMappingSQL)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := false
		for rows.Next() {
			var id, url, label, n string
			var account, key []byte
			if err := rows.Scan(&id, &account, &url, &key, &label, &n); err != nil {
				t.Fatal(err)
			}
			var a subscriptionNodeAccount
			if json.Unmarshal(account, &a) == nil && a.UserID != "" {
				got = true
			}
			if strings.Contains(string(account), node) {
				t.Fatal("transferred entire mapping")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("mapping %s got=%v want=%v", raw, got, tc.want)
		}
	}
}

func TestUsageKeepsHealthyPrefetchAtDeadline(t *testing.T) {
	// A local HTTP fixture verifies actual deadline cancellation and retained successful data.
	orig := usageClient.Transport
	usageClient.Transport = &http.Transport{DialContext: testDial(), DisableKeepAlives: true}
	defer func() { usageClient.Transport = orig }()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := box.Seal([]byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"users":[{"user_id":"u","traffic_used_bytes":42}]}`)
	}))
	defer healthy.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	c := &Collector{box: box, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	fetch, stop := usageFetchContext(ctx)
	perNode := map[string]map[string]portalUserUsage{}
	failures := map[string]string{}
	c.prefetchNodeUsers(fetch, []usageNodeTarget{{nodeID: "healthy", apiURL: healthy.URL, apiKey: key}, {nodeID: "slow", apiURL: slow.URL, apiKey: key}}, perNode, failures)
	stop()
	if perNode["healthy"]["u"].TrafficUsed != 42 || perNode["slow"] != nil || failures["slow"] == "" {
		t.Fatalf("healthy result lost: %v %v", perNode, failures)
	}
	write, stopWrite := detachedWriteContext(ctx, 5*time.Second)
	defer stopWrite()
	if write.Err() != nil {
		t.Fatal("successful prefetch cannot be persisted")
	}
}

func TestDetachedWritePoolWaitIsBounded(t *testing.T) {
	db := auditCollectorDB(t)
	cfg := db.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, stop := detachedWriteContext(parent, 50*time.Millisecond)
	defer stop()
	start := time.Now()
	if _, err := pool.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("occupied pool should time out")
	}
	if time.Since(start) > time.Second {
		t.Fatal("detached database wait exceeded bound")
	}
}

func TestSubscriptionUsageStoresHealthyNodeWhenPeerTimesOut(t *testing.T) {
	db := auditCollectorDB(t)
	ctx := context.Background()
	_, err := db.Exec(ctx, `CREATE TABLE subscription_tokens(id uuid,node_accounts jsonb,deleted_at timestamptz);
 CREATE TABLE servers(id uuid,name text,deleted_at timestamptz);
 CREATE TABLE hysteria_nodes(id uuid,server_id uuid,api_url text,api_key bytea,deleted_at timestamptz);
 CREATE TABLE subscription_usage(subscription_id uuid,node_id uuid,user_id text,used_bytes bigint,limit_bytes bigint,ok boolean,error text,sampled_at timestamptz,PRIMARY KEY(subscription_id,node_id));
 CREATE TABLE subscription_measured_usage(subscription_id uuid,node_id uuid,user_id text,measured_bytes bigint,started_at timestamptz,quality_ok boolean,error text,sources jsonb,gaps jsonb,collected_at timestamptz,current_ok boolean,online_devices bigint,PRIMARY KEY(subscription_id,node_id));
 CREATE TABLE subscription_online_ips(subscription_id uuid,node_id uuid,ip text,last_seen bigint,collected_at timestamptz,PRIMARY KEY(subscription_id,node_id,ip));
 CREATE TABLE subscription_live_connections(subscription_id uuid,node_id uuid,user_id text,ips jsonb,ok boolean,error text,hysteria_ok boolean,reality_ok boolean,reality_error text,collected_at timestamptz,PRIMARY KEY(subscription_id,node_id));
 INSERT INTO servers VALUES('00000000-0000-0000-0000-000000000001','fixture',NULL);
 INSERT INTO subscription_tokens VALUES('00000000-0000-0000-0000-000000000004','{"00000000-0000-0000-0000-000000000002":{"user_id":"u"},"00000000-0000-0000-0000-000000000003":{"user_id":"u"}}',NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	orig := usageClient.Transport
	usageClient.Transport = &http.Transport{DialContext: testDial(), DisableKeepAlives: true}
	defer func() { usageClient.Transport = orig }()
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := box.Seal([]byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"users":[{"user_id":"u","traffic_used_bytes":42,"measured_bytes":99,"measurement_ok":true,"measurement_started_at":"2026-11-10T00:00:00Z"}]}`)
	}))
	defer healthy.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	for i, url := range []string{healthy.URL, slow.URL} {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+2)
		if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes VALUES($1::uuid,'00000000-0000-0000-0000-000000000001',$2,$3,NULL)`, id, url, key); err != nil {
			t.Fatal(err)
		}
	}
	c := &Collector{box: box, db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	round, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c.collectSubscriptionUsage(round)
	var measured int64
	var quality bool
	if err := db.QueryRow(ctx, `SELECT measured_bytes,quality_ok FROM subscription_measured_usage WHERE node_id='00000000-0000-0000-0000-000000000002'`).Scan(&measured, &quality); err != nil {
		t.Fatal(err)
	}
	if measured != 99 || !quality {
		t.Fatalf("healthy sample lost: measured=%d quality=%v", measured, quality)
	}
	var stale bool
	if err := db.QueryRow(ctx, `SELECT NOT quality_ok FROM subscription_measured_usage WHERE node_id='00000000-0000-0000-0000-000000000003'`).Scan(&stale); err != nil || !stale {
		t.Fatalf("slow peer not stale: %v %v", stale, err)
	}
}
