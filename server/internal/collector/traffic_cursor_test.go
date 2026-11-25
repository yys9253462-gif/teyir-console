package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRemoteSnapshotScriptIsReadOnlyAndQuotesPaths(t *testing.T) {
	command := fmt.Sprintf(trafficSnapshotScript, shellArgument("/tmp/a'b"), shellArgument("https://example.invalid"))
	if strings.Contains(command, "tmp.replace(p)") || strings.Contains(command, "write_text(") {
		t.Fatal("remote probe must not create or advance a cursor")
	}
	if !strings.Contains(command, "'/tmp/a'\"'\"'b'") || !strings.Contains(command, "'bootstrap':old") {
		t.Fatal("missing read-only bootstrap")
	}
	if !strings.Contains(command, "time.time_ns()") {
		t.Fatal("missing observation ordering")
	}
}

func TestTrafficCursorCommitReplayAndBootstrap(t *testing.T) {
	db := auditCollectorDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE hysteria_nodes(id uuid PRIMARY KEY);
 CREATE TABLE traffic_samples(node_id uuid NOT NULL,sampled_at timestamptz NOT NULL,rx_bytes bigint NOT NULL,tx_bytes bigint NOT NULL,source text NOT NULL,PRIMARY KEY(node_id,sampled_at,source));`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/060_collector_traffic_cursors.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	const node = "00000000-0000-0000-0000-000000000001"
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes VALUES($1::uuid)`, node); err != nil {
		t.Fatal(err)
	}
	c := &Collector{db: db}
	sample := func(epoch string, n int, tx, rx int64, bootstrap string) string {
		cursor := trafficCursor{Epoch: epoch, Counters: map[string]trafficCounter{"u": {Tx: tx, Rx: rx}}}
		if n == 0 {
			cursor.Counters = map[string]trafficCounter{}
		}
		var boot *trafficCursor
		if bootstrap != "" {
			boot = &trafficCursor{Epoch: epoch, Counters: map[string]trafficCounter{"u": {Tx: 100, Rx: 200}}}
		}
		raw, _ := json.Marshal(trafficSnapshot{trafficCursor: cursor, ObservedNS: int64(n), Bootstrap: boot})
		return string(raw)
	}
	nodes := []nodeRow{{id: node, port: 443}}
	// A legacy cursor is the baseline, not historical traffic to replay.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 1, 150, 250, "legacy")); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 2, 170, 290, "")); err != nil {
		t.Fatal(err)
	}
	// Identical observation replay after an unobservable commit must be a no-op.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 2, 170, 290, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 70, 90)
	// A database failure before commit preserves the cursor and lets retry add exactly once.
	if _, err := db.Exec(ctx, `ALTER TABLE traffic_samples RENAME TO samples_unavailable`); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 3, 180, 310, "")); err == nil {
		t.Fatal("failed sample must not commit cursor")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE samples_unavailable RENAME TO traffic_samples`); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 3, 180, 310, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 80, 110)
	// Concurrent replay serializes on the row lock and does not overcount.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 4, 190, 330, "")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	assertTrafficTotals(t, db, node, 90, 130)
	// Fail at COMMIT after both the bucket and cursor UPDATE have executed.
	if _, err := db.Exec(ctx, `CREATE FUNCTION reject_cursor_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_cursor_commit AFTER UPDATE ON collector_traffic_cursors DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_cursor_commit()`); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 5, 195, 335, "")); err == nil {
		t.Fatal("deferred failure must fail commit")
	}
	assertTrafficTotals(t, db, node, 90, 130)
	if _, err := db.Exec(ctx, `DROP TRIGGER reject_cursor_commit ON collector_traffic_cursors`); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 5, 195, 335, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 95, 135)
	// Re-fetching unchanged counters and replaying an older epoch are both idempotent.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("a", 6, 195, 335, "")); err != nil {
		t.Fatal(err)
	}
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("older", 4, 999, 999, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 95, 135)
	// A restart with new users starts each new counter at its absolute current value.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://node/traffic", sample("b", 7, 8, 9, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 103, 144)
	var observed int64
	if err := db.QueryRow(ctx, `SELECT observed_ns FROM collector_traffic_cursors WHERE node_id=$1`, node).Scan(&observed); err != nil || observed != 7 {
		t.Fatalf("cursor=%d err=%v", observed, err)
	}
	// An unmatched port returns before reading or initializing the database.
	if err := c.storeTrafficSnapshot(ctx, nodes, 0, "http://node/traffic", sample("b", 8, 99, 99, "")); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT observed_ns FROM collector_traffic_cursors WHERE node_id=$1`, node).Scan(&observed); err != nil || observed != 7 {
		t.Fatalf("unmatched advanced cursor=%d err=%v", observed, err)
	}
	// No old cursor: preserve the old first-sample zero-baseline semantics.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://other/traffic", sample("a", 1, 7, 11, "")); err != nil {
		t.Fatal(err)
	}
	assertTrafficTotals(t, db, node, 103, 144)
	// Malformed bootstrap cannot seed an apparently successful cursor.
	if err := c.storeTrafficSnapshot(ctx, nodes, 443, "http://bad/traffic", `{"epoch":"a","counters":{"u":{"tx":1,"rx":2}},"observed_ns":1,"bootstrap":{"counters":{}}}`); err == nil {
		t.Fatal("invalid bootstrap accepted")
	}
}

func assertTrafficTotals(t *testing.T, db *pgxpool.Pool, node string, tx, rx int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var gotTx, gotRx int64
	if err := db.QueryRow(ctx, `SELECT sum(tx_bytes),sum(rx_bytes) FROM traffic_samples WHERE node_id=$1 AND source='hysteria_traffic'`, node).Scan(&gotTx, &gotRx); err != nil {
		t.Fatal(err)
	}
	if gotTx != tx || gotRx != rx {
		t.Fatalf("traffic totals tx/rx=%d/%d want %d/%d", gotTx, gotRx, tx, rx)
	}
}

func TestRemoteSnapshotExecutesWithoutAdvancingLegacyCursor(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python required for embedded probe regression")
	}
	script := strings.SplitN(trafficSnapshotScript, "\n", 2)[1]
	script = strings.TrimSuffix(script, "METERPY\n")
	harness := `import io,json,sys
from unittest.mock import patch,Mock
script=sys.argv[1]
legacy={"epoch":"epoch-a","counters":{"u":{"tx":100,"rx":200}}}
def read_text(path):
 return 'secret' if str(path)=='/fixture-secret' else json.dumps(legacy)
def urlopen(*args,**kwargs):
 response=Mock()
 response.__enter__=Mock(return_value=io.StringIO('{"u":{"tx":150,"rx":250}}'))
 response.__exit__=Mock(return_value=False)
 return response
with patch('pathlib.Path.read_text',read_text),patch('pathlib.Path.exists',return_value=True),patch('pathlib.Path.write_text',side_effect=AssertionError('remote write')),patch('subprocess.check_output',return_value=b'epoch-a'),patch('urllib.request.urlopen',urlopen),patch('sys.argv',['probe','/fixture-secret','http://example.invalid']),patch('time.time_ns',return_value=12345):
 exec(compile(script,'remote-probe','exec'))
`
	out, err := exec.Command(python, "-I", "-c", harness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("remote snapshot: %v %s", err, out)
	}
	var snapshot trafficSnapshot
	if err := json.Unmarshal([]byte(parseKeyValues(string(out))["TRAFFIC"]), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Counters["u"].Tx != 150 || snapshot.Bootstrap == nil || snapshot.Bootstrap.Counters["u"].Tx != 100 || snapshot.ObservedNS != 12345 {
		t.Fatalf("snapshot/bootstrap changed: %+v", snapshot)
	}
}
