package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-06：上传 SSH 私钥时用连接时同一个解析器校验，不再只查「PRIVATE KEY」字样。
func TestUploadCredentialParsesPrivateKey(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) uploadCredential(")
	check := strings.Index(body, "sshclient.ValidatePrivateKey([]byte(secret))")
	seal := strings.Index(body, "s.box.Seal([]byte(secret))")
	if check < 0 || seal < 0 || check > seal {
		t.Fatal("uploadCredential 必须在加密保存之前解析私钥")
	}
}

// 2026-11-06：连接测试首次登记主机指纹时与采集器、执行器一样记 host_key_first_trust（带指纹）。
func TestTestServerSSHAuditsFirstTrust(t *testing.T) {
	body := funcBlock(t, "hostextras.go", "func (s *Server) testServerSSH(")
	if !strings.Contains(body, `"host_key_first_trust"`) || !strings.Contains(body, `"fingerprint": fingerprint`) {
		t.Fatal("首次信任主机密钥必须记审计并带上指纹")
	}
}

// 2026-11-06：到期结案取订阅锁时的数据库错误走 errCloseoutInternal（500），不把连接错误原文包进 409。
func TestCloseExpiredLockDBErrorIsInternal(t *testing.T) {
	body := funcBlock(t, "expired_closeout.go", "func (s *Server) closeExpiredSubscription(")
	if strings.Contains(body, `fmt.Errorf("订阅正在处理中，请稍后重试：%w", err)`) {
		t.Fatal("取锁失败不能把原始错误包进「订阅正在处理中」")
	}
	if !strings.Contains(body, "errors.As(err, &dbErr)") || !strings.Contains(body, "errCloseoutInternal") {
		t.Fatal("取锁时的数据库错误必须归为 errCloseoutInternal")
	}
}

// 2026-11-06：订阅锁的数据库错误不能把连接原文带进 Error()（它会被拼进回收失败明细、待办原因）。
func TestSubscriptionLockDBErrorHidesConnectionDetails(t *testing.T) {
	e := subscriptionLockDBError{errors.New(`failed to connect to user=nodeops database=nodeops: 10.0.0.5:5432`)}
	if strings.Contains(e.Error(), "nodeops") || strings.Contains(e.Error(), "10.0.0.5") {
		t.Fatalf("错误文案不能带连接信息：%s", e.Error())
	}
	if !strings.Contains(e.Unwrap().Error(), "10.0.0.5") {
		t.Fatal("原文仍要能取到（进日志）")
	}
}

// 2026-11-06：删除订阅时拿不到订阅锁：回 409，不登记「回收后删除」待办（待办会被立刻作废，运营却以为在补删）。
func TestDeleteSubscriptionLockBusyDoesNotQueue(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS subscription_pending_node_actions(subscription_id uuid PRIMARY KEY, action text, then_delete boolean, next_attempt_at timestamptz, last_error text, attempts int DEFAULT 0, register_revoked_order boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	other := fixServer(db)
	_, release, err := other.lockSubscription(context.WithValue(ctx, subscriptionLockKey{}, "holder"), fixSub)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	req := httptest.NewRequest(http.MethodDelete, "/api/subscriptions/"+fixSub, nil)
	req.SetPathValue("id", fixSub)
	rec := httptest.NewRecorder()
	s.deleteSubscriptionHandler(rec, asUser(req, adminA))
	if rec.Code != http.StatusConflict {
		t.Fatalf("订阅被占用时应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, fixSub).Scan(&n); err != nil || n != 0 {
		t.Fatalf("没开始回收就不能登记待办：n=%d err=%v", n, err)
	}
}

// 2026-11-06：回给商城的回收失败明细不带数据库错误原文；节点侧的报错照原样给。
func TestPublicRevokeFailuresHideDatabaseErrors(t *testing.T) {
	in := []subscriptionRevokeFailure{
		{NodeID: "n1", Reason: `读取回收退避状态失败：ERROR: relation "x" does not exist (SQLSTATE 42P01)`},
		{NodeID: "n2", Reason: `failed to connect to user=nodeops database=nodeops: 10.0.0.5:5432`},
		{NodeID: "n3", Reason: "节点 API 报错: timeout"},
	}
	out := publicRevokeFailures(in)
	for _, f := range out[:2] {
		if strings.Contains(f.Reason, "SQLSTATE") || strings.Contains(f.Reason, "nodeops") {
			t.Fatalf("数据库错误原文不能回给商城：%s", f.Reason)
		}
	}
	if out[2].Reason != in[2].Reason {
		t.Fatalf("节点侧报错应原样保留：%s", out[2].Reason)
	}
	if !strings.Contains(in[0].Reason, "SQLSTATE") {
		t.Fatal("不能改动原始明细（审计里要留原文）")
	}
	body := funcBlock(t, "store.go", "func (s *Server) storeRevokeOrder(")
	if !strings.Contains(body, `"failed": publicRevokeFailures(outcome.Failed)`) {
		t.Fatal("商城撤单的失败响应必须用 publicRevokeFailures")
	}
}

// 2026-11-06：单条对账——面板侧读库错误不报 502；有节点没对上回 207。
func TestReconcileHandlerStatusCodes(t *testing.T) {
	body := funcBlock(t, "provisioning.go", "func (s *Server) reconcileSubscriptionHandler(")
	call := strings.Index(body, "s.reconcileSubscriptionAccounts(lockCtx, rec)")
	after := body[call:]
	if strings.Contains(after[:min(len(after), 400)], "http.StatusBadGateway") {
		t.Fatal("reconcileSubscriptionAccounts 的错误来自面板自身，不能报 502")
	}
	if !strings.Contains(body, "status = http.StatusMultiStatus") {
		t.Fatal("部分节点失败时必须回 207")
	}
}

// 2026-11-06：拿锁前已到期、拿到锁时已被续费的订阅不能被报成「已删除」。
func TestPurgeReportsRenewedSubscriptionAsNotDeleted(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db) // 30 天后到期 = 生效中
	s := fixServer(db)
	ctx := context.Background()
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	rec.ExpiresAt = &past // 调用方手里的快照：已到期
	err = s.purgeSubscriptionOutcome(ctx, rec)
	var notStarted purgeNotStarted
	if !errors.As(err, &notStarted) || !errors.Is(err, errRenewedDuringPurge) {
		t.Fatalf("a renewed subscription must be reported as not purged, got %v", err)
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_tokens WHERE id=$1::uuid AND deleted_at IS NULL)`, fixSub).Scan(&exists); err != nil || !exists {
		t.Fatalf("the renewed subscription must still exist: %v %v", exists, err)
	}
}
