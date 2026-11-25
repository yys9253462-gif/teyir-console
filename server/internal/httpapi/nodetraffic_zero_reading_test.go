package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"nodeops/server/internal/secrets"
)

// 2026-11-12：节点门户在读不到默认路由网卡时回 ok 加 rx/tx 全 0。这样的读数不能当作有效样本：
// 原来基线会被写成 0，下一次正常读数把开机以来的全部流量一次计进本周期，有月限额的节点当场被停。
func TestNodeTrafficIgnoresZeroCounterReading(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const node = "00000000-0000-4000-8000-0000000009a1"
	const server = "00000000-0000-4000-8000-0000000009a2"
	if _, err := db.Exec(ctx, `INSERT INTO servers(id,name) VALUES($1::uuid,'h')`, server); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes
		ADD COLUMN traffic_error text,
		ADD COLUMN traffic_limit_bytes bigint,
		ADD COLUMN traffic_reset_day int DEFAULT 1,
		ADD COLUMN traffic_cycle_start timestamptz,
		ADD COLUMN traffic_cycle_bytes bigint DEFAULT 0,
		ADD COLUMN traffic_counter_rx bigint,
		ADD COLUMN traffic_counter_tx bigint,
		ADD COLUMN traffic_boot_id text,
		ADD COLUMN traffic_sampled_at timestamptz;
	CREATE TABLE traffic_samples(node_id uuid, sampled_at timestamptz, rx_bytes bigint, tx_bytes bigint, source text, UNIQUE(node_id, sampled_at, source))`); err != nil {
		t.Fatal(err)
	}
	// 月限额 10 GB：远小于「开机以来」的 800 GB 累计值。
	const limit = int64(10) << 30
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,server_id,api_url,api_key,traffic_suspended,traffic_limit_bytes)
		VALUES($1::uuid,'n',$2::uuid,'https://8.8.8.8:8443',$3,false,$4)`, node, server, key, limit); err != nil {
		t.Fatal(err)
	}

	var rx, tx int64
	var suspendCalls int
	old := portalClient
	portalClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true}`
		switch {
		case strings.Contains(r.URL.Path, "suspend"):
			suspendCalls++
		case strings.Contains(r.URL.Path, "traffic"):
			ifaces := `["eth0"]`
			if rx == 0 && tx == 0 {
				ifaces = `[]` // 读不到默认路由网卡时门户的真实输出
			}
			body = fmt.Sprintf(`{"ok":true,"rx_bytes":%d,"tx_bytes":%d,"suspended":false,"boot_id":"b1","interfaces":%s}`, rx, tx, ifaces)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { portalClient = old })
	s := fixServer(db)
	s.box = box

	state := func() (counterRx, counterTx, cycle int64, note string, suspended bool) {
		t.Helper()
		if err := db.QueryRow(ctx, `SELECT COALESCE(traffic_counter_rx,-1), COALESCE(traffic_counter_tx,-1), traffic_cycle_bytes, COALESCE(traffic_error,''), traffic_suspended
			FROM hysteria_nodes WHERE id=$1::uuid`, node).Scan(&counterRx, &counterTx, &cycle, &note, &suspended); err != nil {
			t.Fatal(err)
		}
		return
	}

	// 1) 正常读数：开机以来 rx=500GB、tx=300GB。第一次只建基线，不计入。
	const baseRx, baseTx = int64(500) << 30, int64(300) << 30
	rx, tx = baseRx, baseTx
	s.meterNodeByID(ctx, node)
	if cRx, cTx, cycle, _, _ := state(); cRx != baseRx || cTx != baseTx || cycle != 0 {
		t.Fatalf("baseline not established: rx=%d tx=%d cycle=%d", cRx, cTx, cycle)
	}

	// 2) 门户一次性读不到网卡：0 / 0。基线必须保持不动，并留下可见的原因。
	rx, tx = 0, 0
	s.meterNodeByID(ctx, node)
	cRx, cTx, cycle, note, _ := state()
	if cRx != baseRx || cTx != baseTx {
		t.Fatalf("a zero reading overwrote the baseline: rx=%d tx=%d", cRx, cTx)
	}
	if cycle != 0 || note == "" {
		t.Fatalf("a zero reading must not be metered and must leave a visible reason: cycle=%d note=%q", cycle, note)
	}

	// 3) 正常读数回来，这一分钟只多了 1 MB + 2 MB。计入的必须是这 3 MB，而不是开机以来的 800 GB。
	rx, tx = baseRx+(1<<20), baseTx+(2<<20)
	s.meterNodeByID(ctx, node)
	cRx, cTx, cycle, note, suspended := state()
	if cycle != 3<<20 {
		t.Fatalf("after the zero reading the cycle must only grow by the real delta (3 MiB), got %d bytes", cycle)
	}
	if cRx != rx || cTx != tx || note != "" {
		t.Fatalf("normal metering did not resume: rx=%d tx=%d note=%q", cRx, cTx, note)
	}
	if suspended || suspendCalls != 0 {
		t.Fatalf("the node was suspended for traffic it never used: suspended=%v suspend_calls=%d", suspended, suspendCalls)
	}
	var samples int
	var total int64
	if err := db.QueryRow(ctx, `SELECT count(*), COALESCE(sum(rx_bytes+tx_bytes),0) FROM traffic_samples WHERE node_id=$1::uuid`, node).Scan(&samples, &total); err != nil {
		t.Fatal(err)
	}
	if samples != 1 || total != 3<<20 {
		t.Fatalf("want one 3 MiB sample, got %d samples totalling %d bytes", samples, total)
	}
}
