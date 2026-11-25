package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

// 验证非管理员编辑节点时：未改动敏感字段可保存日常信息，改动归属主机/端口/域名/商城状态一律拒绝并写入拒绝审计。
func TestOperatorUpdateNodeSensitiveFieldsForbidden(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const otherHost = "00000000-0000-4000-8000-000000000802"
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text, ADD COLUMN status text, ADD COLUMN last_checked_at timestamptz, ADD COLUMN consecutive_probe_failures int DEFAULT 0, ADD COLUMN version text;
INSERT INTO servers(id) VALUES('`+fixHost+`'), ('`+otherHost+`');
INSERT INTO hysteria_nodes(id,name,api_url,api_key,portal_basic_user,portal_basic_password,portal_path_token,server_id,in_store,listen_port,domain)
 VALUES('`+fixNode+`','n','https://203.0.113.10:8443','k'::bytea,'u','p'::bytea,'0123456789abcdef0123456789abcdef','`+fixHost+`',false,443,'node.example.com')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)

	call := func(role, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/nodes/"+fixNode, strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: adminA, Role: role}))
		req.SetPathValue("id", fixNode)
		rec := httptest.NewRecorder()
		s.updateNode(rec, req)
		return rec.Code
	}

	// 运维角色带着未变敏感原值仅修改名称，应放行（兼容前端整表保存）。
	if code := call("operator", `{"name":"运维改名","server_id":"`+fixHost+`","listen_port":443,"domain":"node.example.com","in_store":false}`); code != http.StatusOK {
		t.Fatalf("运维仅改名称应返回 200，实际得到 %d", code)
	}

	// 运维角色实质变更任意敏感字段，必须全部返回 403。
	forbiddenPayloads := []string{
		`{"in_store":true}`,
		`{"domain":"evil.example.com"}`,
		`{"listen_port":8443}`,
		`{"server_id":"` + otherHost + `"}`,
	}
	for _, body := range forbiddenPayloads {
		if code := call("operator", body); code != http.StatusForbidden {
			t.Fatalf("运维尝试敏感变更 %s 应返回 403，实际得到 %d", body, code)
		}
	}

	// 验证拒绝审计记录落库。
	var deniedCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='forbidden_admin_only'`).Scan(&deniedCount); err != nil {
		t.Fatal(err)
	}
	if deniedCount != len(forbiddenPayloads) {
		t.Fatalf("期望写入 %d 条拒绝审计，实际 %d 条", len(forbiddenPayloads), deniedCount)
	}

	// 管理员可正常修改敏感字段；接管 gapFillOnce 并同步等待异步补开户协程退出，避免与测试库清理并发竞态。
	gapDone := make(chan struct{}, 1)
	oldGapFillOnce := gapFillOnce
	gapFillOnce = func(*Server, context.Context, string) int {
		select {
		case gapDone <- struct{}{}:
		default:
		}
		return 0
	}
	t.Cleanup(func() {
		gapFillInFlight.Lock()
		gapFillOnce = oldGapFillOnce
		gapFillInFlight.Unlock()
	})
	if code := call("admin", `{"domain":"new.example.com","in_store":true}`); code != http.StatusOK {
		t.Fatalf("管理员修改敏感字段应返回 200，实际得到 %d", code)
	}
	select {
	case <-gapDone:
	case <-time.After(3 * time.Second):
		t.Fatal("等待节点上架触发的异步补开户协程超时")
	}
	// 获取并释放一次互斥锁，确保后台补开户协程已完整退出后再执行测试清理。
	gapFillInFlight.Lock()
	gapFillInFlight.Unlock()
}

// 验证运维角色不能取消管理员排队的写类任务，但可取消只读任务；管理员可取消写类任务。
func TestOperatorCannotCancelWriteTask(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const writeTask = "00000000-0000-0000-0000-0000000c0001"
	const readTask = "00000000-0000-0000-0000-0000000c0002"
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN operation_type text, ADD COLUMN finished_at timestamptz;
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO operation_tasks(id,server_id,status,operation_type) VALUES
 ('`+writeTask+`','`+fixHost+`','queued','system_preset'),
 ('`+readTask+`','`+fixHost+`','queued','health_check')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)

	cancelAs := func(role, taskID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/cancel", nil)
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: adminA, Role: role}))
		req.SetPathValue("id", taskID)
		rec := httptest.NewRecorder()
		s.cancelTask(rec, req)
		return rec.Code
	}

	if code := cancelAs("operator", writeTask); code != http.StatusForbidden {
		t.Fatalf("运维取消写类任务应返回 403，实际得到 %d", code)
	}
	// 确认写类任务状态仍保持 queued。
	var st string
	if err := db.QueryRow(ctx, `SELECT status FROM operation_tasks WHERE id=$1::uuid`, writeTask).Scan(&st); err != nil || st != "queued" {
		t.Fatalf("被拒取消后写类任务应仍为 queued，实际 status=%q err=%v", st, err)
	}
	// 运维取消只读任务应成功。
	if code := cancelAs("operator", readTask); code != http.StatusOK {
		t.Fatalf("运维取消只读任务应返回 200，实际得到 %d", code)
	}
	// 管理员取消写类任务应成功。
	if code := cancelAs("admin", writeTask); code != http.StatusOK {
		t.Fatalf("管理员取消写类任务应返回 200，实际得到 %d", code)
	}
}

// 验证 3X-UI 与节点用户路径段拒绝 "." 和 ".." 防止路径穿越。
func TestXUIAndPortalUserIDRejectDotSegments(t *testing.T) {
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}

	for _, bad := range []string{".", ".."} {
		// 3X-UI 删除客户端路径段校验
		req := httptest.NewRequest(http.MethodDelete, "/api/xui/panels/p/inbounds/1/clients/x", nil)
		req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req.SetPathValue("inboundID", "1")
		req.SetPathValue("clientId", bad)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		rec := httptest.NewRecorder()
		s.xuiDeleteClient(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("xuiDeleteClient 对 %q 应返回 400，实际得到 %d", bad, rec.Code)
		}

		// 3X-UI 重置流量路径段校验
		req2 := httptest.NewRequest(http.MethodPost, "/api/xui/panels/p/inbounds/1/clients/x/reset", nil)
		req2.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req2.SetPathValue("inboundID", "1")
		req2.SetPathValue("email", bad)
		rec2 := httptest.NewRecorder()
		s.xuiResetClientTraffic(rec2, req2)
		if rec2.Code != http.StatusBadRequest {
			t.Fatalf("xuiResetClientTraffic 对 %q 应返回 400，实际得到 %d", bad, rec2.Code)
		}

		// 节点门户路径用户 ID 校验
		req3 := httptest.NewRequest(http.MethodDelete, "/api/nodes/n/users/x", nil)
		req3.SetPathValue("uid", bad)
		rec3 := httptest.NewRecorder()
		if _, ok := s.portalUserIDFromPath(rec3, req3); ok || rec3.Code != http.StatusBadRequest {
			t.Fatalf("portalUserIDFromPath 对 %q 应拒绝并返回 400，实际 ok=%v code=%d", bad, ok, rec3.Code)
		}

		// 节点门户创建用户 body 校验
		req4 := httptest.NewRequest(http.MethodPost, "/api/nodes/n/users", strings.NewReader(`{"user_id":"`+bad+`"}`))
		rec4 := httptest.NewRecorder()
		s.portalCreateUser(rec4, req4)
		if rec4.Code != http.StatusBadRequest {
			t.Fatalf("portalCreateUser 对 %q 应返回 400，实际得到 %d", bad, rec4.Code)
		}
	}
}

// 验证新版门户 proxy_<12位十六进制> 代理服务 ID 在删除接口中可通过校验。
func TestPortalProxyDeleteAcceptsPrefixedID(t *testing.T) {
	if !proxyPrefixedIDPattern.MatchString("proxy_0123456789ab") {
		t.Fatal("proxyPrefixedIDPattern 应接受合法前缀 ID")
	}
	// 非法 ID 必须在查库前返回 400。
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	req := httptest.NewRequest(http.MethodDelete, "/api/nodes/n/proxy-services/bad!", nil)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	req.SetPathValue("pid", "proxy_INVALID!")
	rec := httptest.NewRecorder()
	s.portalDeleteProxyService(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法代理 ID 应返回 400，实际得到 %d", rec.Code)
	}
	// 验证源码放行 proxyPrefixedIDPattern，确保新版 ID 不再被误拦。
	body := funcBlock(t, "portalproxy.go", "func (s *Server) portalDeleteProxyService(")
	if !strings.Contains(body, "proxyPrefixedIDPattern.MatchString(proxyID)") {
		t.Fatal("portalDeleteProxyService 必须同时放行 proxyPrefixedIDPattern")
	}
}

// 验证代理密码控制字符、SOCKS5 端口边界与出站控制字符校验。
func TestPortalActionAndOutboundValidation(t *testing.T) {
	spec := portalActions["manage_proxy"]
	if err := validateActionForm(spec, map[string]string{"action": "create", "password": "abc\r\nmalicious"}); err == nil {
		t.Fatal("manage-proxy 密码包含换行/控制字符时必须拒绝")
	}
	if err := validateActionForm(spec, map[string]string{"action": "create", "password": "safe_password_123"}); err != nil {
		t.Fatalf("合法代理密码不应被拒：%v", err)
	}

	// SOCKS5 端口越界与控制字符
	if err := validateOutboundForm(map[string]string{"action": "save", "name": "s1", "type": "socks5", "addr": "203.0.113.1:99999"}); err == nil {
		t.Fatal("SOCKS5 端口 99999 必须拒绝")
	}
	if err := validateOutboundForm(map[string]string{"action": "save", "name": "s1", "type": "socks5", "addr": "203.0.113.1:0"}); err == nil {
		t.Fatal("SOCKS5 端口 0 必须拒绝")
	}
	if err := validateOutboundForm(map[string]string{"action": "save", "name": "s1", "type": "socks5", "addr": "203.0.113.1:1080", "password": "ok\x00bad"}); err == nil {
		t.Fatal("出站密码包含空字节控制字符必须拒绝")
	}
	if err := validateOutboundForm(map[string]string{"action": "save", "name": "s1", "type": "socks5", "addr": "203.0.113.1:1080", "username": "u", "password": "p"}); err != nil {
		t.Fatalf("合法 SOCKS5 出站不应被拒：%v", err)
	}
}

// 验证流量统计接口地址的端口边界与路径穿越防护。
func TestValidTrafficEndpointStrictBounds(t *testing.T) {
	if !validTrafficEndpoint("", "") {
		t.Fatal("同时留空应合法")
	}
	if !validTrafficEndpoint("http://127.0.0.1:29862", "/etc/hysteria/traffic.secret") {
		t.Fatal("合法回环地址与绝对路径应通过")
	}
	for _, tc := range [][2]string{
		{"http://127.0.0.1:0", "/etc/hysteria/traffic.secret"},
		{"http://127.0.0.1:65536", "/etc/hysteria/traffic.secret"},
		{"http://127.0.0.1:99999", "/etc/hysteria/traffic.secret"},
		{"http://127.0.0.1:29862", "/etc/hysteria/../../root/.ssh/id_rsa"},
		{"http://127.0.0.1:29862", "/etc/../etc/shadow"},
	} {
		if validTrafficEndpoint(tc[0], tc[1]) {
			t.Fatalf("非法流量接口配置 (%q, %q) 必须拒绝", tc[0], tc[1])
		}
	}
}

// 验证迁移证书附件路径校验拦截路径穿越。
func TestMigrationTLSPathRejectsTraversal(t *testing.T) {
	for _, okPath := range []string{
		"/etc/hysteria/cert/server.crt",
		"/etc/letsencrypt/live/node.example.com/fullchain.pem",
		"/root/.acme.sh/node.example.com_ecc/fullchain.cer",
	} {
		if !validMigrationTLSPath(okPath) {
			t.Fatalf("合法证书路径 %q 应放行", okPath)
		}
	}
	for _, badPath := range []string{
		"/etc/hysteria/../../root/.ssh/authorized_keys",
		"/etc/hysteria/./cert.pem",
		"/etc/passwd",
		"/etc/hysteria/cert\n.pem",
		"/etc/hysteria/",
		"/etc/hysteria/..\\evil",
	} {
		if validMigrationTLSPath(badPath) {
			t.Fatalf("危险证书路径 %q 必须拒绝", badPath)
		}
	}
}

// 验证主机指纹确认拒绝空指纹与含控制字符的畸形指纹。
func TestAcceptServerFingerprintRejectsMalformed(t *testing.T) {
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	for _, payload := range []string{
		`{"expected_old":"SHA256:old","new":"SHA256:"}`,
		`{"expected_old":"SHA256:old","new":"SHA256:abc def"}`,
		`{"expected_old":"SHA256:old","new":"SHA256:abc\ndef"}`,
		`{"expected_old":"   ","new":"SHA256:validhash"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/servers/s/fingerprint/accept", strings.NewReader(payload))
		rec := httptest.NewRecorder()
		s.acceptServerFingerprint(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("畸形指纹 %s 应返回 400，实际得到 %d", payload, rec.Code)
		}
	}
}

// 验证 setDeliveryDomain 同时提交合法 name 与非法 value 时先校验全部参数，不会部分修改名称缓存。
func TestDeliveryDomainValidatesAllBeforePersisting(t *testing.T) {
	prevName, _ := subscriptionNameCache.Load().(string)
	t.Cleanup(func() { subscriptionNameCache.Store(prevName) })
	subscriptionNameCache.Store("OldName")

	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	req := httptest.NewRequest(http.MethodPut, "/api/settings/delivery-domain", strings.NewReader(`{"name":"NewName","value":"http://127.0.0.1:8080"}`))
	rec := httptest.NewRecorder()
	s.setDeliveryDomain(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法交付域名应返回 400，实际得到 %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := subscriptionNameCache.Load().(string); got != "OldName" {
		t.Fatalf("校验失败时不得修改名称缓存，期望 OldName，实际 %q", got)
	}
}

// 验证 subscriptionCache.load 在构建 panic 时不死锁并清理 inflight，在构建超时时不污染共享缓存。
func TestSubscriptionCacheLoadHandlesPanicAndTimeout(t *testing.T) {
	c := &subscriptionCache{}

	// 1. 构建函数 panic：不崩溃、不死锁、清理 inflight 且返回错误。
	if _, err := c.load(context.Background(), "k1", func(context.Context) *subscriptionBuild {
		panic("boom-test")
	}); err == nil {
		t.Fatal("resolve 发生 panic 时应返回错误")
	}
	c.mu.Lock()
	inflightLen := len(c.inflight)
	cached := c.build
	c.mu.Unlock()
	if inflightLen != 0 || cached != nil {
		t.Fatalf("panic 后必须清空 inflight 且不得写入缓存：inflight=%d build=%v", inflightLen, cached)
	}

	// 2. 构建上下文已超时：即使 resolve 返回部分构造对象也不得写入共享缓存。
	oldTimeout := subscriptionRebuildTimeout
	subscriptionRebuildTimeout = 15 * time.Millisecond
	t.Cleanup(func() { subscriptionRebuildTimeout = oldTimeout })

	partial := &subscriptionBuild{Nodes: []subscriptionNode{{ID: "partial"}}}
	got, err := c.load(context.Background(), "k2", func(bctx context.Context) *subscriptionBuild {
		<-bctx.Done()
		return partial
	})
	if err != nil || got != partial {
		t.Fatalf("超时返回部分构建结果时应返回本次结果而非挂死：got=%v err=%v", got, err)
	}
	c.mu.Lock()
	cachedAfterTimeout := c.build
	c.mu.Unlock()
	if cachedAfterTimeout != nil {
		t.Fatal("构建上下文超时后产生的残缺结果绝不能写入共享缓存")
	}
}

// 验证 syncSubscriptionNodeAttributes 对已软删节点标记 gone，使 onlyGone() 为真并保留可读节点名。
func TestSyncSubscriptionNodeAttributesMarksGoneNodes(t *testing.T) {
	s := repairSubscriptionDB(t)
	ctx := context.WithValue(context.Background(), migrationRequestKey{}, true)
	repairExec(t, s, `INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES($1,'已删节点甲',now())`, repairFirst)
	rec := &subscriptionRecord{nodeAccounts: map[string]subscriptionNodeAccount{
		repairFirst: {UserID: "sub_fixture_1"},
	}}

	out := s.syncSubscriptionNodeAttributes(ctx, rec)
	if !out.onlyGone() {
		t.Fatalf("全部失败节点均为已删除节点时 onlyGone() 必须为 true，实际：%#v", out)
	}
	if len(out.Gone) != 1 || out.Gone[0] != "已删节点甲" {
		t.Fatalf("Gone 列表应记录已删节点可读名称，实际：%#v", out.Gone)
	}
	if out.Failed["已删节点甲"] == "" {
		t.Fatalf("Failed 映射应保留已删节点可读名称，实际：%#v", out.Failed)
	}
}

// 验证 subscriptionBaseURL 与 subscriptionPublicURL 清洗畸形或注入型 X-Forwarded-Host。
func TestSubscriptionBaseURLSanitizesForwardedHost(t *testing.T) {
	prev := deliveryBaseCache.Load()
	deliveryBaseCache.Store("")
	t.Cleanup(func() {
		if prev != nil {
			deliveryBaseCache.Store(prev)
		}
	})
	t.Setenv("SUBSCRIPTION_PUBLIC_BASE_URL", "")

	for _, badHost := range []string{
		"evil.example.com/phish",
		"user@evil.example.com",
		"evil.example.com?x=1",
		"evil.example.com#frag",
		"evil.example.com:99999",
		"evil.example.com\r\nX-Injected: 1",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/subscription", nil)
		req.Host = "panel.example.com"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", badHost)
		got := subscriptionPublicURL(req, "tok123")
		if got != "https://panel.example.com/sub/tok123" {
			t.Fatalf("畸形 X-Forwarded-Host %q 必须被拒绝并回落到 r.Host，实际得到 %q", badHost, got)
		}
	}

	// 多代理逗号分隔场景仅取首个合法主机名。
	req := httptest.NewRequest(http.MethodGet, "/api/subscription", nil)
	req.Host = "internal:8080"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "outer.example.com, proxy.internal")
	if got := subscriptionPublicURL(req, "tok123"); got != "https://outer.example.com/sub/tok123" {
		t.Fatalf("多值 X-Forwarded-Host 应取首段合法值，实际得到 %q", got)
	}
}

// 验证 finishPendingNodeAction 事务化执行软删、黑名单登记与待办清理，黑名单失败时整笔回滚。
func TestFinishPendingNodeActionAtomicRevokeBlacklist(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const subID = "00000000-0000-4000-8000-000000000901"
	const subFailID = "00000000-0000-4000-8000-000000000902"
	// fixDB 已预建 subscription_tokens（含非空唯一 token 列）与 store_revoked_orders（迁移 056），此处仅补充待办动作表。
	if _, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(
	subscription_id uuid PRIMARY KEY,
	action text NOT NULL,
	then_delete boolean NOT NULL DEFAULT false,
	register_revoked_order boolean NOT NULL DEFAULT false,
	attempts int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	last_error text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO subscription_tokens(id,token,order_id,enabled) VALUES
 ('`+subID+`','tok-finish-ok-000001','OPS-prdord_ok000001',false),
 ('`+subFailID+`','tok-finish-fail-0001','OPS-prdord_fail0001',false);
INSERT INTO subscription_pending_node_actions(subscription_id,action,then_delete,register_revoked_order,next_attempt_at) VALUES
 ('`+subID+`','revoke',true,true,now()),
 ('`+subFailID+`','revoke',true,true,now());
`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)

	// 1. 正常场景：三张表原子提交。
	s.finishPendingNodeAction(ctx, &subscriptionRecord{ID: subID, OrderID: "OPS-prdord_ok000001"}, true, true)
	var deleted, blacklisted, pending int
	if err := db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid AND deleted_at IS NOT NULL),
		(SELECT count(*) FROM store_revoked_orders WHERE order_id='OPS-prdord_ok000001'),
		(SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid)`, subID).Scan(&deleted, &blacklisted, &pending); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || blacklisted != 1 || pending != 0 {
		t.Fatalf("正常结案应三步原子完成，实际 deleted=%d blacklisted=%d pending=%d", deleted, blacklisted, pending)
	}

	// 2. 破坏 store_revoked_orders 触发写入失败：软删与待办删除必须全部回滚，避免漏写黑名单。
	if _, err := db.Exec(ctx, `DROP TABLE store_revoked_orders`); err != nil {
		t.Fatal(err)
	}
	s.finishPendingNodeAction(ctx, &subscriptionRecord{ID: subFailID, OrderID: "OPS-prdord_fail0001"}, true, true)
	var failDeleted, failPending, failAttempts int
	if err := db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid AND deleted_at IS NOT NULL),
		(SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid),
		(SELECT attempts FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid)`, subFailID).Scan(&failDeleted, &failPending, &failAttempts); err != nil {
		t.Fatal(err)
	}
	if failDeleted != 0 || failPending != 1 || failAttempts != 1 {
		t.Fatalf("黑名单写入失败时必须回滚软删并保留待办重试，实际 deleted=%d pending=%d attempts=%d", failDeleted, failPending, failAttempts)
	}
}

// 验证所有新增环境变量解析函数的安全边界校验。
func TestEnvURLValidationBoundaries(t *testing.T) {
	// STORE_RENEW_BASE_URL
	for _, bad := range []string{
		"https://user:pass@shop.example.com",
		"https://shop.example.com?foo=bar",
		"https://shop.example.com#frag",
		"ftp://shop.example.com",
		"https://shop.example.com/\nevil",
		"https://shop.example.com/\x07bell",
	} {
		t.Setenv("STORE_RENEW_BASE_URL", bad)
		if got := storeRenewBaseURL(); got != "" {
			t.Fatalf("非法 STORE_RENEW_BASE_URL %q 应返回空串，实际 %q", bad, got)
		}
	}
	t.Setenv("STORE_RENEW_BASE_URL", "https://shop.example.com/renew")
	if got := storeRenewBaseURL(); got != "https://shop.example.com/renew/" {
		t.Fatalf("合法 STORE_RENEW_BASE_URL 解析错误：%q", got)
	}

	// PANEL_PUBLIC_URL：拒绝回环/私网/含凭据，放行公网域名。
	t.Setenv("SUBSCRIPTION_PUBLIC_BASE_URL", "")
	for _, bad := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://10.0.0.1",
		"https://admin:secret@panel.example.com",
		"https://internal-host",
	} {
		t.Setenv("PANEL_PUBLIC_URL", bad)
		if got := telegramPanelURL(); got != "" {
			t.Fatalf("不可外访或含凭据的 PANEL_PUBLIC_URL %q 应返回空串，实际 %q", bad, got)
		}
	}
	t.Setenv("PANEL_PUBLIC_URL", "https://panel.example.com/")
	if got := telegramPanelURL(); got != "https://panel.example.com" {
		t.Fatalf("合法 PANEL_PUBLIC_URL 应保留并去尾斜杠，实际 %q", got)
	}

	// BAN_WEBHOOK_URL：允许宿主机本地地址，但拒绝含凭据/查询串/锚点。
	for _, bad := range []string{
		"http://user:pass@127.0.0.1:9999",
		"http://127.0.0.1:9999?x=1",
		"http://127.0.0.1:9999#a",
	} {
		t.Setenv("BAN_WEBHOOK_URL", bad)
		if got := banWebhookBaseURL(); got != "" {
			t.Fatalf("非法 BAN_WEBHOOK_URL %q 应返回空串，实际 %q", bad, got)
		}
	}
	t.Setenv("BAN_WEBHOOK_URL", "http://127.0.0.1:19888/")
	if got := banWebhookBaseURL(); got != "http://127.0.0.1:19888" {
		t.Fatalf("合法 BAN_WEBHOOK_URL 解析错误：%q", got)
	}

	// HY2_INSTALL_SCRIPT_URL：仅接受公网 HTTPS 且不含凭据。
	for _, bad := range []string{
		"http://scripts.example.com/install.sh",
		"https://127.0.0.1/install.sh",
		"https://localhost/install.sh",
		"https://user:pass@scripts.example.com/install.sh",
		"https://scripts.example.com/install.sh 'bad'",
	} {
		t.Setenv("HY2_INSTALL_SCRIPT_URL", bad)
		if _, err := hy2InstallScriptURL(); err == nil {
			t.Fatalf("非法 HY2_INSTALL_SCRIPT_URL %q 必须拒绝", bad)
		}
	}
	t.Setenv("HY2_INSTALL_SCRIPT_URL", "https://scripts.example.com/install.sh")
	if got, err := hy2InstallScriptURL(); err != nil || got != "https://scripts.example.com/install.sh" {
		t.Fatalf("合法 HY2_INSTALL_SCRIPT_URL 应通过：got=%q err=%v", got, err)
	}

	// HY2_INSTALLER_REPO：拒绝 "."、".."、尾点或非法段。
	for _, bad := range []string{
		"./.",
		"../repo",
		"owner/..",
		"owner/repo.",
		"-owner/repo",
		"owner/repo/extra",
	} {
		t.Setenv("HY2_INSTALLER_REPO", bad)
		if _, err := portalInstallerRepo(); err == nil {
			t.Fatalf("非法 HY2_INSTALLER_REPO %q 必须拒绝", bad)
		}
	}
	t.Setenv("HY2_INSTALLER_REPO", "example-org/hysteria2-installer")
	if got, err := portalInstallerRepo(); err != nil || got != "example-org/hysteria2-installer" {
		t.Fatalf("合法 HY2_INSTALLER_REPO 应通过：got=%q err=%v", got, err)
	}
}

// 验证 Telegram /api 只读识别不被正文伪造绕过，以及向导预览全面脱敏凭据字段。
func TestTelegramStaleWriteAndWizardPreviewSanitization(t *testing.T) {
	if !isTelegramReadOnlyAPI("/api GET /api/nodes") {
		t.Fatal("正常 GET 命令应判为只读")
	}
	if !isTelegramReadOnlyAPI("/api@mybot get /api/nodes") {
		t.Fatal("带机器人后缀的小写 get 命令应判为只读")
	}
	if isTelegramReadOnlyAPI(`/api POST /api/nodes {"name":"fake GET method"}`) {
		t.Fatal("POST 命令正文里包含 GET 时绝不能被误判为只读命令")
	}
	if isTelegramReadOnlyAPI(`/api DELETE /api/nodes/1 {"note":" GET "}`) {
		t.Fatal("DELETE 命令正文里包含 GET 时绝不能被误判为只读命令")
	}

	raw, _ := json.Marshal(map[string]any{
		"api_key":       "secret-api-key-value",
		"private_key":   "-----BEGIN PRIVATE KEY-----",
		"cookie":        "sid=very-secret",
		"authorization": "Bearer abcdef",
		"credential":    "top-secret-cred",
		"name":          "public-name",
	})
	preview := telegramWizardPreview(raw)
	for _, leak := range []string{"secret-api-key-value", "BEGIN PRIVATE KEY", "very-secret", "Bearer abcdef", "top-secret-cred"} {
		if strings.Contains(preview, leak) {
			t.Fatalf("向导预览泄露敏感字段 %q：\n%s", leak, preview)
		}
	}
	if !strings.Contains(preview, "public-name") {
		t.Fatalf("非敏感字段应正常展示：\n%s", preview)
	}
}

// 验证 lockNodeRead 与 terminalGate 对大小写 UUID 共用同一并发槽位。
func TestCanonicalUUIDLockConsistency(t *testing.T) {
	s := &Server{}
	lower := "00000000-0000-4000-8000-000000000abc"
	upper := strings.ToUpper(lower)

	// 占满 4 个读槽位（混合大小写 UUID），第 5 个必须因同键满槽而超时失败。
	releases := make([]func(), 0, nodeReadConcurrency)
	for i := 0; i < nodeReadConcurrency; i++ {
		id := lower
		if i%2 == 1 {
			id = upper
		}
		rel, err := s.lockNodeRead(context.Background(), id)
		if err != nil {
			t.Fatalf("第 %d 次获取读槽位失败：%v", i+1, err)
		}
		releases = append(releases, rel)
	}
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()

	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.lockNodeRead(shortCtx, upper); err == nil {
		t.Fatal("大小写不同但同一 UUID 不得绕过读并发上限")
	}
}

// 验证 loginGuard 在封禁表超过上限时主动淘汰最早解封条目，防止分布式爆破耗尽内存。
func TestLoginGuardEvictsBannedOverflow(t *testing.T) {
	g := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < maxLoginGuardEntries+50; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", i/256, i%256)
		g.banned[ip] = now.Add(time.Duration(i+1) * time.Second)
	}
	g.mu.Lock()
	g.evictBannedLocked(now, "keep-me")
	size := len(g.banned)
	_, oldestStillThere := g.banned["203.0.0.0"]
	g.mu.Unlock()
	if size > maxLoginGuardEntries {
		t.Fatalf("封禁表淘汰后不应超过上限 %d，实际 %d", maxLoginGuardEntries, size)
	}
	if oldestStillThere {
		t.Fatal("最早解封的条目应被优先淘汰")
	}
}

// 验证 setCNProbe 将大写主机 UUID 规范为小写存储，确保与 blockedNodeConditionsSQL 匹配。
func TestSetCNProbeNormalizesLowercaseUUID(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS server_credentials(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), server_id uuid);
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO server_credentials(server_id) VALUES('`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)

	upper := strings.ToUpper(fixHost)
	req := httptest.NewRequest(http.MethodPut, "/api/settings/cn-probe", strings.NewReader(`{"server_id":"`+upper+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: adminA, Role: "admin"}))
	rec := httptest.NewRecorder()
	s.setCNProbe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setCNProbe 应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var saved string
	if err := db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, cnProbeSettingKey).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != fixHost {
		t.Fatalf("写入 system_settings 的探测点 ID 必须规范为小写 UUID %q，实际得到 %q", fixHost, saved)
	}
}

// 无数据库环境下补充校验关键权限门禁、事务原子性与 gone 识别逻辑不被后续重构误删。
func TestSwarmAuditStaticSafetyContracts(t *testing.T) {
	upd := funcBlock(t, "admin.go", "func (s *Server) updateNode(")
	if !strings.Contains(upd, "sensitiveChanged && claims.Role != \"admin\"") || !strings.Contains(upd, `s.auditDenied(r, "forbidden_admin_only")`) {
		t.Fatal("updateNode 必须保留非管理员敏感字段变更拦截与拒绝审计")
	}
	cancel := funcBlock(t, "manage.go", "func (s *Server) cancelTask(")
	if !strings.Contains(cancel, "!readOnlyOperations[operation] && claims.Role != \"admin\"") || !strings.Contains(cancel, `s.auditDenied(r, "forbidden_admin_only")`) {
		t.Fatal("cancelTask 必须保留写类任务仅限管理员取消与拒绝审计")
	}
	attrs := funcBlock(t, "subscription_attributes.go", "func (s *Server) syncSubscriptionNodeAttributes(")
	if !strings.Contains(attrs, "errors.Is(err, errPortalNodeNotFound)") || !strings.Contains(attrs, "collectSubscriptionStatusResults(results)") {
		t.Fatal("syncSubscriptionNodeAttributes 必须识别已删除节点 gone 状态并通过 collectSubscriptionStatusResults 汇总")
	}
	finish := funcBlock(t, "pending_node_actions.go", "func (s *Server) finishPendingNodeAction(")
	if !strings.Contains(finish, "tx, err := s.db.Begin(") || !strings.Contains(finish, "tx.Commit(") {
		t.Fatal("finishPendingNodeAction 必须在单事务内原子提交软删、黑名单登记与待办清理")
	}
}
