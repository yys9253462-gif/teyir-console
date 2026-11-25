package httpapi

import (
	"os"
	"strings"
	"testing"
)

// 2026-10-23：商城退款、后台停用时节点回收失败，原来没人再补。
// 守住：失败必须入队；sweeper 每轮补做；续费/重新启用会作废待办；
// 补做前再确认订阅仍处于停用状态，绝不停掉/删掉正在使用的账号。
func TestStoreRevokeFailureIsQueued(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) storeRevokeOrder(")
	fail := strings.Index(body, "if !outcome.OK()")
	queue := strings.Index(body, "s.queuePendingNodeActionFor(r.Context(), rec.ID, pendingActionRevoke, input.Delete, input.Delete")
	reply := strings.Index(body, "http.StatusBadGateway")
	if fail < 0 || queue < 0 || reply < 0 || !(fail < queue && queue < reply) {
		t.Fatal("a failed shop revoke must be queued for retry before answering")
	}
}

func TestAdminDisableFailureIsQueuedAndReenableCancels(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) updateSubscriptionHandler(")
	if !strings.Contains(body, "s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionDisable") {
		t.Fatal("a failed admin disable must be queued for retry")
	}
	if !strings.Contains(body, "if active {\n\t\t\t// 重新启用或续期") || !strings.Contains(body, "s.cancelPendingNodeAction(r.Context(), rec.ID)") {
		t.Fatal("re-enabling must cancel a queued disable/revoke")
	}
	renew := funcBlock(t, "store.go", "func (s *Server) storeRenewOrder(")
	commit := strings.Index(renew, "tx.Commit(r.Context())")
	cancel := strings.Index(renew, "DELETE FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid")
	// 2026-10-30：cancel 必须在事务里（commit 之前），而不是 commit 之后单独执行。
	// 单独执行在 DB 抖动时会留下「续费成功 + 后台仍按旧待办停用」的窗口，
	// 买家付了钱账号却被停。
	if commit < 0 || cancel < 0 || cancel > commit {
		t.Fatal("a committed renewal must cancel a queued disable/revoke inside the same transaction")
	}
	if strings.Contains(renew, "s.cancelPendingNodeAction(r.Context(), rec.ID)") {
		t.Fatal("commit 之后不能再单独调 cancelPendingNodeAction —— 那正是会产生交错窗口的旧写法")
	}
}

func TestSweeperDrainsQueue(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) sweepExpiredSubscriptions(")
	if !strings.Contains(body, "s.drainPendingNodeActions(ctx)") {
		t.Fatal("the sweeper must drain pending node actions every round")
	}
}

func TestPendingActionNeverTouchesActiveSubscription(t *testing.T) {
	body := funcBlock(t, "pending_node_actions.go", "func (s *Server) runPendingNodeAction(")
	guard := strings.Index(body, "if rec.Enabled && !rec.expired(time.Now()) {")
	revoke := strings.Index(body, "s.revokeSubscriptionAccounts(ctx, rec)")
	disable := strings.Index(body, "s.applySubscriptionNodeStatus(ctx, rec, false)")
	if guard < 0 || revoke < 0 || disable < 0 || guard > revoke || guard > disable {
		t.Fatal("the worker must re-check the subscription is still inactive before touching nodes")
	}
	if !strings.Contains(body[guard:revoke], "s.cancelPendingNodeAction(ctx, subID)") {
		t.Fatal("an active subscription must cancel the queued action")
	}
}

func TestRetryQueuesAbortOnIncompleteReads(t *testing.T) {
	for _, tc := range []struct {
		file string
		sig  string
		scan string
	}{
		{"pending_node_actions.go", "func (s *Server) drainPendingNodeActions(", "rows.Scan(&it.id, &it.action, &it.del, &it.register); err != nil"},
		// delivery_sync.go 的重试已改为 claimNextSubscriptionSync 逐条 QueryRow 领取，没有多行读取，不在此列。
	} {
		body := funcBlock(t, tc.file, tc.sig)
		if !strings.Contains(body, tc.scan) || !strings.Contains(body, "rows.Err()") {
			t.Errorf("%s must abort on scan and iteration errors", tc.sig)
		}
	}
}

func TestPendingActionsMigrationHasNoCascade(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/047_pending_node_actions.sql")
	if err != nil {
		t.Fatalf("migration missing: %v", err)
	}
	sql := strings.ToUpper(string(raw))
	if strings.Contains(sql, "REFERENCES") || strings.Contains(sql, "CASCADE") {
		t.Fatal("pending actions must not be deleted together with the subscription row")
	}
	if !strings.Contains(sql, "CHECK (ACTION IN ('REVOKE', 'DISABLE'))") {
		t.Fatal("action must be constrained to revoke/disable")
	}
}

// 2026-11-04：待办执行全程持订阅锁并在锁内重读；软删只针对仍未生效的行。
func TestPendingActionHoldsLockAndNeverDeletesActiveRow(t *testing.T) {
	body := funcBlock(t, "pending_node_actions.go", "func (s *Server) runPendingNodeAction(")
	lock := strings.Index(body, "s.lockSubscription(ctx, subID)")
	load := strings.Index(body, "s.loadSubscriptionByID(ctx, subID)")
	if lock < 0 || load < 0 || lock > load {
		t.Fatal("the subscription must be locked before it is (re)loaded")
	}
	finish := funcBlock(t, "pending_node_actions.go", "func (s *Server) finishPendingNodeAction(")
	if !strings.Contains(finish, "AND NOT (enabled AND (expires_at IS NULL OR expires_at > now()))") {
		t.Fatal("the soft-delete must refuse rows that are active again")
	}
}

// 2026-11-04：后台修改仍生效的订阅（续期 / 改设备数）时，节点同步不完整要排后台同步，不能只记日志。
func TestActiveSubscriptionEditQueuesSyncOnPartialFailure(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) updateSubscriptionHandler(")
	at := strings.Index(body, "if !stateOutcome.allOK() {")
	if at < 0 {
		t.Fatal("partial-failure branch not found")
	}
	branch := body[at:]
	branch = branch[:strings.Index(branch, "updated, err := s.loadSubscriptionByID(")]
	// 2026-11-06：登记重试必须用不带取消的 ctx —— 走到这里往往正因为客户端先断开了，
	// 用已取消的 r.Context() 登记会直接失败，而这是唯一会补推到期时间的地方。
	if !strings.Contains(branch, "s.queueSubscriptionSync(context.WithoutCancel(r.Context()), rec.ID)") {
		t.Fatal("an active subscription with failed node updates must queue a background sync with a non-cancelled ctx")
	}
}

// 2026-11-04：任务心跳在全局任务锁内执行，必须带超时（库卡住时不能把所有安装 / 迁移一起挂死）。
func TestHostJobHeartbeatIsBounded(t *testing.T) {
	body := funcBlock(t, "host_job_journal.go", "func (s *Server) StartHostJobJournal(")
	at := strings.Index(body, "SET heartbeat=now()")
	if at < 0 {
		t.Fatal("heartbeat update not found")
	}
	before := body[:at]
	if !strings.Contains(before[strings.LastIndex(before, "for id := range installJobs.owned"):], "context.WithTimeout(ctx, 5*time.Second)") {
		t.Fatal("the heartbeat Exec must run under a bounded context")
	}
}

// 2026-11-06：清理步骤（第 2 步）出错不能让 sweeper 提前返回 —— 第 3 步的退款 / 停用补做每轮都必须跑。
func TestSweeperAlwaysReachesPendingDrain(t *testing.T) {
	body := funcBlock(t, "store.go", "func (s *Server) sweepExpiredSubscriptions(")
	drain := strings.Index(body, "s.drainPendingNodeActions(ctx)")
	if drain < 0 {
		t.Fatal("the sweeper must drain pending node actions")
	}
	for _, line := range strings.Split(body[:drain], "\n") {
		if strings.TrimSpace(line) == "return" {
			t.Fatal("no early return may skip drainPendingNodeActions")
		}
	}
	if !strings.Contains(body, "s.purgeExpiredAutoDelete(ctx, now)") {
		t.Fatal("the purge step must be isolated so its errors cannot skip the drain")
	}
}
