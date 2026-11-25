package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// 2026-10-21 事故：韩国-首尔被「故障节点自动下线」判离线 12 小时（订阅里早已不下发它），
// 但交付判定仍要求它开户成功，于是整单卡在 pending，商城报交付失败。
// 规则：缺口只落在已自动下线的节点、且有健康节点已交付时，算 degraded（对商城=已交付），
// 并继续重试；节点恢复后补齐为 synced。

const degHealthy = "00000000-0000-0000-0000-000000000201"
const degDown = "00000000-0000-0000-0000-000000000202"

// reconcileSubscriptionAccounts 按节点并发调用 portal，RoundTrip 会被多个
// goroutine 同时进入，读写 down/expiry 必须持锁（-race 下否则直接 fatal）。
type hostTransport struct {
	mu     sync.Mutex
	down   map[string]bool
	expiry map[string]int64
}

func (h *hostTransport) setDown(down map[string]bool) {
	h.mu.Lock()
	h.down = down
	h.mu.Unlock()
}

func (h *hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down[host] {
		return nil, errors.New("isolated node unavailable")
	}
	body := ""
	if r.Method == "POST" {
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		if v, ok := input["expires_at"].(float64); ok {
			h.expiry[host] = int64(v)
		}
		body = `{"ok":true}`
	} else {
		body = fmt.Sprintf(`{"ok":true,"user":{"expires_at":%d}}`, h.expiry[host])
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func resetHealthGate() {
	healthGateCache.Lock()
	healthGateCache.at = time.Time{}
	healthGateCache.nodes = nil
	healthGateCache.Unlock()
	// autoHideEnabled 也加了 30 秒缓存；测试之间必须一起清，否则先跑的用例
	// 会把开关值带进下一个用例。
	invalidateAutoHideCache()
}

func syncStatus(t *testing.T, s *Server) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(context.Background(), `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestDegradedDeliveryWhenOnlyAutoHiddenNodesFail(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ id, name, url string }{{degHealthy, "healthy", "https://8.8.8.8:8443"}, {degDown, "down", "https://8.8.4.4:8443"}} {
		if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,$2,$3,$4,'fixture.test',443,$5::uuid)`, n.id, n.name, n.url, cipher, fixHost); err != nil {
			t.Fatal(err)
		}
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old; resetHealthGate() })
	tr := &hostTransport{down: map[string]bool{"8.8.4.4": true}, expiry: map[string]int64{}}
	portalClient = &http.Client{Transport: tr}
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://panel.example/api/store/orders?order_id=fixture-order", nil)

	// 1. 节点刚断、还没被判下线：仍按未完成处理（结果未知，不能假装交付）。
	resetHealthGate()
	if err = s.synchronizeSubscription(ctx, rec); err == nil {
		t.Fatal("failure on a node not yet auto-hidden must stay pending")
	}
	if got := syncStatus(t, s); got != "pending" {
		t.Fatal("want pending, got", got)
	}

	// 2. 故障节点已被自动下线超过 5 分钟：健康节点已交付即算交付成功（degraded）。
	if _, err = db.Exec(ctx, `INSERT INTO alerts(node_id,type,status,first_seen_at) VALUES($1::uuid,'node_offline','open',now()-interval '10 minutes')`, degDown); err != nil {
		t.Fatal(err)
	}
	resetHealthGate()
	if err = s.synchronizeSubscription(ctx, rec); err != nil {
		t.Fatal("degraded delivery reported as failure:", err)
	}
	if got := syncStatus(t, s); got != syncDegraded {
		t.Fatal("want degraded, got", got)
	}
	reply := httptest.NewRecorder()
	s.writeStoreOrder(reply, req, rec)
	if reply.Code != 200 {
		t.Fatal("shop must see degraded as delivered:", reply.Code, reply.Body.String())
	}
	// degraded 仍在重试队列里（节点恢复后要补开户）。
	var queued int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM subscription_sync_jobs WHERE status IN ('pending','degraded')`).Scan(&queued)
	if queued != 1 {
		t.Fatal("degraded job dropped from retry queue")
	}

	// 3. 故障节点恢复：补开户后变 synced。
	tr.setDown(map[string]bool{})
	if err = s.synchronizeSubscription(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got := syncStatus(t, s); got != "synced" {
		t.Fatal("want synced after recovery, got", got)
	}
}

func TestDegradedNeverWithoutAHealthyDeliveredNode(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'down','https://8.8.4.4:8443',$2,'fixture.test',443,$3::uuid)`, degDown, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO alerts(node_id,type,status,first_seen_at) VALUES($1::uuid,'node_offline','open',now()-interval '10 minutes')`, degDown); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old; resetHealthGate() })
	portalClient = &http.Client{Transport: &hostTransport{down: map[string]bool{"8.8.4.4": true}, expiry: map[string]int64{}}}
	resetHealthGate()
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	// 唯一的节点故障：买家没有任何可用节点，绝不能算已交付。
	if err = s.synchronizeSubscription(ctx, rec); err == nil {
		t.Fatal("no healthy node delivered but sync reported success")
	}
	if got := syncStatus(t, s); got != "pending" {
		t.Fatal("want pending, got", got)
	}
}

// 2026-11-03：订阅映射里残留的「已不在范围内」节点不能算已交付。
// 场景：买家订单唯一的节点被取消上架（in_store=false），映射里仍记着它的账号。
// 原来 len(accounts)>0 ⇒ synced，商城报已交付，而 /sub 一个节点都不下发。
func TestOutOfScopeMappingIsNotDelivery(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id,in_store) VALUES($1::uuid,'unlisted','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid,false)`, degHealthy, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=jsonb_build_object($2::text, jsonb_build_object('user_id','sub_x','password','p')) WHERE id=$1::uuid`, fixSub, degHealthy); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old; resetHealthGate() })
	portalClient = &http.Client{Transport: &hostTransport{down: map[string]bool{}, expiry: map[string]int64{}}}
	resetHealthGate()
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.synchronizeSubscription(ctx, rec); err == nil {
		t.Fatal("stale out-of-scope mapping reported as delivered")
	}
	if got := syncStatus(t, s); got != "pending" {
		t.Fatal("want pending, got", got)
	}
}
