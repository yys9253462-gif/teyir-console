package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

// TestR2_SanitizeParamsForAuditRedactsProxyURL 验证门户动作审计会剥离代理 URL 内嵌密码与口令字段。
func TestR2_SanitizeParamsForAuditRedactsProxyURL(t *testing.T) {
	got := sanitizeParamsForAudit(map[string]string{
		"action":   "save",
		"name":     "out1",
		"password": "plain-password-123",
		"url":      "http://alice:super-secret@203.0.113.10:8080",
	})
	if got["password"] != "(已省略)" {
		t.Fatalf("password 字段应省略，实际 %q", got["password"])
	}
	if strings.Contains(got["url"], "super-secret") {
		t.Fatalf("url 字段明文密码泄漏进审计：%q", got["url"])
	}
	if !strings.Contains(got["url"], "203.0.113.10:8080") {
		t.Fatalf("url 主机信息不应丢失：%q", got["url"])
	}

	// 含非法百分号转义的代理密码同样必须被抹除。
	bad := sanitizeParamsForAudit(map[string]string{
		"url": "http://bob:p%ss@203.0.113.11:8080",
	})
	if strings.Contains(bad["url"], "p%ss") {
		t.Fatalf("畸形转义代理密码泄漏进审计：%q", bad["url"])
	}
}

// TestR2_StartMigrateNodeChecksInstallScriptURLBeforeDB 验证节点迁移在动库和占租约前先校验安装脚本来源与节点 ID。
func TestR2_StartMigrateNodeChecksInstallScriptURLBeforeDB(t *testing.T) {
	t.Setenv("HY2_INSTALL_SCRIPT_URL", "")
	t.Setenv("HY2_INSTALLER_REPO", "")

	s := fixServer(nil)
	claims := auth.Claims{Subject: adminA, Role: "admin"}

	// 1. 畸形节点 ID 必须在查库前返回 404。
	badReq := httptest.NewRequest(http.MethodPost, "/api/nodes/not-a-uuid/migrate",
		strings.NewReader(`{"target_server_id":"00000000-0000-0000-0000-000000000103","email":"ops@example.com"}`))
	badReq = badReq.WithContext(context.WithValue(badReq.Context(), claimsKey, claims))
	badReq.SetPathValue("id", "not-a-uuid")
	badRec := httptest.NewRecorder()
	s.startMigrateNode(badRec, badReq)
	if badRec.Code != http.StatusNotFound {
		t.Fatalf("畸形节点 ID 应返回 404，实际 %d %s", badRec.Code, badRec.Body.String())
	}

	// 2. 未配置安装脚本来源时，在访问数据库前即返回 503，绝不先污染目标主机。
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/"+fixNode+"/migrate",
		strings.NewReader(`{"target_server_id":"`+fixHost+`","email":"ops@example.com"}`))
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, claims))
	req.SetPathValue("id", fixNode)
	rec := httptest.NewRecorder()
	s.startMigrateNode(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置安装脚本来源应返回 503，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestR2_PortalUpdateStatusCacheRecoversPanicAndHonorsGen 验证门户版本缓存异常恢复与版本号防回写。
func TestR2_PortalUpdateStatusCacheRecoversPanicAndHonorsGen(t *testing.T) {
	invalidatePortalUpdateStatus()
	t.Cleanup(invalidatePortalUpdateStatus)

	// s.db == nil 会在 checkPortalUpdateStatus 中触发 panic；cachedPortalUpdateStatus 必须恢复并清理 inflight，不得死锁。
	s := fixServer(nil)
	res1 := s.cachedPortalUpdateStatus(context.Background(), true)
	if res1.Error == "" {
		t.Fatalf("异常路径应返回错误提示，实际为空")
	}
	portalUpdateStatusCache.Lock()
	inflight := portalUpdateStatusCache.inflight
	genBefore := portalUpdateStatusCache.gen
	portalUpdateStatusCache.Unlock()
	if inflight != nil {
		t.Fatal("异常后 inflight 未清空，后续请求将永久阻塞")
	}

	invalidatePortalUpdateStatus()
	portalUpdateStatusCache.Lock()
	genAfter := portalUpdateStatusCache.gen
	portalUpdateStatusCache.Unlock()
	if genAfter <= genBefore {
		t.Fatalf("invalidatePortalUpdateStatus 必须递增版本号：before=%d after=%d", genBefore, genAfter)
	}

	// 第二次调用同样可立即完成，不挂死。
	res2 := s.cachedPortalUpdateStatus(context.Background(), true)
	if res2.Error == "" {
		t.Fatalf("二次异常调用应返回错误提示，实际为空")
	}
}

// TestR2_ClientOptionsCacheGenerationPreventsStaleWriteback 验证缓存作废时在途旧拉取不会把过期配置回写进缓存。
func TestR2_ClientOptionsCacheGenerationPreventsStaleWriteback(t *testing.T) {
	InvalidateSubscriptionCache()
	t.Cleanup(InvalidateSubscriptionCache)

	oldFetch := fetchClientOptionsFn
	t.Cleanup(func() { fetchClientOptionsFn = oldFetch })

	entered := make(chan struct{})
	release := make(chan struct{})
	fetchClientOptionsFn = func(_ *Server, _ context.Context, node subscriptionNode) (subscriptionNode, error) {
		close(entered)
		<-release
		node.ObfsPass = "old-obfs"
		node.HopRange = "20000-20010"
		return node, nil
	}

	node := subscriptionNode{
		ID:            fixNode,
		Name:          "测试节点",
		Host:          "node.example.com",
		Port:          443,
		AccountUserID: "sub_user_1",
		Password:      "p1",
	}
	s := fixServer(nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.cachedSubscriptionClientOptions(context.Background(), node)
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("等待进入远端拉取超时")
	}

	// 拉取在途期间触发缓存失效（例如节点改密或改端口跳跃）。
	InvalidateSubscriptionCache()
	close(release)
	wg.Wait()

	sum := sha256.Sum256([]byte(node.ID + "\x00" + node.AccountUserID + "\x00" + node.Password + "\x00" + node.Host + "\x00" + "443"))
	key := hex.EncodeToString(sum[:])
	clientOptionsCache.Lock()
	_, exists := clientOptionsCache.m[key]
	clientOptionsCache.Unlock()
	if exists {
		t.Fatal("缓存作废前发起的在途请求不应把旧配置回写进 clientOptionsCache")
	}
}

// TestR2_NilPurityKeysSafety 验证未初始化第三方密钥库时所有读/通知路径不发生空指针崩溃，且空 IP 不污染封禁表。
func TestR2_NilPurityKeysSafety(t *testing.T) {
	s := fixServer(nil) // s.purityKeys == nil, s.db == nil
	if tok := s.telegramToken(); tok != "" {
		t.Fatalf("未初始化密钥库时令牌应为空，实际 %q", tok)
	}
	if cf := s.cloudflareClient(); cf == nil || cf.Token != "" {
		t.Fatalf("未初始化密钥库时 cloudflareClient 应返回空令牌客户端")
	}

	claims := auth.Claims{Subject: adminA, Role: "admin"}

	// purityKeysGet 不应 panic。
	req1 := httptest.NewRequest(http.MethodGet, "/api/settings/purity-keys", nil)
	req1 = req1.WithContext(context.WithValue(req1.Context(), claimsKey, claims))
	rec1 := httptest.NewRecorder()
	s.purityKeysGet(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("purityKeysGet 应返回 200，实际 %d", rec1.Code)
	}

	// getTelegramSettings 不应 panic。
	req2 := httptest.NewRequest(http.MethodGet, "/api/settings/telegram", nil)
	req2 = req2.WithContext(context.WithValue(req2.Context(), claimsKey, claims))
	rec2 := httptest.NewRecorder()
	s.getTelegramSettings(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("getTelegramSettings 应返回 200，实际 %d", rec2.Code)
	}

	// testTelegram 在未配置 Chat ID 时应返回 400 而非 panic。
	req3 := httptest.NewRequest(http.MethodPost, "/api/settings/telegram/test", nil)
	req3 = req3.WithContext(context.WithValue(req3.Context(), claimsKey, claims))
	rec3 := httptest.NewRecorder()
	s.testTelegram(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("testTelegram 未配置 Chat ID 应返回 400，实际 %d", rec3.Code)
	}

	// 空 IP 登录失败不应进入封禁表。
	for i := 0; i < loginFailMax+2; i++ {
		s.onLoginFailure("   ", "test@example.com")
	}
	if loginGuardState.bannedUntil(loginKey(""), time.Now()) != (time.Time{}) {
		t.Fatal("空 IP 不应被写入登录封禁表")
	}
}

// TestR2_MissingLimitersEnforcedOnFourRoutes 验证四条补齐限流的管理接口在超限时返回 429。
func TestR2_MissingLimitersEnforcedOnFourRoutes(t *testing.T) {
	claims := auth.Claims{Subject: adminA, Role: "admin"}
	zeroRead := func() *Server {
		s := fixServer(nil)
		s.readLimiter = newSlidingLimiter(time.Minute, 0)
		return s
	}
	zeroWrite := func() *Server {
		s := fixServer(nil)
		s.writeLimiter = newSlidingLimiter(time.Minute, 0)
		return s
	}

	cases := []struct {
		name    string
		srv     *Server
		method  string
		path    string
		handler func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"分类列表", zeroRead(), http.MethodGet, "/api/categories", (*Server).categoriesList},
		{"商城签名查询", zeroRead(), http.MethodGet, "/api/settings/store-signature", (*Server).getStoreSignature},
		{"电报设置查询", zeroRead(), http.MethodGet, "/api/settings/telegram", (*Server).getTelegramSettings},
		{"电报测试发送", zeroWrite(), http.MethodPost, "/api/settings/telegram/test", (*Server).testTelegram},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(context.WithValue(req.Context(), claimsKey, claims))
			rec := httptest.NewRecorder()
			tc.handler(tc.srv, rec, req)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("%s 超限应返回 429，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestR2_MalformedUUIDReturns404BeforeDB 验证所有补齐 UUID 校验的接口对非法 ID 在触库前返回 404。
func TestR2_MalformedUUIDReturns404BeforeDB(t *testing.T) {
	s := fixServer(nil) // s.db == nil：若未提前拦截必会 panic 或 500
	claims := auth.Claims{Subject: adminA, Role: "admin"}

	cases := []struct {
		name      string
		method    string
		path      string
		body      string
		dangerous bool
		handler   func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"deleteNode", http.MethodDelete, "/api/nodes/bad-id", "", true, (*Server).deleteNode},
		{"updateNode", http.MethodPatch, "/api/nodes/bad-id", `{"name":"x"}`, false, (*Server).updateNode},
		{"nodeSetCategories", http.MethodPut, "/api/nodes/bad-id/categories", `{"category_ids":[]}`, false, (*Server).nodeSetCategories},
		{"serviceStatus", http.MethodGet, "/api/servers/bad-id/services", "", false, (*Server).serviceStatus},
		{"xuiDeletePanel", http.MethodDelete, "/api/xui/panels/bad-id", "", true, (*Server).xuiDeletePanel},
		{"testServerSSH", http.MethodPost, "/api/servers/bad-id/test-ssh", "", false, (*Server).testServerSSH},
		{"putServerBilling", http.MethodPut, "/api/servers/bad-id/billing", `{"currency":"USD"}`, false, (*Server).putServerBilling},
		{"changeServerIP", http.MethodPost, "/api/servers/bad-id/change-ip", `{"ip":"1.1.1.1","update_dns":false}`, true, (*Server).changeServerIP},
		{"serverMetricsTrend", http.MethodGet, "/api/servers/bad-id/metrics/trend", "", false, (*Server).serverMetricsTrend},
		{"nodeIPPurity", http.MethodGet, "/api/nodes/bad-id/ip-purity", "", false, (*Server).nodeIPPurity},
		{"refreshNodeIPPurity", http.MethodPost, "/api/nodes/bad-id/ip-purity/refresh", "", false, (*Server).refreshNodeIPPurity},
		{"refreshNodeUnlock", http.MethodPost, "/api/nodes/bad-id/unlock/refresh", "", false, (*Server).refreshNodeUnlock},
		{"setSubscriptionRevenue", http.MethodPut, "/api/subscriptions/bad-id/revenue", `{"revenue_currency":"CNY"}`, false, (*Server).setSubscriptionRevenue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req = req.WithContext(context.WithValue(req.Context(), claimsKey, claims))
			req.SetPathValue("id", "not-a-valid-uuid")
			if tc.dangerous {
				req.Header.Set("X-Confirm-Dangerous", "yes")
			}
			rec := httptest.NewRecorder()
			tc.handler(s, rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s 畸形 UUID 应返回 404，实际 %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestR2_CreateServerAndUploadCredentialRejectControlChars 验证主机与凭据接口拦截含空白或控制字符的 host / ssh_user。
func TestR2_CreateServerAndUploadCredentialRejectControlChars(t *testing.T) {
	s := fixServer(nil)
	claims := auth.Claims{Subject: adminA, Role: "admin"}

	badBodies := []string{
		`{"name":"n1","host":"203.0.113.1\u0007","ssh_port":22,"ssh_user":"root"}`,
		`{"name":"n2","host":"203.0.113.2","ssh_port":22,"ssh_user":"root admin"}`,
		`{"name":"n3","host":"203.0.113.3","ssh_port":22,"ssh_user":"root\nid"}`,
	}
	for _, body := range badBodies {
		req := httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, claims))
		rec := httptest.NewRecorder()
		s.createServer(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("createServer 非法入参 %s 应返回 400，实际 %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestR2_XUICallRejectsOversizedResponse 验证 3X-UI 客户端遇到超过 1 MiB 响应时显式报错而不静默截断。
func TestR2_XUICallRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), (1<<20)+128))
	}))
	defer srv.Close()

	_, _, _, err := xuiCall(context.Background(), srv.Client(), xuiRequest{
		method: http.MethodGet,
		url:    srv.URL + "/panel/api/inbounds/list",
	})
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限响应应显式拒绝并说明超限，实际 err=%v", err)
	}
}

// TestR2_RevokeExpiredInBackgroundBounded 验证过期订阅后台回收在并发槽满时直接跳过且不残留去重标记。
func TestR2_RevokeExpiredInBackgroundBounded(t *testing.T) {
	// 临时占满全局槽位。
	for i := 0; i < cap(expiredRevokeSem); i++ {
		expiredRevokeSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(expiredRevokeSem); i++ {
			<-expiredRevokeSem
		}
	}()

	s := fixServer(nil)
	rec := &subscriptionRecord{ID: "00000000-0000-0000-0000-000000000999"}
	s.revokeExpiredInBackground(rec)

	if _, leaked := expiredRevokeInFlight.Load(rec.ID); leaked {
		expiredRevokeInFlight.Delete(rec.ID)
		t.Fatal("并发槽满跳过回收后不应残留 expiredRevokeInFlight 标记")
	}
}

// TestR2_StructuralGuards 第二轮后端修复结构防回归守卫。
func TestR2_StructuralGuards(t *testing.T) {
	checks := []struct {
		file string
		fn   string
		want []string
	}{
		{"store.go", "func (s *Server) recordSubscriptionHit(", []string{
			"WHEN d.deleted_at IS NOT NULL THEN 'not_found'",
			"WHEN d.expires_at IS NOT NULL AND d.expires_at <= $4::timestamptz THEN 'expired'",
			"WHEN NOT d.enabled THEN 'disabled'",
			"ELSE 'ip_limit'",
		}},
		{"store.go", "func (s *Server) enforceSubscriptionAccess(", []string{"if verdict != \"\""}},
		{"store.go", "func (s *Server) revokeNodeFromSubscriptions(", []string{"canonicalUUID(nodeID)"}},
		{"delivery_sync.go", "func (s *Server) lockSubscription(", []string{"canonicalUUID(id)"}},
		{"delivery_sync.go", "func (s *Server) subscriptionMutation(", []string{"canonicalUUID(r.PathValue(\"id\"))"}},
		{"store_adopt.go", "func (s *Server) storeAdopt(", []string{"pg_advisory_xact_lock(hashtext('hy2-ops:store-order:' || $1))"}},
		{"blockcheck.go", "func (s *Server) runBlockChecks(", []string{"unprobeablePersistFailed", "targetPersistFailed", "len(targets) - targetPersistFailed"}},
		{"scheduler.go", "func (s *Server) collectAuditDigest(", []string{"s.telegramToken() != \"\""}},
		{"telegram.go", "func (s *Server) StartTelegramBot(", []string{"isOwner := telegramPrivateOwner", "if isOwner {"}},
		{"telegram_ui.go", "func (s *Server) telegramPick(", []string{"COALESCE(label,'')"}},
		{"telegram_wizard.go", "func (s *Server) telegramWizardOptions(", []string{"COALESCE(label,'')", "COALESCE(payload->>'step','')"}},
		{"server.go", "func (s *Server) authenticated(", []string{"forbidden_viewer_subscription_credential"}},
		{"server.go", "func (s *Server) taskLogs(", []string{"forbidden_viewer_task_logs"}},
	}
	for _, c := range checks {
		body := funcBlock(t, c.file, c.fn)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s %s 缺少第二轮修复守卫：%q", c.file, c.fn, w)
			}
		}
	}
}

// TestR2_PG_RecordSubscriptionHitSeparatesThreeFailureStates 真实 PG 验证行锁内三态分离与网络上限准确判定。
func TestR2_PG_RecordSubscriptionHitSeparatesThreeFailureStates(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	s := fixServer(db)

	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET ip_limit=1 WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	rec := &subscriptionRecord{ID: fixSub, Enabled: true, IPLimit: 1}
	now := time.Now().UTC()

	// 1. 首个公网 IP 放行并落账。
	if v, err := s.recordSubscriptionHit(ctx, rec, "203.0.113.10", now); err != nil || v != "" {
		t.Fatalf("首个 IP 应放行，实际 verdict=%q err=%v", v, err)
	}
	// 2. 第二个不同 IP 超限应返回 ip_limit。
	if v, err := s.recordSubscriptionHit(ctx, rec, "203.0.113.11", now); err != nil || v != "ip_limit" {
		t.Fatalf("超网络上限应返回 ip_limit，实际 verdict=%q err=%v", v, err)
	}
	// 3. 并发停用（即使调用方持有旧快照 Enabled=true）必须返回 disabled 而非 ip_limit。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET enabled=false WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	if v, err := s.recordSubscriptionHit(ctx, rec, "203.0.113.10", now); err != nil || v != "disabled" {
		t.Fatalf("停用订阅应返回 disabled，实际 verdict=%q err=%v", v, err)
	}
	// 4. 订阅到期且已停用：到期优先于停用，必须返回 expired。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET expires_at=now()-interval '1 minute' WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	if v, err := s.recordSubscriptionHit(ctx, rec, "203.0.113.10", now); err != nil || v != "expired" {
		t.Fatalf("到期订阅应返回 expired，实际 verdict=%q err=%v", v, err)
	}
	// 5. 订阅软删必须返回 not_found。
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET deleted_at=now() WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	if v, err := s.recordSubscriptionHit(ctx, rec, "203.0.113.10", now); err != nil || v != "not_found" {
		t.Fatalf("软删订阅应返回 not_found，实际 verdict=%q err=%v", v, err)
	}
}

// TestR2_PG_UppercaseNodeIDRevokesBuyerAccounts 真实 PG 验证大写节点 UUID 同样能精准匹配 node_accounts 并注销买家账号。
func TestR2_PG_UppercaseNodeIDRevokesBuyerAccounts(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)

	const upperNode = "00000000-0000-0000-0000-0000000001AB"
	lowerNode := strings.ToLower(upperNode)
	accounts, _ := json.Marshal(map[string]subscriptionNodeAccount{
		lowerNode: {UserID: "sub_upper_case_1", Password: "pwd"},
	})
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=$2::jsonb WHERE id=$1::uuid`, fixSub, string(accounts)); err != nil {
		t.Fatal(err)
	}

	oldClient := portalClient
	t.Cleanup(func() { portalClient = oldClient })
	var deletedUID string
	portalClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			deletedUID = body.UserID
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		}),
	}

	s := fixServer(db)
	override := &portalTarget{
		NodeID:   lowerNode,
		NodeName: "大写测试节点",
		BaseURL:  "https://203.0.113.20:8443",
		APIKey:   strings.Repeat("a", 48),
	}
	// 故意传入全大写 UUID：必须命中并完成节点注销与映射清理。
	outcome := s.revokeNodeFromAllSubscriptions(ctx, upperNode, override)
	if !outcome.OK() || outcome.Revoked != 1 {
		t.Fatalf("大写 UUID 回收应成功注销 1 个账号，实际 revoked=%d failed=%v err=%v", outcome.Revoked, outcome.Failed, outcome.Err)
	}
	if deletedUID != "sub_upper_case_1" {
		t.Fatalf("节点未收到目标账号注销请求，实际 %q", deletedUID)
	}
	var remaining int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid AND node_accounts ? $2`, fixSub, lowerNode).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("注销完成后 node_accounts 仍残留节点键")
	}
}

// TestR2_PG_LockSubscriptionCaseInsensitiveMutualExclusion 真实 PG 验证订阅锁对大小写 UUID 一致互斥且支持重入。
func TestR2_PG_LockSubscriptionCaseInsensitiveMutualExclusion(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)

	const lowerID = "00000000-0000-0000-0000-0000000001cd"
	upperID := strings.ToUpper(lowerID)

	lockedCtx, release, err := s.lockSubscription(ctx, lowerID)
	if err != nil {
		t.Fatalf("首次加锁失败：%v", err)
	}
	defer release()

	// 不同上下文使用大写同 ID 竞争锁，必须报并发冲突而非拿到第二把锁。
	if _, rel2, err2 := s.lockSubscription(ctx, upperID); err2 == nil {
		rel2()
		t.Fatal("大写 UUID 绕过了小写 UUID 持有的跨进程订阅互斥锁")
	}

	// 同一上下文重入传入大写 ID 必须识别为同一把锁。
	_, relReentrant, errReentrant := s.lockSubscription(lockedCtx, upperID)
	if errReentrant != nil {
		t.Fatalf("同上下文以大写同 ID 重入应成功，实际报错：%v", errReentrant)
	}
	relReentrant()
}

// TestR2_PG_StoreAdoptBlocksRevokedAndAdoptsValidOrder 真实 PG 验证挂单事务级订单锁与已撤单拦截。
func TestR2_PG_StoreAdoptBlocksRevokedAndAdoptsValidOrder(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const subToken = "manual-sub-token-for-adopt-regression-0001"
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,expires_at) VALUES($1::uuid,$2,now()+interval '30 days')`, fixSub, subToken); err != nil {
		t.Fatal(err)
	}
	const (
		revokedSubID = "00000000-0000-0000-0000-000000000998"
		revokedOrder = "OPS-prdord_revoked_r2_0001"
		validOrder   = "OPS-prdord_valid_r2_0001"
	)
	if _, err := db.Exec(ctx, `INSERT INTO store_revoked_orders(order_id, subscription_id) VALUES($1, $2::uuid)`, revokedOrder, revokedSubID); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)

	callAdopt := func(orderID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/store/adopt",
			strings.NewReader(`{"token":"`+subToken+`","order_id":"`+orderID+`"}`))
		rec := httptest.NewRecorder()
		s.storeAdopt(rec, req)
		return rec.Code
	}

	if code := callAdopt(revokedOrder); code != http.StatusGone {
		t.Fatalf("已撤单订单号应返回 410，实际 %d", code)
	}
	if code := callAdopt(validOrder); code != http.StatusOK {
		t.Fatalf("合法订单号挂接应返回 200，实际 %d", code)
	}
	var savedOrder string
	if err := db.QueryRow(ctx, `SELECT COALESCE(order_id,'') FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&savedOrder); err != nil {
		t.Fatal(err)
	}
	if savedOrder != validOrder {
		t.Fatalf("挂单后数据库 order_id 应为 %s，实际 %q", validOrder, savedOrder)
	}
}
