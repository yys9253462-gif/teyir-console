package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 批次六（hy2-ops-console）专项回归测试：验证全部核心缺陷在未修代码上真实报红、修复后全绿。

func TestAudit20261122_DedupeNodeNamesReservesBuiltins(t *testing.T) {
	nodes := dedupeNodeNames([]subscriptionNode{
		{Name: "PROXY", Port: 443},
		{Name: "AUTO", Port: 8443},
		{Name: "DIRECT", Port: 2053},
	})
	for _, n := range nodes {
		if n.Name == "PROXY" || n.Name == "AUTO" || n.Name == "DIRECT" {
			t.Fatalf("节点名与 Clash/sing-box 内置策略名冲突未被避让: %q", n.Name)
		}
	}
}

func TestAudit20261122_SyncHTTPStatusPatternRejectsIPAndVersion(t *testing.T) {
	if deterministicSyncFailure(errors.New("dial tcp 10.0.401.5:443: i/o timeout")) {
		t.Fatal("IP 地址中的 401 不得被误判为确定性 HTTP 401 拒绝")
	}
	if deterministicSyncFailure(errors.New("portal v403 handshake timeout")) {
		t.Fatal("版本号 v403 不得被误判为确定性 HTTP 403 拒绝")
	}
	if !deterministicSyncFailure(errors.New("节点 API 报错: HTTP 401")) {
		t.Fatal("标准 HTTP 401 必须识别为确定性拒绝")
	}
}

func TestAudit20261122_StoreSignatureBodyLimitAndHashedReplayKey(t *testing.T) {
	secret := "FAKE_STORE_SIGN_SECRET_FOR_UNIT_TEST_ONLY"
	bigBody := bytes.Repeat([]byte("a"), (1<<20)+16)
	req := httptest.NewRequest(http.MethodPost, "/api/store/orders", bytes.NewReader(bigBody))
	now := time.Now()
	req.Header.Set("X-Store-Timestamp", "1700000000")
	req.Header.Set("X-Store-Signature", strings.Repeat("a", 64))
	signed, err := verifyStoreSignature(req, secret, time.Unix(1700000000, 0))
	if !signed || err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("超过 1 MiB 的签名请求体应被显式拒绝，实际 signed=%v err=%v", signed, err)
	}

	// 验证进程内重放缓存不以明文拼接 secret
	nonce := "0123456789abcdef0123456789abcdef"
	ts := "1700000000"
	body := []byte(`{"order_id":"ORD-1"}`)
	msg := storeSignatureMessageV2(ts, http.MethodPost, "/api/store/orders", "", nonce, body)
	sig := storeSign(secret, msg)
	req2 := httptest.NewRequest(http.MethodPost, "/api/store/orders", bytes.NewReader(body))
	req2.Header.Set("X-Store-Timestamp", ts)
	req2.Header.Set("X-Store-Signature-Version", "2")
	req2.Header.Set("X-Store-Nonce", nonce)
	req2.Header.Set("X-Store-Signature", sig)
	storeNonces.Lock()
	storeNonces.seen = map[string]time.Time{}
	storeNonces.Unlock()
	if _, err := verifyStoreSignature(req2, secret, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("合法 v2 签名不应失败: %v", err)
	}
	storeNonces.Lock()
	for k := range storeNonces.seen {
		if strings.Contains(k, secret) {
			storeNonces.Unlock()
			t.Fatalf("内存防重放表不得以明文保存共享密钥: %s", k)
		}
	}
	storeNonces.Unlock()
	_ = now
}

func TestAudit20261122_RequestClientIPRejectsMalformedPeer(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "not-an-ip-address"
	if got := s.requestClientIP(req); got != "" {
		t.Fatalf("非法 RemoteAddr 应返回空字符串避免写 inet 报错，实际 %q", got)
	}
}

func TestAudit20261122_SlidingLimiterSweepsExpiredBeforeHardReset(t *testing.T) {
	l := newSlidingLimiter(time.Minute, 10)
	expired := time.Now().Add(-2 * time.Minute)
	active := time.Now()
	for i := 0; i < limiterHardResetLimit-1; i++ {
		l.attempts[strings.Repeat("e", 1)+itoa(i)] = []time.Time{expired}
	}
	l.attempts["hot-attacker"] = []time.Time{active, active, active}
	_ = l.allow("new-caller")
	if len(l.attempts["hot-attacker"]) != 3 {
		t.Fatalf("达硬上限时应先清理过期条目并保留活跃攻击者计数，实际剩 %d", len(l.attempts["hot-attacker"]))
	}
}

func TestAudit20261122_LoginGuardCapsMemoryUnderFlood(t *testing.T) {
	const capLimit = 16384
	g := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < capLimit+50; i++ {
		g.fail("flood-"+itoa(i), now)
	}
	if len(g.fails) > capLimit {
		t.Fatalf("登录失败表应受硬上限 %d 约束，实际 %d", capLimit, len(g.fails))
	}
}

func TestAudit20261122_CollectSubscriptionStatusSortsGone(t *testing.T) {
	out := collectSubscriptionStatusResults([]subscriptionStatusResult{
		{nodeID: "2", name: "节点-Z", err: "deleted", gone: true},
		{nodeID: "1", name: "节点-A", err: "deleted", gone: true},
	})
	if len(out.Gone) != 2 || out.Gone[0] != "节点-A" || out.Gone[1] != "节点-Z" {
		t.Fatalf("Gone 列表应稳定排序，实际 %v", out.Gone)
	}
}

func TestAudit20261122_SourceGuardsInPlace(t *testing.T) {
	// 验证关键并发/安全修复已落位
	ds := funcBlock(t, "delivery_sync.go", "func (s *Server) claimNextSubscriptionSync(")
	if !strings.Contains(ds, "CASE status WHEN 'pending' THEN 0 ELSE 1 END") {
		t.Fatal("claimNextSubscriptionSync 必须优先领取 pending 防止被 degraded 饥饿")
	}
	mg := funcBlock(t, "manage.go", "func (s *Server) updateServer(")
	if !strings.Contains(mg, "input.TrafficURL != nil") || !strings.Contains(mg, "input.TrafficFile != nil") {
		t.Fatal("updateServer 必须将 TrafficURL / TrafficFile 纳入管理员权限门禁")
	}
	sv := funcBlock(t, "server.go", "func (s *Server) createNode(")
	if !strings.Contains(sv, "if s.box == nil") {
		t.Fatal("createNode 调用 s.box.Seal 前必须判空防 panic")
	}
	st := funcBlock(t, "store.go", "func (s *Server) recordSubscriptionHit(")
	if !strings.Contains(st, "d.ip_limit = 0 OR d.seen OR d.live_count < d.ip_limit") {
		t.Fatal("recordSubscriptionHit 必须使用锁内权威 d.ip_limit 而非锁外参数快照")
	}
	sa := funcBlock(t, "store_adopt.go", "func (s *Server) storeAdopt(")
	if !strings.Contains(sa, "is_panel_merged") || !strings.Contains(sa, "loadSubscriptionByID(lockedCtx") {
		t.Fatal("storeAdopt 必须在订阅锁内重读并拒绝接管面板默认合并订阅")
	}
	sub := funcBlock(t, "subscription.go", "func (s *Server) subscriptionPreview(")
	if strings.Contains(sub, "nodes := build.Nodes") {
		t.Fatal("subscriptionPreview 遇 ErrNoRows 绝不能回退到包含主密码的全站 build.Nodes")
	}
	ec := funcBlock(t, "expired_closeout.go", "func (s *Server) closeExpiredSubscription(")
	if !strings.Contains(ec, "s.db.Begin(ctx)") {
		t.Fatal("closeExpiredSubscription 摘映射与删行必须在同一事务中提交")
	}
	_ = context.Background()
}
