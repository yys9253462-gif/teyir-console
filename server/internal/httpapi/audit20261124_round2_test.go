package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// 验证同步失败分类正则只匹配明确 HTTP/状态前缀的 401/403，不误判端口号或毫秒延迟。
func TestSyncHTTPStatusPatternRejectsPortsAndLatencies(t *testing.T) {
	mustMatch := []string{
		"节点返回 HTTP 401",
		"portal status=403",
		"节点 API 返回 401",
		"HTTP 403",
	}
	for _, msg := range mustMatch {
		if !deterministicSyncFailure(errors.New(msg)) {
			t.Errorf("应判定为确定性拒绝，实际误判为可恢复: %q", msg)
		}
	}

	mustNotMatch := []string{
		"dial tcp 203.0.113.10:403: connection refused",
		"dial tcp 203.0.113.10:401: i/o timeout",
		"probe timeout after 403 ms",
		"retrying request 401 times",
	}
	for _, msg := range mustNotMatch {
		if deterministicSyncFailure(errors.New(msg)) {
			t.Errorf("不应把端口或耗时中的 401/403 误判为确定性拒绝: %q", msg)
		}
	}
}

// 验证 Clash 配置仅对自签节点（Insecure=true）下发证书指纹，公网 CA 证书节点不误贴指纹。
func TestClashProxyOnlyPinsSelfSignedNodes(t *testing.T) {
	pin := strings.Repeat("a", 64)
	selfSigned := clashProxy(subscriptionNode{
		Name: "self", Host: "203.0.113.10", Port: 443, Password: "pw", Insecure: true, PinSHA256: pin,
	})
	if selfSigned["fingerprint"] != pin {
		t.Fatalf("自签节点应下发 fingerprint，实际=%v", selfSigned["fingerprint"])
	}

	caSigned := clashProxy(subscriptionNode{
		Name: "ca", Host: "example.test", Port: 443, Password: "pw", Insecure: false, PinSHA256: pin,
	})
	if _, exists := caSigned["fingerprint"]; exists {
		t.Fatalf("公网 CA 证书节点（Insecure=false）不应下发 fingerprint: %+v", caSigned)
	}
}

// 验证短密钥脱敏不会暴露全部明文，长密钥保留少量首尾用于辨识。
func TestMaskSecretProtectsShortKeys(t *testing.T) {
	if got := maskSecret(""); got != "" {
		t.Fatalf("空串应保持空串，实际=%q", got)
	}
	for _, short := range []string{"1234567890123", "1234567890123456"} {
		got := maskSecret(short)
		if got != strings.Repeat("*", len(short)) {
			t.Errorf("长度为 %d 的短密钥必须全掩码，实际=%q", len(short), got)
		}
	}
	longKey := "fake-secret-token-for-unit-test-9999"
	got := maskSecret(longKey)
	if !strings.HasPrefix(got, "fake-s********") || !strings.HasSuffix(got, "9999") {
		t.Fatalf("长密钥掩码格式异常: %q", got)
	}
	if strings.Contains(got, "secret-token") {
		t.Fatalf("长密钥掩码泄露中间明文: %q", got)
	}
}

// 验证商城签名重放缓存队列能正常拦截重放并在过期后自动淘汰放行。
func TestStoreNoncesQueueEvictionAndExpiry(t *testing.T) {
	storeNonces.Lock()
	oldSeen, oldOrder, oldHead := storeNonces.seen, storeNonces.order, storeNonces.head
	storeNonces.seen = map[string]time.Time{}
	storeNonces.order = nil
	storeNonces.head = 0
	storeNonces.Unlock()
	t.Cleanup(func() {
		storeNonces.Lock()
		storeNonces.seen, storeNonces.order, storeNonces.head = oldSeen, oldOrder, oldHead
		storeNonces.Unlock()
	})

	secret := "store-test-secret-fake-only"
	now := time.Unix(1760000000, 0)
	makeReq := func(ts time.Time, nonce string) *http.Request {
		body := `{"order_id":"ord_1"}`
		tsRaw := strconv.FormatInt(ts.Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/api/store/orders", strings.NewReader(body))
		msg := storeSignatureMessageV2(tsRaw, req.Method, req.URL.Path, req.URL.Query().Encode(), nonce, []byte(body))
		req.Header.Set("X-Store-Timestamp", tsRaw)
		req.Header.Set("X-Store-Signature-Version", "2")
		req.Header.Set("X-Store-Nonce", nonce)
		req.Header.Set("X-Store-Signature", storeSign(secret, msg))
		return req
	}

	nonce := "0123456789abcdef0123456789abcdef"
	if signed, err := verifyStoreSignature(makeReq(now, nonce), secret, now); !signed || err != nil {
		t.Fatalf("首次合法签名应通过: signed=%v err=%v", signed, err)
	}
	if signed, err := verifyStoreSignature(makeReq(now, nonce), secret, now.Add(time.Second)); !signed || err == nil {
		t.Fatalf("过期前重放必须被拒绝: signed=%v err=%v", signed, err)
	}
	later := now.Add(storeNonceTTL + time.Second)
	if signed, err := verifyStoreSignature(makeReq(later, nonce), secret, later); !signed || err != nil {
		t.Fatalf("TTL 过期后队列应自动淘汰旧键并允许新时间戳请求: signed=%v err=%v", signed, err)
	}
}

// 验证 3X-UI 地址规范化拒绝空主机名与 ./.. 路径穿越段。
func TestNormalizeXUIBaseURLRejectsEmptyHostAndDotSegments(t *testing.T) {
	bad := []string{
		"https://",
		"https:///panel",
		"http://:2053",
		"https://xui.example.com/..",
		"https://xui.example.com/.",
		"https://xui.example.com/sub/../admin",
	}
	for _, raw := range bad {
		if got, err := normalizeXUIBaseURL(raw); err == nil {
			t.Errorf("非法 3X-UI 地址 %q 未被拒绝，返回=%q", raw, got)
		}
	}
	if got, err := normalizeXUIBaseURL("https://xui.example.com/sub/"); err != nil || got != "https://xui.example.com/sub" {
		t.Fatalf("合法 3X-UI 子路径地址被误拒: got=%q err=%v", got, err)
	}
}

// 验证 3X-UI 入站归一化补齐孤儿统计客户端时顺序稳定且无遗漏。
func TestNormalizeXUIInboundPreservesOrphanStatOrder(t *testing.T) {
	raw := map[string]any{
		"id":       float64(1),
		"port":     float64(443),
		"protocol": "vless",
		"remark":   "main",
		"enable":   true,
		"settings": map[string]any{
			"clients": []any{
				map[string]any{"email": "a@test", "id": "u-a"},
			},
		},
		"clientStats": []any{
			map[string]any{"email": "a@test", "up": float64(10), "down": float64(20)},
			map[string]any{"email": "orphan-1@test", "up": float64(1), "down": float64(2)},
			map[string]any{"email": "orphan-2@test", "up": float64(3), "down": float64(4)},
			map[string]any{"email": "orphan-3@test", "up": float64(5), "down": float64(6)},
		},
	}
	for i := 0; i < 10; i++ {
		summary := normalizeXUIInbound(raw)
		if len(summary.Clients) != 4 {
			t.Fatalf("期望 4 个客户端，实际 %d", len(summary.Clients))
		}
		wantOrder := []string{"a@test", "orphan-1@test", "orphan-2@test", "orphan-3@test"}
		for idx, want := range wantOrder {
			if got, _ := summary.Clients[idx]["email"].(string); got != want {
				t.Fatalf("第 %d 次顺序漂移: idx=%d got=%q want=%q", i, idx, got, want)
			}
		}
	}
}

// 验证 missingAccountNodes 兼容空订阅指针，并把缺失 UserID 的残缺映射视为缺账号。
func TestMissingAccountNodesNilSafeAndChecksUserID(t *testing.T) {
	nodes := []subscriptionNode{{ID: "n1", Name: "n1", InStore: true}}
	if got := missingAccountNodes(nodes, nil, nil); got != nil {
		t.Fatalf("nil 订阅记录应安全返回 nil，实际=%+v", got)
	}
	rec := &subscriptionRecord{
		nodeAccounts: map[string]subscriptionNodeAccount{
			"n1": {UserID: "", Password: "only-password"},
		},
	}
	missing := missingAccountNodes(nodes, rec, nil)
	if len(missing) != 1 || missing[0].ID != "n1" {
		t.Fatalf("缺失 UserID 的残缺映射必须识别为缺账号，实际=%+v", missing)
	}
}

// 验证批量到期结案开启锁内复核后，存在在役节点时即使已进入单条结案也会被拦截且不写入孤儿记录。
func TestCloseExpiredBatchRequiresAllNodesGoneUnderLock(t *testing.T) {
	db := closeoutDB(t)
	ctx := context.Background()

	const (
		liveNode = "00000000-0000-0000-0000-00000000e501"
		expSub   = "00000000-0000-0000-0000-00000000e510"
	)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	liveKey, _ := box.Seal([]byte("live-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,deleted_at) VALUES($1,'在役节点','https://example.invalid:8443',$2,NULL)`, liveNode, liveKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,expires_at,enabled,auto_delete,node_accounts) VALUES($1, now()-interval '2 days', false, true, jsonb_build_object($2::text, jsonb_build_object('user_id','sub_live')))`, expSub, liveNode); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, box: box, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orphans, deleted, err := s.closeExpiredSubscription(withRequireAllNodesGone(ctx), expSub, "批量结案")
	if err == nil || deleted || orphans != 0 {
		t.Fatalf("锁内发现在役节点必须拒绝批量结案且不登记孤儿: orphans=%d deleted=%v err=%v", orphans, deleted, err)
	}

	var remainSub, remainOrphan int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid), (SELECT count(*) FROM orphan_node_accounts)`, expSub).Scan(&remainSub, &remainOrphan); err != nil {
		t.Fatal(err)
	}
	if remainSub != 1 || remainOrphan != 0 {
		t.Fatalf("拒绝批量结案后订阅行应保留且不得写入孤儿记录: remainSub=%d remainOrphan=%d", remainSub, remainOrphan)
	}
}

// 验证第二轮结构性防回归守卫：防止核心加固在后续迭代中被意外回退。
func TestRound2BackendStructuralHardeningGuards(t *testing.T) {
	checks := []struct {
		file string
		fn   string
		want []string
	}{
		{"manage.go", "func (s *Server) deleteNode(", []string{"portal_basic_user=NULL", "portal_basic_password=NULL", "portal_path_token=NULL", "invalidateNodeCapsCache(nodeID)"}},
		{"manage.go", "func (s *Server) serverDetail(", []string{"s.enforceReadLimit(w, claims.Subject)"}},
		{"manage.go", "func (s *Server) ReconcileAlerts(", []string{"s.alertNotifier != nil &&", "jsonb_typeof(st.node_accounts) = 'object'", "pastInitialGrace"}},
		{"store.go", "func (s *Server) storeCreateOrder(", []string{"pg_advisory_xact_lock(hashtext('hy2-ops:store-order:' || $1))"}},
		{"store.go", "func (s *Server) storeRevokeOrder(", []string{"pg_advisory_xact_lock(hashtext('hy2-ops:store-order:' || $1))", "DELETE FROM subscription_sync_jobs"}},
		{"store.go", "func (s *Server) sweepExpiredSubscriptions(", []string{"s.lockSubscription(ctx, rec.ID)", "s.loadSubscriptionByID(lockedCtx, rec.ID)"}},
		{"store_adopt.go", "func (s *Server) storeAdoptInfo(", []string{"s.databaseError(w, r, err)"}},
		{"categories.go", "func (s *Server) categoryDelete(", []string{"FOR UPDATE"}},
		{"capabilities.go", "func (s *Server) cachedNodeCapabilities(", []string{"if r := recover(); r != nil", "c.gen == startGen"}},
		{"delivery_sync.go", "func (s *Server) synchronizeClaimedSubscription(", []string{"s.clearAllRevokeBackoff(ctx, rec.ID)", "degrade := s.canDegradeSync(metaCtx, scope, failures)"}},
		{"subscription.go", "func (s *Server) cachedSubscriptionClientOptions(", []string{"delete(clientOptionsCache.m, key)"}},
		{"subscription.go", "func (s *Server) subscriptionFeed(", []string{"pctEncode(name+ext)"}},
	}
	for _, c := range checks {
		body := funcBlock(t, c.file, c.fn)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s %s 缺少第二轮加固代码: %q", c.file, c.fn, w)
			}
		}
	}
}
