package httpapi

// 第七批行为的回归测试（2026-10-30）。

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"nodeops/server/internal/auth"
	"strings"
	"testing"
	"time"
)

// 告警通知在静默期睡眠时，服务关闭必须能打断它。
func TestAlertGraceSleepIsInterruptible(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "manage.go", "func (s *Server) notifyAlertFor(")
	if !strings.Contains(body, "shutdown != nil") || !strings.Contains(body, "<-shutdown:") {
		t.Error("静默期睡眠必须能被服务关闭打断 —— 否则进程退出时正在睡眠的告警会无声丢失")
	}
	if strings.Count(body, "time.Sleep(") > 0 && !strings.Contains(body, "time.After(") {
		t.Error("触发/重现告警的等待必须用 time.After + select，不能直接 time.Sleep")
	}
}

// 关闭通知接线必须在 main 里完成。
func TestShutdownNotifyIsWired(t *testing.T) {
	t.Parallel()
	main := readSourceFileAbs(t, "../../cmd/api/main.go")
	if !strings.Contains(main, "SetShutdownNotify(") {
		t.Error("main 必须把 shutdownCtx 接到 handler —— 否则 shutdownDone 永远是 nil")
	}
	serverSrc := readSourceFileAbs(t, "server.go")
	if !strings.Contains(serverSrc, "SetShutdownNotify") || !strings.Contains(serverSrc, "shutdownDone") {
		t.Error("Server 必须提供 SetShutdownNotify 并保存 shutdownDone")
	}
}

// 孤儿补删不允许对已结案的记录再次发起删除。
func TestOrphanRetryRejectsResolved(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "orphan.go", "func (s *Server) retryOrphan(")
	if !strings.Contains(body, "AND resolved_at IS NULL") {
		t.Error("SELECT 必须排除已结案的记录 —— 已结案的孤儿被再次重试会刷新 attempts、覆盖 resolved_at")
	}
	if !strings.Contains(body, "该待补删记录不存在") || !strings.Contains(body, "已结案") {
		t.Error("已结案与不存在要分开说 —— 混在一起会让人对不存在的记录反复点重试")
	}
}

// 每日对账在完全失败时不能把"上次运行"写成现在。
func TestNodeReconcileOnlyStampsWhenItCheckedNodes(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "node_account_reconcile.go", "func (s *Server) maybeReconcileNodeAccounts(")
	put := strings.Index(body, "putSetting")
	if put < 0 {
		t.Fatal("对账成功时必须更新上次运行时间")
	}
	if !strings.Contains(body[:put], "NodesChecked > 0") {
		t.Error("节点列表或订阅读失败时不能把上次运行写成现在 —— 否则下一次要等 23 小时")
	}
}

// 续费取消待办必须在同一个事务里。
func TestRenewCancelsPendingInSameTransaction(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) storeRenewOrder(")
	commit := strings.Index(body, "tx.Commit")
	cancel := strings.Index(body, "DELETE FROM subscription_pending_node_actions")
	if commit < 0 || cancel < 0 || cancel > commit {
		t.Fatal("续费与取消待办必须在同一个事务里 —— commit 之后再删会留下交错窗口")
	}
	if strings.Contains(body, "cancelPendingNodeAction(r.Context()") {
		t.Fatal("续费路径不能再单独调 cancelPendingNodeAction —— 那正是旧的事务外写法")
	}
}

// 已确认成功必须先落库，再看顶层错误。
func TestPersistBeforeCheckingTopLevelError(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "delivery_sync.go", "func (s *Server) synchronizeClaimedSubscription(")
	persist := strings.LastIndex(body, "persistReconciledAccounts(")
	if persist < 0 {
		t.Fatal("synchronizeClaimedSubscription 必须调用 persistReconciledAccounts")
	}
	// 判据是「有成功结果」就落库，不看 reconcile 的顶层错误。
	if !strings.Contains(body[:persist], "if len(accounts) > 0 {") {
		t.Error("落库判据必须只看「有成功结果」，不能加 err==nil —— 部分成功+一个失败正是要保留的场景")
	}
	// 也不能在判据里出现 err==nil —— 那会把"部分成功"判成"不落库"。
	if strings.Contains(body[:persist], "if err == nil && len(accounts) > 0") {
		t.Error("落库不能要求 err==nil —— 部分成功时 reconcile 会返回错误")
	}
}

// 限流器清理不会在每次请求都扫全表。
func TestSlidingLimiterSweepIsThrottled(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "server.go", "func (l *slidingLimiter) allowWithRetry(")
	if !strings.Contains(body, "limiterSweepInterval") {
		t.Error("全量清理必须限频 —— 持锁状态下每次请求都 O(n) 会把登录接口串行化")
	}
}

// 锁定槽必须有界：无界会在高并发时耗尽连接池。
// 上限取自生产实测（8 台节点、每节点最多 2 个并发同步，留一倍余量）。
func TestSubscriptionLockSlotsAreBounded(t *testing.T) {
	t.Parallel()
	src := readSourceFile(t, "delivery_sync.go")
	if !strings.Contains(src, "make(chan struct{}, 16)") {
		t.Error("锁槽必须有界 —— 无界会在高并发时耗尽连接池")
	}
}

// 反向验证辅助：readSourceFile 读同目录源码；跨目录的（main.go）单独走。
func readSourceFileAbs(t *testing.T, rel string) string {
	t.Helper()
	return readSourceFile(t, rel)
}

// 2026-11-06：跨源部署下 CORS 预检必须放行 PUT 与 X-Confirm-Dangerous，
// 否则一半写操作与所有「危险操作确认」在浏览器里直接被挡。
func TestCORSAllowsEveryMethodAndConfirmHeader(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"http://localhost:5173"}, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	h := s.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodOptions, "/api/schedules", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	methods := rec.Header().Get("Access-Control-Allow-Methods")
	headers := rec.Header().Get("Access-Control-Allow-Headers")
	for _, m := range []string{"PUT", "PATCH", "DELETE", "POST"} {
		if !strings.Contains(methods, m) {
			t.Fatalf("preflight must allow %s, got %q", m, methods)
		}
	}
	if !strings.Contains(headers, "X-Confirm-Dangerous") {
		t.Fatalf("preflight must allow X-Confirm-Dangerous, got %q", headers)
	}
	if rec.Header().Get("Vary") == "" {
		t.Fatal("Vary: Origin must be sent unconditionally")
	}
	// 未登记的源不能拿到允许头。
	evil := httptest.NewRequest(http.MethodOptions, "/api/schedules", nil)
	evil.Header.Set("Origin", "http://evil.example")
	recEvil := httptest.NewRecorder()
	h.ServeHTTP(recEvil, evil)
	if recEvil.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unknown origins must not be allowed")
	}
}

// 2026-11-06：写类主机任务（含 system_preset）仅限管理员；只读任务 operator 仍可用。
// 原来只校验 X-Confirm-Dangerous，operator 带上就能以 root 跑 kernel_reboot / uninstall_hy2。
func TestOperatorCannotQueueWriteTasks(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret"})
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	call := func(role, body string, confirm bool) int {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
		if confirm {
			req.Header.Set("X-Confirm-Dangerous", "yes")
		}
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: "u1", Role: role}))
		rec := httptest.NewRecorder()
		s.createTask(rec, req)
		return rec.Code
	}
	for _, body := range []string{
		`{"server_id":"00000000-0000-0000-0000-000000000001","operation_type":"system_preset","parameters":{"preset":"kernel_reboot"}}`,
		`{"server_id":"00000000-0000-0000-0000-000000000001","operation_type":"restart","parameters":{"unit":"hysteria-server"}}`,
	} {
		if code := call("operator", body, true); code != http.StatusForbidden {
			t.Fatalf("operator must not queue %s: got %d", body, code)
		}
	}
	// 只读任务对 operator 仍放行：403 只挂在 !readOnlyOperations 上（走 DB 分支需要库，这里断言判据本身）。
	if body := funcBlock(t, "server.go", "func (s *Server) createTask("); !strings.Contains(body, "!readOnlyOperations[input.OperationType] && claims.Role != \"admin\"") {
		t.Fatal("the admin gate must apply only to write-type operations")
	}
}
