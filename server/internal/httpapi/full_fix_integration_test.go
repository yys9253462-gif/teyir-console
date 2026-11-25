package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"nodeops/server/internal/secrets"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for PostgreSQL integration")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("nodeops_fix_%d", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); base.Close() })
	_, err = db.Exec(ctx, `CREATE TABLE system_settings(key text PRIMARY KEY,value text,description text,updated_at timestamptz DEFAULT now());
 CREATE TABLE servers(id uuid PRIMARY KEY,name text DEFAULT '',host text DEFAULT '',deleted_at timestamptz);
 CREATE TABLE operation_tasks(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),server_id uuid,status text);
 CREATE TABLE subscription_tokens(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),token text NOT NULL UNIQUE,label text DEFAULT 'fixture',enabled boolean DEFAULT true,expires_at timestamptz,ip_limit int DEFAULT 5,auto_delete boolean DEFAULT false,order_id text UNIQUE,user_email text,note text DEFAULT '',active_ips jsonb DEFAULT '[]',created_at timestamptz DEFAULT now(),last_accessed_at timestamptz,access_count int DEFAULT 0,deleted_at timestamptz,node_accounts jsonb DEFAULT '{}',category_id uuid,revenue_monthly double precision,revenue_currency text,is_panel_merged boolean NOT NULL DEFAULT false);
 CREATE TABLE hysteria_nodes(id uuid PRIMARY KEY,name text,api_url text,api_key bytea,domain text,listen_port int,updated_at timestamptz DEFAULT now(),created_at timestamptz DEFAULT now(),deleted_at timestamptz,in_store boolean DEFAULT true,server_id uuid,traffic_suspended boolean NOT NULL DEFAULT false,traffic_suspended_at timestamptz);
 CREATE TABLE node_category_members(category_id uuid,node_id uuid);
 CREATE TABLE audit_logs(user_id uuid,action text,resource_type text,resource_id uuid,ip_address inet,details jsonb);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"042_delivery_sync_signature_v2.sql", "043_durable_host_jobs.sql", "044_subscription_node_ids.sql", "056_store_revoked_orders.sql"} {
		raw, err := os.ReadFile(filepath.Join("../../migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
	}
	return db
}
func fixServer(db *pgxpool.Pool) *Server {
	// 2026-11-06：默认给一个高配额的读写限流器。读端点现在都走 enforceReadLimit，而 fixServer 原来
	// 把 limiter 留成 nil —— 直接调 handler 的集成测试会 nil 解引用 panic（不是限流问题，是夹具缺省值）。
	// 需要真限流的用例自己换成小配额。
	return &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		readLimiter:   newSlidingLimiter(time.Minute, 100000),
		writeLimiter:  newSlidingLimiter(time.Minute, 100000),
		publicLimiter: newSlidingLimiter(time.Minute, 100000)}
}

const fixSub = "00000000-0000-0000-0000-000000000101"
const fixNode = "00000000-0000-0000-0000-000000000102"
const fixHost = "00000000-0000-0000-0000-000000000103"

func createFixSub(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	_, err := db.Exec(context.Background(), `INSERT INTO subscription_tokens(id,token,order_id,expires_at) VALUES($1::uuid,'fixture-token-for-customer-only','fixture-order',now()+interval '30 days')`, fixSub)
	if err != nil {
		t.Fatal(err)
	}
}
func TestMatchServerFallsBackToHostnameWhenIPIsMissing(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const serverID = "00000000-0000-0000-0000-000000000103"
	if _, err := db.Exec(ctx, `INSERT INTO servers(id,name,host) VALUES($1::uuid,'known host','hy2.example.com')`, serverID); err != nil {
		t.Fatal(err)
	}
	id, name, err := fixServer(db).matchServer(ctx, "", "HY2.Example.com.")
	if err != nil || id != serverID || name != "known host" {
		t.Fatalf("hostname match failed: id=%q name=%q err=%v", id, name, err)
	}
}

func TestFixOutboxAtomicAndCancelled(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	var status string
	if err := db.QueryRow(context.Background(), `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil || status != "pending" {
		t.Fatal(status, err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE subscription_tokens SET enabled=false WHERE id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(context.Background(), `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status)
	if status != "cancelled" {
		t.Fatal(status)
	}
	tx, _ := db.Begin(context.Background())
	_, _ = tx.Exec(context.Background(), `UPDATE subscription_tokens SET enabled=true WHERE id=$1::uuid`, fixSub)
	_ = tx.Rollback(context.Background())
	_ = db.QueryRow(context.Background(), `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status)
	if status != "cancelled" {
		t.Fatal("outbox escaped rollback")
	}
}

type revokeTestTransport struct {
	onDelete func()
	status   int
	body     string
}

func (f revokeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.onDelete != nil {
		f.onDelete()
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	body := f.body
	if body == "" {
		body = `{"ok":true}`
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestRevokeNodeTreatsMissingUserAsSuccess(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	ctx := context.Background()
	_, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=jsonb_build_object($1::text,jsonb_build_object('user_id','sub_test')) WHERE id=$2::uuid`, fixNode, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old })
	portalClient = &http.Client{Transport: revokeTestTransport{status: http.StatusNotFound, body: `{"ok":false,"error":"User not found"}`}}
	outcome := fixServer(db).revokeNodeFromAllSubscriptions(ctx, fixNode, &portalTarget{BaseURL: "https://node.example", APIKey: "0123456789abcdef"})
	if !outcome.OK() || outcome.Revoked != 1 {
		t.Fatalf("节点已无该账号时回收应幂等成功: %+v", outcome)
	}
	var remains bool
	if err := db.QueryRow(ctx, `SELECT node_accounts ? $1 FROM subscription_tokens WHERE id=$2::uuid`, fixNode, fixSub).Scan(&remains); err != nil || remains {
		t.Fatalf("账号已不存在后应移除本地映射: remains=%v err=%v", remains, err)
	}
}

func TestRevokeNodePreservesOtherNodeMapping(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	ctx := context.Background()
	_, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=jsonb_build_object($1::text,jsonb_build_object('user_id','sub_test')) WHERE id=$2::uuid`, fixNode, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old })
	portalClient = &http.Client{Transport: revokeTestTransport{onDelete: func() {
		_, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=node_accounts || jsonb_build_object('other',jsonb_build_object('user_id','other_user')) WHERE id=$1::uuid`, fixSub)
		if err != nil {
			t.Fatal(err)
		}
	}}}
	outcome := fixServer(db).revokeNodeFromAllSubscriptions(ctx, fixNode, &portalTarget{BaseURL: "https://node.example", APIKey: "0123456789abcdef"})
	if !outcome.OK() || outcome.Revoked != 1 {
		t.Fatalf("回收应成功: %+v", outcome)
	}
	var removed, kept bool
	err = db.QueryRow(ctx, `SELECT node_accounts ? $1, node_accounts ? 'other' FROM subscription_tokens WHERE id=$2::uuid`, fixNode, fixSub).Scan(&removed, &kept)
	if err != nil || removed || !kept {
		t.Fatalf("必须只移除目标节点: removed=%v kept=%v err=%v", removed, kept, err)
	}
}

func TestRevokeNodeReportsBusySubscription(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	ctx := context.Background()
	_, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=jsonb_build_object($1::text,jsonb_build_object('user_id','sub_test')) WHERE id=$2::uuid`, fixNode, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	_, release, err := s.lockSubscription(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	outcome := s.revokeNodeFromAllSubscriptions(ctx, fixNode, &portalTarget{})
	if outcome.OK() || len(outcome.Failed) != 1 {
		t.Fatalf("被锁定的订阅必须报告回收失败: %+v", outcome)
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT node_accounts ? $1 FROM subscription_tokens WHERE id=$2::uuid`, fixNode, fixSub).Scan(&exists); err != nil || !exists {
		t.Fatalf("未回收时必须保留映射: exists=%v err=%v", exists, err)
	}
}

func TestFixSubscriptionLockExcludesOtherInstances(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx, release, err := s.lockSubscription(context.Background(), fixSub)
	if err != nil {
		t.Fatal(err)
	}
	_, nested, err := s.lockSubscription(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	nested()
	other := fixServer(db)
	if _, unlock, err := other.lockSubscription(context.Background(), fixSub); err == nil {
		unlock()
		t.Fatal("concurrent instance obtained same lock")
	}
	release()
	_, unlock, err := other.lockSubscription(context.Background(), fixSub)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

type fixTransport struct {
	expiry *int64
	fail   *bool
}

func (f fixTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if *f.fail {
		return nil, errors.New("isolated node unavailable")
	}
	body := ""
	if r.Method == "POST" {
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		*f.expiry = int64(input["expires_at"].(float64))
		body = `{"ok":true}`
	} else {
		body = fmt.Sprintf(`{"ok":true,"user":{"expires_at":%d}}`, *f.expiry)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}
func TestFixDeliveryFailurePendingAndRecoveryReadback(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	box, _ := secrets.NewBox(key)
	s.box = box
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(context.Background(), `INSERT INTO servers(id) VALUES($1::uuid);`, fixHost); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(context.Background(), `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'fixture-node','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, fixNode, cipher, fixHost)
	if err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old })
	fail := true
	expiry := int64(0)
	portalClient = &http.Client{Transport: fixTransport{&expiry, &fail}}
	rec, err := s.loadSubscriptionByID(context.Background(), fixSub)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.synchronizeSubscription(context.Background(), rec); err == nil {
		t.Fatal("offline node marked complete")
	}
	req := httptest.NewRequest("GET", "http://panel.example/api/store/orders?order_id=fixture-order", nil)
	reply := httptest.NewRecorder()
	s.writeStoreOrder(reply, req, rec)
	if reply.Code != 207 || !strings.Contains(reply.Body.String(), `"ok":false`) {
		t.Fatal("failed delivery reported successful", reply.Code, reply.Body.String())
	}
	fail = false
	if err = s.synchronizeSubscription(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if expiry != rec.ExpiresAt.Unix() {
		t.Fatal("node absolute deadline mismatch")
	}
	reply = httptest.NewRecorder()
	s.writeStoreOrder(reply, req, rec)
	if reply.Code != 200 {
		t.Fatal(reply.Body.String())
	}
	// 旧快照不能覆盖数据库中的更新；同步器在锁内重新读取权威期限。
	stale := *rec
	_, err = db.Exec(context.Background(), `UPDATE subscription_tokens SET expires_at=expires_at+interval '30 days' WHERE id=$1::uuid`, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.synchronizeSubscription(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}
	var want time.Time
	_ = db.QueryRow(context.Background(), `SELECT expires_at FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&want)
	if expiry != want.Unix() {
		t.Fatal("stale snapshot rewound node deadline")
	}
}
func TestFixHostJobsSurviveAndKeepLease(t *testing.T) {
	db := fixDB(t)
	_, _ = db.Exec(context.Background(), `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost)
	st := &installJobStore{db: db, owner: "old-instance", ready: true, jobs: map[string]*installJob{}, byServer: map[string]string{}}
	job := &installJob{ID: "fixture-host-job", ServerID: fixHost, Status: "running", Step: "fixture", StartedAt: time.Now()}
	if err := st.createLocked(context.Background(), job, []string{fixHost}); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(context.Background(), `UPDATE host_operation_jobs SET heartbeat=now()-interval '2 minutes' WHERE id=$1`, job.ID)
	fresh := &installJobStore{db: db, owner: "new-instance", jobs: map[string]*installJob{}, byServer: map[string]string{}, owned: map[string]bool{}}
	if err := fresh.restoreLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh.jobs[job.ID].Status != "interrupted" || fresh.byServer[fixHost] != job.ID {
		t.Fatal("lost interrupted job or lease")
	}
	fresh.ready = true
	conflictErr := fresh.createLocked(context.Background(), &installJob{ID: "conflict", Status: "running"}, []string{fixHost})
	if conflictErr == nil {
		t.Fatal("duplicate host operation admitted")
	}
	// 2026-11-04：租约冲突是 409（客户端可处理），不是 503；其它数据库错误不能冒充「已有租约」。
	var conflict errHostJobConflict
	if !errors.As(conflictErr, &conflict) {
		t.Fatalf("a lease conflict must be reported as errHostJobConflict, got %T %v", conflictErr, conflictErr)
	}
	rec := httptest.NewRecorder()
	(&Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).writeHostJobCreateError(rec, httptest.NewRequest(http.MethodPost, "/x", nil), conflictErr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("lease conflict must map to 409, got %d", rec.Code)
	}
	fresh.jobs[job.ID].Status = "failed"
	if err := fresh.persistLocked(fresh.jobs[job.ID]); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM host_operation_leases WHERE server_id=$1::uuid`, fixHost).Scan(&count)
	if count != 0 {
		t.Fatal("finalized job retained lease")
	}

	// 2026-11-03：结束态落库失败时要登记重试，巡检补上后租约才会释放。
	if _, err := db.Exec(context.Background(), `INSERT INTO host_operation_leases(server_id,job_id) VALUES($1::uuid,$2)`, fixHost, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(context.Background(), `UPDATE host_operation_jobs SET status='running' WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	// 让这一次 UPDATE 失败（模拟库抖动），而不动表结构。
	if _, err := db.Exec(context.Background(), `CREATE FUNCTION fail_job_save() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN RAISE EXCEPTION 'simulated outage'; END $f$;
		CREATE TRIGGER fail_job_save BEFORE UPDATE ON host_operation_jobs FOR EACH ROW EXECUTE FUNCTION fail_job_save()`); err != nil {
		t.Fatal(err)
	}
	fresh.update(job.ID, func(j *installJob) { j.Status = "success" })
	if !fresh.unsaved[job.ID] {
		t.Fatal("a failed final save must be queued for retry")
	}
	if _, err := db.Exec(context.Background(), `DROP TRIGGER fail_job_save ON host_operation_jobs`); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	fresh.retryUnsavedLocked()
	fresh.mu.Unlock()
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM host_operation_leases WHERE server_id=$1::uuid`, fixHost).Scan(&count)
	if count != 0 || fresh.unsaved[job.ID] {
		t.Fatal("retried final save must release the host lease")
	}
}

// 2026-11-03：重试历史任务必须走与新建任务同一张白名单。
func TestRetryTaskRejectsUnsupportedOperation(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN node_id uuid, ADD COLUMN parameters jsonb DEFAULT '{}'::jsonb, ADD COLUMN operation_type text;
INSERT INTO operation_tasks(id,server_id,status,operation_type) VALUES ('00000000-0000-0000-0000-0000000a0001','`+fixHost+`','failed','deploy')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/x/retry", nil)
	req.SetPathValue("id", "00000000-0000-0000-0000-0000000a0001")
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.retryTask(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("retrying an unsupported operation must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM operation_tasks`).Scan(&n)
	if n != 1 {
		t.Fatalf("refused retry must not queue a task, have %d rows", n)
	}
}

// 2026-11-04：安装脚本超时 / 取消时任务记 interrupted、保留主机租约（远端进程可能还在跑）；
// 只有操作者确认远端已停止（resolveInstallJob）才释放。
func TestInstallTimeoutKeepsHostLease(t *testing.T) {
	db := fixDB(t)
	_, _ = db.Exec(context.Background(), `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost)
	st := &installJobStore{db: db, owner: "fixture", ready: true, jobs: map[string]*installJob{}, byServer: map[string]string{}}
	job := &installJob{ID: "fixture-timeout-job", ServerID: fixHost, Servers: []string{fixHost}, Status: "running", StartedAt: time.Now()}
	if err := st.createLocked(context.Background(), job, []string{fixHost}); err != nil {
		t.Fatal(err)
	}
	st.jobs[job.ID] = job
	st.byServer[fixHost] = job.ID
	st.update(job.ID, func(j *installJob) { j.Status = "interrupted" })
	var leases int
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM host_operation_leases WHERE server_id=$1::uuid`, fixHost).Scan(&leases)
	if leases != 1 {
		t.Fatalf("an interrupted install must keep its host lease, got %d", leases)
	}
	body := funcBlock(t, "installhy2.go", "func (s *Server) runInstallScript(")
	// 2026-11-06：任何没拿到退出码的错误（超时、取消、连接重置）都必须以 interrupted 收尾。
	guard := "\tif err != nil {\n\t\t// 2026-11-04：超时 / 取消时不能当成普通失败。"
	at := strings.Index(body, guard)
	generic := strings.Index(body, "if err != nil || exit != 0 {")
	if at < 0 || generic < at || !strings.Contains(body[at:generic], "interrupt(") {
		t.Fatal("an install script that ended without an exit code must end as interrupted, not failed")
	}
}

// 2026-11-04：迁移时新主机的安装脚本超时，同样要以 interrupted 收尾、保留主机互斥，并先回退节点归属。
func TestMigrateInstallTimeoutIsInterrupted(t *testing.T) {
	body := funcBlock(t, "migratenode.go", "func (s *Server) runMigrateNode(")
	run := strings.Index(body, "installScriptCommand(newSudo, env)")
	guard := strings.Index(body, "\tif err != nil {\n\t\tif rollback != nil {")
	generic := strings.Index(body, `finish(fmt.Errorf("新主机安装失败`)
	if run < 0 || guard < run || generic < guard {
		t.Fatal("a timed-out migration install must be handled before the generic failure branch")
	}
	branch := body[guard:generic]
	if !strings.Contains(branch, `"interrupted"`) || strings.Contains(branch, "finish(") || !strings.Contains(branch, "rollback()") {
		t.Fatal("the timeout branch must roll back, mark the job interrupted and keep the host lease (no finish)")
	}
}

// 2026-11-04：同时存在「按域名登记」与「按 IP 登记」的主机时，IP 匹配优先，与行序无关。
func TestMatchServerPrefersIPOverHostname(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const byName = "00000000-0000-0000-0000-0000000001a1"
	const byIP = "00000000-0000-0000-0000-0000000001a2"
	// 先插按域名登记的那台：旧实现按行序返回它。
	if _, err := db.Exec(ctx, `INSERT INTO servers(id,name,host) VALUES($1::uuid,'by name','hy2.example.com'),($2::uuid,'by ip','8.8.8.8')`, byName, byIP); err != nil {
		t.Fatal(err)
	}
	id, _, err := fixServer(db).matchServer(ctx, "8.8.8.8", "hy2.example.com")
	if err != nil || id != byIP {
		t.Fatalf("the host registered by the node's public IP must win, got %q err=%v", id, err)
	}
}

// 2026-11-04：节点下架（移出商城）只回收买家订阅的账号，手工 / 自用订阅照常可用。
func TestDelistRevokesOnlyBuyerSubscriptions(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	ctx := context.Background()
	const manualSub = "00000000-0000-0000-0000-0000000000ab"
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,expires_at) VALUES($1::uuid,'fixture-token-manual-self-use',now()+interval '30 days')`, manualSub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE subscription_tokens SET node_accounts=jsonb_build_object($1::text,jsonb_build_object('user_id','sub_'||left(id::text,8))) WHERE id IN ($2::uuid,$3::uuid)`, fixNode, fixSub, manualSub); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old })
	portalClient = &http.Client{Transport: revokeTestTransport{}}
	outcome := fixServer(db).revokeNodeFromSubscriptions(ctx, fixNode, &portalTarget{BaseURL: "https://node.example", APIKey: "0123456789abcdef"}, nil, true)
	if !outcome.OK() || outcome.Revoked != 1 {
		t.Fatalf("下架应只回收 1 个买家账号: %+v", outcome)
	}
	var buyerKept, manualKept bool
	if err := db.QueryRow(ctx, `SELECT (SELECT node_accounts ? $1 FROM subscription_tokens WHERE id=$2::uuid), (SELECT node_accounts ? $1 FROM subscription_tokens WHERE id=$3::uuid)`, fixNode, fixSub, manualSub).Scan(&buyerKept, &manualKept); err != nil {
		t.Fatal(err)
	}
	if buyerKept || !manualKept {
		t.Fatalf("买家映射应移除、手工订阅映射应保留: buyer=%v manual=%v", buyerKept, manualKept)
	}
	body := funcBlock(t, "admin.go", "func (s *Server) updateNode(")
	if !strings.Contains(body, "s.revokeNodeFromBuyerSubscriptions(") || strings.Contains(body, "s.revokeNodeFromAllSubscriptions(") {
		t.Fatal("updateNode 下架时必须只回收买家订阅（revokeNodeFromBuyerSubscriptions）")
	}
}

// 2026-11-06：主机行被别的事务锁住（如换 IP 正在调 Cloudflare）时，createLocked 不能无限期等待 ——
// 调用方持有 installJobs.mu，等待期间所有主机任务的日志、进度、心跳都会卡住。
func TestCreateHostJobDoesNotWaitForeverOnHostRowLock(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	holder, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM servers WHERE id=$1::uuid FOR UPDATE`, fixHost); err != nil {
		t.Fatal(err)
	}
	st := &installJobStore{db: db, owner: "fixture", ready: true, jobs: map[string]*installJob{}, byServer: map[string]string{}}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- st.createLocked(ctx, &installJob{ID: "blocked-job", ServerID: fixHost, Status: "running", StartedAt: time.Now()}, []string{fixHost})
	}()
	select {
	case err := <-done:
		var conflict errHostJobConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("a held host row lock must surface as a conflict, got %T %v", err, err)
		}
		if time.Since(start) > 9*time.Second {
			t.Fatalf("lock wait took %v", time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("createLocked waited indefinitely for the host row lock")
	}
}
