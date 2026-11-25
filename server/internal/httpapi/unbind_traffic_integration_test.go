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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/auth"
	"nodeops/server/internal/secrets"
	"nodeops/server/internal/speedtest"
)

// 2026-11-03：超额停用中的节点不能解绑（解绑后面板不会再为它发恢复请求）；
// 正常解绑要清掉计量基线，重新绑定到另一台机器时不会把对方开机以来的流量一次算进来。
func TestUnbindRefusesSuspendedAndClearsBaseline(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN users_count int, ADD COLUMN api_checked_at timestamptz,
 ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text,
 ADD COLUMN portal_caps jsonb, ADD COLUMN portal_checked_at timestamptz,
 ADD COLUMN traffic_counter_rx bigint, ADD COLUMN traffic_counter_tx bigint, ADD COLUMN traffic_boot_id text;
INSERT INTO hysteria_nodes(id,name,api_url,api_key,traffic_counter_rx,traffic_counter_tx,traffic_boot_id,traffic_suspended)
 VALUES('`+fixNode+`','n','https://8.8.8.8:8443','k'::bytea,111,222,'boot|eth0',true)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	unbind := func() int {
		req := httptest.NewRequest(http.MethodDelete, "/api/nodes/x/api", nil)
		req.SetPathValue("id", fixNode)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		rec := httptest.NewRecorder()
		s.unbindNodeAPI(rec, req)
		return rec.Code
	}
	if code := unbind(); code != http.StatusConflict {
		t.Fatalf("unbinding a traffic-suspended node: want 409, got %d", code)
	}
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET traffic_suspended=false`); err != nil {
		t.Fatal(err)
	}
	// 2026-11-06：仍有订阅在这台节点上开着账号时不能解绑（解绑后这些账号永远回收不了）。
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,node_accounts) VALUES($1::uuid,'fixture-token-unbind-guard-01',$2::jsonb)`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_x","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	if code := unbind(); code != http.StatusConflict {
		t.Fatalf("unbinding a node that still carries subscription accounts: want 409, got %d", code)
	}
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	if code := unbind(); code != http.StatusOK {
		t.Fatalf("unbind: want 200, got %d", code)
	}
	var rx *int64
	var boot *string
	if err := db.QueryRow(ctx, `SELECT traffic_counter_rx, traffic_boot_id FROM hysteria_nodes`).Scan(&rx, &boot); err != nil {
		t.Fatal(err)
	}
	if rx != nil || boot != nil {
		t.Fatalf("unbind must clear the traffic baseline, got rx=%v boot=%v", rx, boot)
	}
}

// 2026-11-03：主机正在安装 / 迁移时（持有 host_operation_leases），不允许解绑或换绑节点 API。
func TestBindAndUnbindRefusedDuringHostMaintenance(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id) VALUES('`+fixNode+`','n','https://8.8.8.8:8443','k'::bytea,'`+fixHost+`');
ALTER TABLE host_operation_leases DROP CONSTRAINT IF EXISTS host_operation_leases_job_id_fkey;
INSERT INTO host_operation_leases(server_id,job_id) VALUES('`+fixHost+`','job-1')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodDelete, "/api/nodes/x/api", nil)
	req.SetPathValue("id", fixNode)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.unbindNodeAPI(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unbind during maintenance: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/nodes/x/api", strings.NewReader(`{"api_url":"https://8.8.4.4:8443","api_key":"fixture-node-api-key-at-least-16"}`))
	req.SetPathValue("id", fixNode)
	rec = httptest.NewRecorder()
	s.bindNodeAPI(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("bind during maintenance: want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

// 2026-11-04：安装 / 迁移进行中删除节点必须 409，且节点与凭据原样保留。
func TestDeleteNodeRefusedDuringHostMaintenance(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id) VALUES('`+fixNode+`','n','https://8.8.8.8:8443','k'::bytea,'`+fixHost+`');
ALTER TABLE host_operation_leases DROP CONSTRAINT IF EXISTS host_operation_leases_job_id_fkey;
INSERT INTO host_operation_leases(server_id,job_id) VALUES('`+fixHost+`','job-1')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodDelete, "/api/nodes/x", nil)
	req.SetPathValue("id", fixNode)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.deleteNode(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete during maintenance: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	var deleted bool
	var apiURL *string
	if err := db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, api_url FROM hysteria_nodes WHERE id=$1::uuid`, fixNode).Scan(&deleted, &apiURL); err != nil {
		t.Fatal(err)
	}
	if deleted || apiURL == nil {
		t.Fatalf("node must stay intact, deleted=%v api_url=%v", deleted, apiURL)
	}
}

// 2026-11-04：读对接密钥失败不能当成「尚未生成密钥」（503 + 引导重新生成），必须是 500。
func TestStoreSecretReadFailureIsNotUnconfigured(t *testing.T) {
	db := fixDB(t)
	if _, err := db.Exec(context.Background(), `DROP TABLE system_settings`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	called := false
	h := s.requireStoreKey(func(http.ResponseWriter, *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/api/store/orders", nil)
	req.Header.Set("X-Store-Key", "whatever")
	rec := httptest.NewRecorder()
	h(rec, req)
	if called || rec.Code != http.StatusInternalServerError {
		t.Fatalf("store key read failure must be 500, got %d %s", rec.Code, rec.Body.String())
	}
	if _, err := s.storeSecret(context.Background()); err == nil {
		t.Fatal("storeSecret must report the read error")
	}
}

// 2026-11-04：按 slug 解析分类时，查询失败必须作为错误上报，不能当成「分类不存在」。
func TestResolveCategorySlugReportsDBErrors(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	// fixture 里没有 node_categories 表：查询必然出错。
	if _, _, ok, err := s.resolveCategoryBySlug(context.Background(), "ai"); err == nil || ok {
		t.Fatalf("a failing lookup must return an error, got ok=%v err=%v", ok, err)
	}
	if _, err := db.Exec(context.Background(), `CREATE TABLE node_categories(id uuid PRIMARY KEY, slug text UNIQUE, enabled boolean DEFAULT true)`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.resolveCategoryBySlug(context.Background(), "ai"); err != nil || ok {
		t.Fatalf("a missing slug is not an error, got ok=%v err=%v", ok, err)
	}
}

// 2026-11-04：设置国内探测点时，查询失败必须是 500、ID 格式错误是 400，不能都报「主机不存在」。
func TestSetCNProbeSeparatesErrors(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	call := func(body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/settings/cn-probe", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.setCNProbe(rec, req)
		return rec.Code
	}
	if code := call(`{"server_id":"not-a-uuid"}`); code != http.StatusBadRequest {
		t.Fatalf("malformed id: want 400, got %d", code)
	}
	// fixture 没有 server_credentials 表：查询必然失败。
	if code := call(`{"server_id":"` + fixHost + `"}`); code != http.StatusInternalServerError {
		t.Fatalf("query failure: want 500, got %d", code)
	}
}

// 2026-11-04：路径里的畸形 UUID（PostgreSQL 22P02）是调用方错误，必须 400，不能是 500。
func TestMalformedUUIDIsBadRequest(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	_, err := db.Exec(context.Background(), `SELECT 1 FROM servers WHERE id=$1::uuid`, "not-a-uuid")
	if err == nil {
		t.Fatal("expected a cast error")
	}
	rec := httptest.NewRecorder()
	s.databaseError(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil), err)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("22P02 must map to 400, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.databaseError(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil), errors.New("boom"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("other database errors stay 500, got %d", rec.Code)
	}
}

// 2026-11-04：主机有安装 / 迁移租约时不能删除主机。
func TestDeleteServerRefusedDuringHostJob(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`');
ALTER TABLE host_operation_leases DROP CONSTRAINT IF EXISTS host_operation_leases_job_id_fkey;
INSERT INTO host_operation_leases(server_id,job_id) VALUES('`+fixHost+`','job-1')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodDelete, "/api/servers/x", nil)
	req.SetPathValue("id", fixHost)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.deleteServer(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete during a host job: want 409, got %d %s", rec.Code, rec.Body.String())
	}
	var deleted bool
	if err := db.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM servers WHERE id=$1::uuid`, fixHost).Scan(&deleted); err != nil || deleted {
		t.Fatalf("host must stay, deleted=%v err=%v", deleted, err)
	}
}

// 2026-11-04：已删除主机不能再新建 / 重试任务（执行器不会领取，任务会永远排队）。
func TestTasksRefusedForDeletedHost(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN node_id uuid, ADD COLUMN operation_type text, ADD COLUMN parameters jsonb NOT NULL DEFAULT '{}';
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO operation_tasks(id,server_id,status,operation_type) VALUES('00000000-0000-0000-0000-0000000005a1','`+fixHost+`','failed','health_check')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	create := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"server_id":"`+fixHost+`","operation_type":"health_check"}`))
		rec := httptest.NewRecorder()
		s.createTask(rec, req)
		return rec.Code
	}
	retry := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/x/retry", nil)
		req.SetPathValue("id", "00000000-0000-0000-0000-0000000005a1")
		rec := httptest.NewRecorder()
		s.retryTask(rec, req)
		return rec.Code
	}
	if code := create(); code != http.StatusCreated {
		t.Fatalf("live host: want 201, got %d", code)
	}
	if code := retry(); code != http.StatusCreated {
		t.Fatalf("live host retry: want 201, got %d", code)
	}
	if _, err := db.Exec(ctx, `UPDATE servers SET deleted_at=now() WHERE id=$1::uuid`, fixHost); err != nil {
		t.Fatal(err)
	}
	if code := create(); code != http.StatusNotFound {
		t.Fatalf("deleted host: want 404, got %d", code)
	}
	if code := retry(); code != http.StatusConflict {
		t.Fatalf("deleted host retry: want 409, got %d", code)
	}
}

// 2026-11-04：新建用户按不区分大小写查重（种子管理员的邮箱按配置原样存，可能含大写）。
func TestCreateUserRejectsCaseInsensitiveDuplicate(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE users(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE, password_hash text, role text, created_at timestamptz DEFAULT now());
INSERT INTO users(email,password_hash,role) VALUES('Admin@Example.com','x','admin')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(`{"email":"admin@example.com","password":"Fixture-Passw0rd-long","role":"viewer"}`))
	rec := httptest.NewRecorder()
	s.createUser(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a case-only duplicate must be 409, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("no second account may be created, got %d err=%v", n, err)
	}
}

// 2026-11-04：同一范围的手动测速进行中时，再次触发必须 409，不能在后台叠加一轮；其它节点不受影响。
func TestManualSpeedtestDoesNotStack(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE server_credentials(server_id uuid);
INSERT INTO servers(id,name) VALUES('`+fixHost+`','h');
INSERT INTO hysteria_nodes(id,name,server_id) VALUES('`+fixNode+`','n','`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	s.prober = speedtest.NewProber(s.logger, db, nil)
	manualSpeedtestRunning.Store("", struct{}{})
	t.Cleanup(func() { manualSpeedtestRunning.Delete("") })
	call := func(q string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/speedtest/run"+q, nil)
		rec := httptest.NewRecorder()
		s.speedtestRun(rec, req)
		return rec.Code
	}
	if code := call(""); code != http.StatusConflict {
		t.Fatalf("a second full run while one is in progress must be 409, got %d", code)
	}
	// 另一个范围（单个节点）：这台节点两条通道都不可用 ⇒ 200 + started=0，且不受全量那轮影响。
	if code := call("?node_id=" + fixNode); code != http.StatusOK {
		t.Fatalf("another scope must not be blocked, got %d", code)
	}
}

// 没有后台任务要启动的返回（这里：两条通道都不可用 ⇒ started=0）必须释放占位，否则这个范围永远 409。
func TestManualSpeedtestReleasesScopeWhenNothingStarted(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE server_credentials(server_id uuid);
INSERT INTO servers(id,name) VALUES('`+fixHost+`','h');
INSERT INTO hysteria_nodes(id,name,server_id) VALUES('`+fixNode+`','n','`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	s.prober = speedtest.NewProber(s.logger, db, nil)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/speedtest/run?node_id="+fixNode, nil)
		rec := httptest.NewRecorder()
		s.speedtestRun(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: nothing was started, so the scope must be free again; got %d", i+1, rec.Code)
		}
	}
}

// 2026-11-04：流量限额只接受 null/0（不限）与正数；负数、截断成 0 字节的值、畸形节点 id 都是 400。
func TestNodeTrafficLimitRejectsBadValues(t *testing.T) {
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	for _, tc := range []struct{ id, body string }{
		{fixNode, `{"limit_gb":-5,"reset_day":1}`},
		{fixNode, `{"limit_gb":1e-10,"reset_day":1}`},
		{"not-a-uuid", `{"limit_gb":10,"reset_day":1}`},
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/nodes/x/traffic-limit", strings.NewReader(tc.body))
		req.SetPathValue("id", tc.id)
		rec := httptest.NewRecorder()
		s.updateNodeTrafficLimit(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: want 400, got %d", tc.id, tc.body, rec.Code)
		}
	}
}

// 2026-11-04：改密作废旧令牌，但必须为本会话签发一枚新令牌（否则下一个请求就 401、被踢回登录页）。
func TestChangePasswordReturnsFreshToken(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	hash, err := auth.HashPassword("Fixture-Old-Passw0rd")
	if err != nil {
		t.Fatal(err)
	}
	var uid string
	if _, err := db.Exec(ctx, `CREATE TABLE users(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE, password_hash text, role text, token_version bigint NOT NULL DEFAULT 0, created_at timestamptz DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO users(email,password_hash,role) VALUES('a@b.c',$1,'admin') RETURNING id::text`, hash).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	s.signer = auth.NewSigner("change-password-test-secret-long", time.Hour)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	old, _ := s.signer.IssueVersion(uid, "a@b.c", "admin", 0)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/change-password", strings.NewReader(`{"current_password":"Fixture-Old-Passw0rd","new_password":"Fixture-New-Passw0rd"}`))
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: uid, Email: "a@b.c", Role: "admin"}))
	rec := httptest.NewRecorder()
	s.changePassword(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("a fresh token must be returned, got %s", rec.Body.String())
	}
	check := func(token string) error {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		_, err := s.authenticate(r)
		return err
	}
	if err := check(out.Token); err != nil {
		t.Fatalf("the fresh token must authenticate: %v", err)
	}
	if err := check(old); err == nil {
		t.Fatal("the pre-change token must be revoked")
	}
}

// 2026-11-04：管理员不能用「重置他人密码」（不验证当前密码）改自己的密码。
func TestResetPasswordRefusesSelf(t *testing.T) {
	const self = "00000000-0000-0000-0000-0000000007a1"
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	req := httptest.NewRequest(http.MethodPost, "/api/users/x/password", strings.NewReader(`{"password":"Fixture-New-Passw0rd"}`))
	req.SetPathValue("id", strings.ToUpper(self))
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: self, Role: "admin"}))
	rec := httptest.NewRecorder()
	s.resetUserPassword(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("resetting one's own password through the admin path must be refused, got %d", rec.Code)
	}
}

// 2026-11-04：发起了运行中批量门户更新的账号不能删（批次行 ON DELETE CASCADE，滚动更新会停在半路）。
func TestDeleteUserRefusedWhileBatchRuns(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE users(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE, password_hash text, role text, token_version bigint NOT NULL DEFAULT 0, created_at timestamptz DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"029_portal_update_batches.sql", "030_portal_batch_lease.sql"} {
		raw, err := os.ReadFile(filepath.Join("../../migrations", m))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	const me, victim = "00000000-0000-0000-0000-0000000007b1", "00000000-0000-0000-0000-0000000007b2"
	if _, err := db.Exec(ctx, `INSERT INTO users(id,email,role) VALUES($1::uuid,'me@x.io','admin'),($2::uuid,'op@x.io','operator');
`, me, victim); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO portal_update_batches(created_by,status,results,expected) VALUES($1::uuid,'running','[]','{}')`, victim); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	req := httptest.NewRequest(http.MethodDelete, "/api/users/x", nil)
	req.SetPathValue("id", victim)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: me, Role: "admin"}))
	rec := httptest.NewRecorder()
	s.deleteUser(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 while the user's batch runs, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM portal_update_batches`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the running batch must survive, got %d err=%v", n, err)
	}
}

// 2026-11-04：订阅圈定节点时，同一节点的大小写两种写法要合并，不能被误判为「有不存在的节点」。
func TestSubscriptionNodeIDsAreCanonicalised(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	// 必须带字母：全数字的 UUID 大小写写法相同，测不出这个问题。
	const node = "00000000-0000-0000-0000-0000000abcde"
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name) VALUES($1::uuid,'n')`, node); err != nil {
		t.Fatal(err)
	}
	ids, err := fixServer(db).validateSubscriptionNodeIDs(ctx, []string{node, strings.ToUpper(node)})
	if err != nil || len(ids) != 1 || ids[0] != node {
		t.Fatalf("case variants of one node must collapse to one id, got %v err=%v", ids, err)
	}
}

// 2026-11-04：续期到已经过去的时间必须 400，不能重新启用订阅、更不能作废待办的回收。
func TestStoreRenewRejectsPastExpiry(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/store/renew", strings.NewReader(`{"order_id":"o1","expires_at":1600000000}`))
	rec := httptest.NewRecorder()
	s.storeRenewOrder(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a renewal to a past expiry must be 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// 2026-11-04：机主账号、以及仍被在用订阅映射着的订阅账号，不能通过「删除节点用户」删掉；残留的 sub_* 照常可删。
func TestPortalDeleteUserProtectsManagedAccounts(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,node_accounts) VALUES($1::uuid,'fixture-token-delete-guard-01',$2::jsonb)`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_live","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	nodeID := fixNode
	call := func(uid string) int {
		req := httptest.NewRequest(http.MethodDelete, "/api/nodes/x/users/"+uid, nil)
		req.SetPathValue("id", nodeID)
		req.SetPathValue("uid", uid)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		rec := httptest.NewRecorder()
		s.portalDeleteUser(rec, req)
		return rec.Code
	}
	if code := call("admin_master"); code != http.StatusBadRequest {
		t.Errorf("admin_master: want 400, got %d", code)
	}
	if code := call("sub_live"); code != http.StatusConflict {
		t.Errorf("mapped sub_ account: want 409, got %d", code)
	}
	// 2026-11-06：节点 id 写成大写不能绕过映射检查。
	nodeID = strings.ToUpper(fixNode)
	if code := call("sub_live"); code != http.StatusConflict {
		t.Errorf("mapped sub_ account via upper-case node id: want 409, got %d", code)
	}
	nodeID = fixNode
	// 残留账号走到「读节点凭据」：fixture 里没有这台节点 ⇒ 404，说明没被订阅保护挡住。
	if code := call("sub_leftover"); code != http.StatusNotFound {
		t.Errorf("unmapped leftover sub_ account must not be blocked, got %d", code)
	}
}

type countingPortal struct {
	mu sync.Mutex
	n  int
}

func (c *countingPortal) RoundTrip(*http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil, errors.New("node unreachable")
}

// 2026-11-04：只改标签（带着原样的 ip_limit / 到期时间）不能逐节点同步属性、也不能报「部分节点失败」。
func TestSubscriptionLabelEditDoesNotTouchNodes(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,expires_at,ip_limit,node_accounts) VALUES($1::uuid,'fixture-token-label-edit-0001',date_trunc('second', now())+interval '10 days',3,$2::jsonb)`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_x","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := db.QueryRow(ctx, `SELECT expires_at FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	portal := &countingPortal{}
	old := portalClient
	portalClient = &http.Client{Transport: portal}
	t.Cleanup(func() { portalClient = old })
	body := fmt.Sprintf(`{"label":"新名字","ip_limit":3,"expires_at":%d}`, expires.Unix())
	req := httptest.NewRequest(http.MethodPatch, "/api/subscriptions/x", strings.NewReader(body))
	req.SetPathValue("id", fixSub)
	rec := httptest.NewRecorder()
	s.updateSubscriptionHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK  bool   `json:"node_state_ok"`
		Msg string `json:"node_state_message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if portal.n != 0 || !out.OK || out.Msg != "" {
		t.Fatalf("an unchanged ip_limit / expiry must not touch nodes: calls=%d ok=%v msg=%q", portal.n, out.OK, out.Msg)
	}
}

// 2026-11-04：删除节点时回收失败，但删除前取到了凭据快照 ⇒ 207 必须说明可在「孤儿账号」补删（retryable=true）。
func TestDeleteNodeReportsRetryableOrphans(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("../../migrations", "023_orphan_node_accounts.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 023: %v", err)
	}
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN status text, ADD COLUMN status_error text, ADD COLUMN users_count int, ADD COLUMN api_checked_at timestamptz;
CREATE TABLE alerts(node_id uuid, status text, resolved_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,$3::uuid)`, fixNode, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,node_accounts) VALUES($1::uuid,'fixture-token-delete-node-0001',$2::jsonb)`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_x","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	old := portalClient
	portalClient = &http.Client{Transport: &countingPortal{}}
	t.Cleanup(func() { portalClient = old })
	req := httptest.NewRequest(http.MethodDelete, "/api/nodes/x", nil)
	req.SetPathValue("id", fixNode)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.deleteNode(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("want 207, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Retryable bool   `json:"retryable"`
		Error     string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Retryable || strings.Contains(out.Error, "无法自动重试") {
		t.Fatalf("a snapshot was taken, so the orphans are retryable: %s", rec.Body.String())
	}
	var withKey bool
	if err := db.QueryRow(ctx, `SELECT api_key_cipher IS NOT NULL FROM orphan_node_accounts WHERE user_id='sub_x'`).Scan(&withKey); err != nil || !withKey {
		t.Fatalf("the orphan must carry the key snapshot, got %v err=%v", withKey, err)
	}
}

// 2026-11-04：到期结案时数据库出错必须 500，且响应里不能带 pgx 原文（表 / 列 / SQLSTATE）。
func TestCloseExpiredDatabaseErrorIsInternal(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,expires_at) VALUES($1::uuid,'fixture-token-closeout-db-err1',now()-interval '1 day')`, fixSub); err != nil {
		t.Fatal(err)
	}
	// 让读取订阅那一步失败：删掉 loadSubscriptionByID 要读的列。
	if _, err := db.Exec(ctx, `ALTER TABLE subscription_tokens DROP COLUMN note`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodPost, "/api/subscriptions/x/close-expired", nil)
	req.SetPathValue("id", fixSub)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.closeExpiredSubscriptionHandler(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a database failure must be 500, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SQLSTATE") || strings.Contains(rec.Body.String(), "column") {
		t.Fatalf("the response must not leak the database error text: %s", rec.Body.String())
	}
}

// 2026-11-04：没有门户网页账号密码的节点不能上架商城（买家订阅里生成不出它的配置）。
func TestListingRequiresPortalBasicCredentials(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text, ADD COLUMN status text, ADD COLUMN last_checked_at timestamptz, ADD COLUMN consecutive_probe_failures int DEFAULT 0;
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id,in_store) VALUES('`+fixNode+`','n','https://8.8.8.8:8443','k'::bytea,'`+fixHost+`',false)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	call := func() int {
		req := httptest.NewRequest(http.MethodPatch, "/api/nodes/x", strings.NewReader(`{"in_store":true}`))
		req.SetPathValue("id", fixNode)
		rec := httptest.NewRecorder()
		s.updateNode(rec, req)
		return rec.Code
	}
	if code := call(); code != http.StatusBadRequest {
		t.Fatalf("an API-only node without portal Basic credentials must not be listed, got %d", code)
	}
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET portal_basic_user='u', portal_basic_password='p'::bytea, portal_path_token='0123456789abcdef0123456789abcdef' WHERE id=$1::uuid`, fixNode); err != nil {
		t.Fatal(err)
	}
	if code := call(); code != http.StatusOK {
		t.Fatalf("with Basic credentials the node can be listed, got %d", code)
	}
}

// 2026-11-06：删除主机时就地归档它的告警，不记 resolved 事件；ReconcileAlerts 对主体已删除的告警同样只归档。
// 原来留给 reconcile 按「条件消失」结案并记 resolved 事件 —— Telegram 随即推一条「[恢复] SSH 已恢复连接」。
func TestDeleteServerArchivesItsAlertsWithoutRecovery(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN finished_at timestamptz, ADD COLUMN result_summary text;
CREATE TABLE alerts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz);
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO alerts(server_id,type,status,first_seen_at) VALUES('`+fixHost+`','server_offline','open',now()-interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodDelete, "/api/servers/x", nil)
	req.SetPathValue("id", fixHost)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.deleteServer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete server: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var open int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE server_id=$1::uuid AND status<>'resolved'`, fixHost).Scan(&open); err != nil || open != 0 {
		t.Fatalf("host alerts must be archived on delete, open=%d err=%v", open, err)
	}

	body := funcBlock(t, "manage.go", "func (s *Server) ReconcileAlerts(")
	for _, want := range []string{"AS subject_gone", "if subjectGone {", "for _, id := range gone {"} {
		if !strings.Contains(body, want) {
			t.Fatalf("ReconcileAlerts 必须把主体已删除的告警只归档、不记 resolved 事件，缺 %q", want)
		}
	}
	gone := body[strings.Index(body, "for _, id := range gone {"):]
	if end := strings.Index(gone, "\n\t}\n"); end > 0 {
		gone = gone[:end]
	}
	if strings.Contains(gone, "recordAlertEvent") {
		t.Fatal("主体已删除的告警不能记 resolved 事件（会推送「已恢复」）")
	}
}

// 2026-11-06：GET /api/tasks?ids= 按 id 取任务，不受「最近 N 条」限制（批量预设任务的进度轮询用）。
func TestTasksFilterByIDs(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN node_id uuid, ADD COLUMN operation_type text DEFAULT 'health_check', ADD COLUMN parameters jsonb NOT NULL DEFAULT '{}',
 ADD COLUMN result_summary text NOT NULL DEFAULT '', ADD COLUMN error_code text, ADD COLUMN created_at timestamptz NOT NULL DEFAULT now(), ADD COLUMN started_at timestamptz, ADD COLUMN finished_at timestamptz`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id,name) VALUES('`+fixHost+`','h')`); err != nil {
		t.Fatal(err)
	}
	const old = "00000000-0000-0000-0000-0000000005b1"
	if _, err := db.Exec(ctx, `INSERT INTO operation_tasks(id,server_id,status,created_at) VALUES($1::uuid,$2::uuid,'running',now()-interval '1 day')`, old, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO operation_tasks(server_id,status) SELECT $1::uuid,'success' FROM generate_series(1,120)`, fixHost); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	get := func(url string) []map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		s.tasks(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", url, rec.Code, rec.Body.String())
		}
		var out []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if all := get("/api/tasks"); len(all) != 100 {
		t.Fatalf("unfiltered list stays capped at 100, got %d", len(all))
	}
	got := get("/api/tasks?ids=" + old)
	if len(got) != 1 || got[0]["id"] != old {
		t.Fatalf("an old task must be found by id, got %v", got)
	}
	rec := httptest.NewRecorder()
	s.tasks(rec, httptest.NewRequest(http.MethodGet, "/api/tasks?ids=not-a-uuid", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ids: want 400, got %d", rec.Code)
	}
}

// 2026-11-06：编辑节点时只有真的换主机 / 改端口才重置状态；带着原值保存（改名、切商城）不能把离线节点变回「未知」。
func TestUpdateNodeResetsStatusOnlyOnRealChange(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text, ADD COLUMN status text, ADD COLUMN last_checked_at timestamptz, ADD COLUMN consecutive_probe_failures int DEFAULT 0, ADD COLUMN version text;
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id,in_store,listen_port,status,consecutive_probe_failures,last_checked_at)
 VALUES('`+fixNode+`','n','https://8.8.8.8:8443','k'::bytea,'`+fixHost+`',false,443,'offline',5,now())`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	call := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/nodes/x", strings.NewReader(body))
		req.SetPathValue("id", fixNode)
		rec := httptest.NewRecorder()
		s.updateNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
		}
	}
	state := func() (string, int) {
		t.Helper()
		var status string
		var failures int
		if err := db.QueryRow(ctx, `SELECT status, consecutive_probe_failures FROM hysteria_nodes WHERE id=$1::uuid`, fixNode).Scan(&status, &failures); err != nil {
			t.Fatal(err)
		}
		return status, failures
	}
	call(`{"name":"renamed","server_id":"` + strings.ToUpper(fixHost) + `","listen_port":443}`)
	if status, failures := state(); status != "offline" || failures != 5 {
		t.Fatalf("a rename with unchanged host/port must keep the status, got %s/%d", status, failures)
	}
	call(`{"listen_port":8443}`)
	if status, failures := state(); status != "unknown" || failures != 0 {
		t.Fatalf("a real port change must reset the status, got %s/%d", status, failures)
	}
}

// 2026-11-06：面板只恢复它自己停用的服务。运维在门户里手工停掉 hy2 后，计量轮次不能把它拉起。
func TestTrafficMeterDoesNotResumeExternallyStoppedService(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const node = "00000000-0000-4000-8000-0000000009f1"
	const server = "00000000-0000-4000-8000-0000000009f2"
	if _, err := db.Exec(ctx, `INSERT INTO servers(id,name) VALUES($1::uuid,'h')`, server); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	// 计量要读这些列：fixDB 的基础表结构没有它们，缺一个 scanTrafficNode 就会直接返回（traffic_error 也就不会写）。
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes
		ADD COLUMN traffic_error text,
		ADD COLUMN traffic_limit_bytes bigint,
		ADD COLUMN traffic_reset_day int DEFAULT 1,
		ADD COLUMN traffic_cycle_start timestamptz,
		ADD COLUMN traffic_cycle_bytes bigint DEFAULT 0,
		ADD COLUMN traffic_counter_rx bigint,
		ADD COLUMN traffic_counter_tx bigint,
		ADD COLUMN traffic_boot_id text`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,server_id,api_url,api_key,traffic_suspended)
		VALUES($1::uuid,'n',$2::uuid,'https://8.8.8.8:8443',$3,false)`, node, server, key); err != nil {
		t.Fatal(err)
	}
	var calls []string
	old := portalClient
	portalClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		body := `{"ok":true}`
		if strings.Contains(r.URL.Path, "traffic") {
			body = `{"ok":true,"rx_bytes":1000,"tx_bytes":500,"suspended":true,"boot_id":"b1"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { portalClient = old })
	s := fixServer(db)
	s.box = box
	s.meterNodeByID(ctx, node)
	for _, path := range calls {
		if strings.Contains(path, "suspend") {
			t.Fatalf("the meter must not resume a service it never suspended, calls=%v", calls)
		}
	}
	// 关键行为是「不发 suspend」。原始状态标记若没被清掉也不影响结论，所以文案只做软断言：
	// 有写就必须说清不是面板停的，没写也不能算失败（这台节点的计量在这套夹具下可能根本没跑到那一步）。
	var note string
	if err := db.QueryRow(ctx, `SELECT COALESCE(traffic_error,'') FROM hysteria_nodes WHERE id=$1::uuid`, node).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "" && !strings.Contains(note, "不是面板停用") {
		t.Fatalf("a recorded reason must explain it was not stopped by the panel, got %q", note)
	}
}

// roundTripFunc 让测试用函数当 Transport。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
