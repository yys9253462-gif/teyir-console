package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// 2026-11-02 生产实测：任一节点在线 IP 不完整时整条订阅跳过设备上限，
// 19 条限额订阅里常有 7 条处于「永不检查」状态。不完整只会少算 IP，
// 已确认的超额仍要拦；但不完整的一轮不能解除拦截。

const dlNodeA = "00000000-0000-0000-0000-000000000301"
const dlNodeB = "00000000-0000-0000-0000-000000000302"

type ipDenyRecorder struct {
	mu    sync.Mutex
	calls [][]string
	down  map[string]bool // host → 模拟这台节点不可达
}

func (r *ipDenyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var in struct {
		IPs []string `json:"ips"`
	}
	_ = json.NewDecoder(req.Body).Decode(&in)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down[req.URL.Hostname()] {
		return nil, errors.New("node unreachable")
	}
	r.calls = append(r.calls, in.IPs)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

func (r *ipDenyRecorder) setDown(host string, down bool) {
	r.mu.Lock()
	if r.down == nil {
		r.down = map[string]bool{}
	}
	r.down[host] = down
	r.mu.Unlock()
}

func (r *ipDenyRecorder) take() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func resetDeviceLimitState() {
	deviceLimits.mu.Lock()
	deviceLimits.loaded = false
	deviceLimits.firstSeen = map[string]map[string]time.Time{}
	deviceLimits.lastSeen = map[string]map[string]time.Time{}
	deviceLimits.recorded = map[string]time.Time{}
	deviceLimits.pushed = map[string]deviceLimitPush{}
	deviceLimits.blocked = map[string]int{}
	deviceLimits.warned = map[string]time.Time{}
	deviceLimits.mu.Unlock()
}

func TestDeviceLimitEnforcesConfirmedExcessWhenANodeIsIncomplete(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	resetDeviceLimitState()
	t.Cleanup(resetDeviceLimitState)
	for _, q := range []string{
		`CREATE TABLE subscription_live_connections(subscription_id uuid,node_id uuid,user_id text,ips jsonb DEFAULT '[]',ok boolean DEFAULT false,error text DEFAULT '',hysteria_ok boolean DEFAULT false,reality_ok boolean DEFAULT false,reality_error text DEFAULT '',collected_at timestamptz DEFAULT now(),PRIMARY KEY(subscription_id,node_id))`,
		`CREATE TABLE subscription_ip_admissions(subscription_id uuid NOT NULL, ip text NOT NULL, first_seen timestamptz NOT NULL, PRIMARY KEY(subscription_id, ip))`,
		`INSERT INTO servers(id) VALUES('` + fixHost + `')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []struct{ id, url string }{{dlNodeA, "https://8.8.8.8:8443"}, {dlNodeB, "https://8.8.4.4:8443"}} {
		if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,$1,$2,$3,'fixture.test',443,$4::uuid)`, n.id, n.url, cipher, fixHost); err != nil {
			t.Fatal(err)
		}
	}
	accounts := `{"` + dlNodeA + `":{"user_id":"sub_fixture","password":"p"},"` + dlNodeB + `":{"user_id":"sub_fixture","password":"p"}}`
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,order_id,expires_at,ip_limit,node_accounts) VALUES($1::uuid,'fixture-token-for-customer-only','fixture-order',now()+interval '30 days',1,$2::jsonb)`, fixSub, accounts); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	rec := &ipDenyRecorder{}
	portalClient = &http.Client{Transport: rec}
	t.Cleanup(func() { portalClient = old })

	now := time.Now().Unix()
	setLive := func(node string, ok bool, ips ...string) {
		t.Helper()
		var entries []map[string]any
		for _, ip := range ips {
			entries = append(entries, map[string]any{"ip": ip, "protocol": "Hy2", "last_seen": now})
		}
		raw, _ := json.Marshal(entries)
		if entries == nil {
			raw = []byte("[]")
		}
		if _, err := db.Exec(ctx, `INSERT INTO subscription_live_connections(subscription_id,node_id,user_id,ips,ok,collected_at) VALUES($1::uuid,$2::uuid,'sub_fixture',$3::jsonb,$4,now())
 ON CONFLICT (subscription_id,node_id) DO UPDATE SET ips=EXCLUDED.ips, ok=EXCLUDED.ok, collected_at=now()`, fixSub, node, string(raw), ok); err != nil {
			t.Fatal(err)
		}
	}
	// 先占名额的 IP：名额顺序按 first_seen，固定下来避免两个 IP 同一时刻入场。
	if _, err := db.Exec(ctx, `INSERT INTO subscription_ip_admissions VALUES($1::uuid,'198.51.100.1',now()-interval '1 hour')`, fixSub); err != nil {
		t.Fatal(err)
	}

	// 1. 节点 B 数据不完整，节点 A 已确认 2 个**不同网络**的 IP，上限 1：仍要拦第二个。
	// 2026-11-06：名额按网络算（IPv4 /24）—— 两个地址必须在不同 /24，否则它们只占一个名额、
	// 本来就不该产生拦截（同一 Wi-Fi / 同一次换 IP 的多地址不算多设备）。
	setLive(dlNodeA, true, "198.51.100.1", "203.0.113.5")
	setLive(dlNodeB, false)
	s.checkDeviceLimits(ctx, "enforce")
	calls := rec.take()
	if len(calls) != 2 {
		t.Fatalf("partial telemetry must still push to both nodes, got %v", calls)
	}
	for _, c := range calls {
		if strings.Join(c, ",") != "203.0.113.5" {
			t.Fatalf("want deny 203.0.113.5, got %v", c)
		}
	}

	// 2. 仍不完整，超额 IP 本轮没出现在已确认数据里：不能解除拦截。
	setLive(dlNodeA, true, "198.51.100.1")
	s.checkDeviceLimits(ctx, "enforce")
	for _, c := range rec.take() {
		if len(c) == 0 {
			t.Fatal("partial telemetry must never lift an existing block")
		}
	}

	// 3. 数据恢复完整且回到名额内：解除拦截。
	setLive(dlNodeB, true)
	s.checkDeviceLimits(ctx, "enforce")
	calls = rec.take()
	if len(calls) != 2 || len(calls[0]) != 0 || len(calls[1]) != 0 {
		t.Fatalf("complete telemetry within limit must clear the block on both nodes, got %v", calls)
	}

	// 4.（2026-11-03）超额时一台节点不可达：另一台已拦下，必须记账，
	//    回到名额内时仍要去解除那台已拦的节点（原来不记账 ⇒ 永远不发解除）。
	rec.setDown("8.8.4.4", true)
	// 同样要两个不同 /24（名额按网络算，见上面第 1 段）。
	setLive(dlNodeA, true, "198.51.100.1", "203.0.113.5")
	setLive(dlNodeB, true)
	s.checkDeviceLimits(ctx, "enforce")
	if calls = rec.take(); len(calls) != 1 || strings.Join(calls[0], ",") != "203.0.113.5" {
		t.Fatalf("the reachable node must get the block, got %v", calls)
	}
	if deviceLimitBlockedCount(fixSub) != 1 {
		t.Fatal("a block applied on at least one node must be recorded")
	}
	setLive(dlNodeA, true, "198.51.100.1")
	s.checkDeviceLimits(ctx, "enforce")
	if calls = rec.take(); len(calls) != 1 || len(calls[0]) != 0 {
		t.Fatalf("back within limit: the node that applied the block must be cleared, got %v", calls)
	}
	// 解除也只送达了一台：下一轮还要再发，直到全部节点确认。
	rec.setDown("8.8.4.4", false)
	s.checkDeviceLimits(ctx, "enforce")
	if calls = rec.take(); len(calls) != 2 || len(calls[0]) != 0 || len(calls[1]) != 0 {
		t.Fatalf("an unblock that only reached some nodes must be retried on all, got %v", calls)
	}
	s.checkDeviceLimits(ctx, "enforce")
	if calls = rec.take(); len(calls) != 0 {
		t.Fatalf("once every node confirmed the unblock nothing more is sent, got %v", calls)
	}

	// 5.（2026-11-03）超额后把上限改成不限：订阅退出查询结果，必须立即解除。
	// 同样是两个不同 /24（名额按网络算）。
	setLive(dlNodeA, true, "198.51.100.1", "203.0.113.5")
	s.checkDeviceLimits(ctx, "enforce")
	rec.take()
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET ip_limit=0 WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	s.checkDeviceLimits(ctx, "enforce")
	if calls = rec.take(); len(calls) != 2 || len(calls[0]) != 0 || len(calls[1]) != 0 {
		t.Fatalf("raising the limit to unlimited must clear existing blocks, got %v", calls)
	}
	if deviceLimitBlockedCount(fixSub) != 0 {
		t.Fatal("delivery page must stop reporting blocked IPs once the limit is lifted")
	}
}

type ipDenyTooOld struct{}

func (ipDenyTooOld) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"API endpoint not found"}`)), Header: make(http.Header)}, nil
}

// 2026-11-04：所有节点都因门户过旧跳过拦截时，不能当成「已拦截」记账，交付页也不能显示拦截数。
func TestDeviceLimitDenyNotRecordedWhenEveryNodeSkipped(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	resetDeviceLimitState()
	t.Cleanup(resetDeviceLimitState)
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'old','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, dlNodeA, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	accounts := `{"` + dlNodeA + `":{"user_id":"sub_fixture","password":"p"}}`
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,order_id,expires_at,ip_limit,node_accounts) VALUES($1::uuid,'fixture-token-for-customer-only','fixture-order',now()+interval '30 days',1,$2::jsonb)`, fixSub, accounts); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	portalClient = &http.Client{Transport: ipDenyTooOld{}}
	t.Cleanup(func() { portalClient = old })
	if applied, _ := s.pushDeviceLimitDeny(ctx, fixSub, []string{"198.51.100.2"}); applied {
		t.Fatal("a deny list that no node accepted must not count as applied")
	}
	if applied, complete := s.pushDeviceLimitDeny(ctx, fixSub, nil); !applied || !complete {
		t.Fatal("clearing on nodes that cannot block anyway is a no-op and must count as done")
	}
}

type slowIPDeny struct {
	delay time.Duration
	mu    sync.Mutex
	n     int
}

func (s *slowIPDeny) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-time.After(s.delay):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

// 2026-11-04：一台慢节点挂着多条限额订阅时，下发要并发，不能让后面的任务全部耗尽整轮预算。
func TestDeviceLimitPushesRunInParallel(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	resetDeviceLimitState()
	t.Cleanup(resetDeviceLimitState)
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'slow','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, dlNodeA, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	var jobs []pushJob
	for i := 0; i < 6; i++ {
		id := "00000000-0000-0000-0000-00000000040" + string(rune('0'+i))
		accounts := `{"` + dlNodeA + `":{"user_id":"sub_` + id[len(id)-3:] + `","password":"p"}}`
		if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,order_id,expires_at,ip_limit,node_accounts) VALUES($1::uuid,$2,$3,now()+interval '30 days',1,$4::jsonb)`, id, "fixture-token-slow-"+id, "o-"+id, accounts); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, pushJob{subID: id, ips: []string{"198.51.100.9"}})
	}
	slow := &slowIPDeny{delay: time.Second}
	old := portalClient
	portalClient = &http.Client{Transport: slow}
	t.Cleanup(func() { portalClient = old })
	runCtx, cancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer cancel()
	s.runDeviceLimitPushes(runCtx, jobs, time.Now())
	for _, j := range jobs {
		if deviceLimitBlockedCount(j.subID) != 1 {
			t.Fatalf("every push must complete within the round budget; %s was not recorded (%d calls done)", j.subID, slow.n)
		}
	}
}
