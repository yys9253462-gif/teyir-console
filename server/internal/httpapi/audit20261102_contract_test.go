package httpapi

// 2026-11-02 审计修复的回归测试（M1 / M3 / M4 / L2 / L14）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// M1：静默期结束后必须重新查告警状态，而不是用等待前的旧值。
func TestAlertNotifyRechecksStatusAfterGrace(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "manage.go", "func (s *Server) notifyAlertFor(")
	wait := strings.Index(body, "time.After(alertGraceFor(kind))")
	if wait < 0 {
		t.Fatal("找不到静默期等待")
	}
	recheck := strings.Index(body[wait:], "SELECT status FROM alerts WHERE id = $1::uuid")
	if recheck < 0 {
		t.Fatal("静默期结束后必须重新 SELECT status —— 否则期间已恢复的告警照样推送")
	}
	decide := strings.Index(body[wait:], `status == "resolved" || young`)
	if decide < 0 || decide < recheck {
		t.Error("是否放弃推送必须基于重查后的 status")
	}
}

// M4：能改主机写权限、SSH 凭据、新建主机和 x-ui 面板的接口只给管理员。
func TestSensitiveWriteRoutesRequireAdmin(t *testing.T) {
	t.Parallel()
	src := readSourceFileAbs(t, "server.go")
	for _, route := range []string{
		`"POST /api/servers", s.authenticated(s.requireAdmin(s.createServer))`,
		`"POST /api/servers/{id}/credential", s.authenticated(s.requireAdmin(s.uploadCredential))`,
		`"POST /api/servers/{id}/write-ops", s.authenticated(s.requireAdmin(s.setWriteOps))`,
		`"POST /api/xui/panels", s.authenticated(s.requireAdmin(s.xuiCreatePanel))`,
	} {
		if !strings.Contains(src, route) {
			t.Errorf("路由必须要求管理员角色：%s", route)
		}
	}
}

// L14：登录成功头写实际角色。
func TestLoginSuccessHeaderCarriesRealRole(t *testing.T) {
	t.Parallel()
	src := readSourceFileAbs(t, "server.go")
	if strings.Contains(src, `"X-Login-Success", "admin"`) {
		t.Error("X-Login-Success 不能写死 admin —— 安全统计会把其他角色记成管理员登录")
	}
	if !strings.Contains(src, `w.Header().Set("X-Login-Success", role)`) {
		t.Error("X-Login-Success 必须写登录用户的实际角色")
	}
}

// M3 / L2：主函数等 Shutdown 排空后再退出；服务端设置读与空闲超时。
func TestMainWaitsForGracefulShutdown(t *testing.T) {
	t.Parallel()
	main := readSourceFileAbs(t, "../../cmd/api/main.go")
	if !strings.Contains(main, "defer close(shutdownDone)") {
		t.Error("Shutdown goroutine 结束时必须关闭 shutdownDone")
	}
	serve := strings.Index(main, "server.ListenAndServe()")
	wait := strings.Index(main, "<-shutdownDone")
	if serve < 0 || wait < serve {
		t.Error("ListenAndServe 返回后必须等待 shutdownDone，否则 10 秒排空窗口不生效")
	}
	for _, field := range []string{"ReadTimeout: 30 * time.Second", "IdleTimeout: 120 * time.Second"} {
		if !strings.Contains(main, field) {
			t.Errorf("http.Server 缺少 %s", field)
		}
	}
	if strings.Contains(main, "WriteTimeout:") {
		t.Error("不能设 WriteTimeout：长耗时的安装/下发请求会被截断")
	}
}

// 中优化：/api/servers 的 24h 流量只扫一遍 traffic_samples。
func TestServersTrafficSingleScan(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "server.go", "func (s *Server) servers(")
	if n := strings.Count(body, "FROM traffic_samples ts"); n != 1 {
		t.Errorf("/api/servers 应只扫一次 traffic_samples，实际 %d 次", n)
	}
	// 2026-11-03：portal 增量改为逐节点正向增量之和（单独一个只读 portal_used 行的子查询，
	// 走 (node_id, sampled_at, source) 唯一索引）；主表扫描仍只有一次。
	if !strings.Contains(body, "p.source = 'portal_used'") || !strings.Contains(body, "SUM(GREATEST(d.delta, 0))") {
		t.Error("portal 增量必须只在 portal_used 样本里按正向增量累加")
	}
}

// 中优化：测速最新一轮按节点走索引取 1 行，不再 DISTINCT ON 扫 7 天。
func TestSpeedtestLatestRoundUsesLateralLimit(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "monitoring.go", "func (s *Server) loadSpeedtestSummaries(")
	if strings.Contains(body, "DISTINCT ON (node_id)") || !strings.Contains(body, "CROSS JOIN LATERAL") || !strings.Contains(body, "LIMIT 1") {
		t.Error("最新一轮应改为 LATERAL ... LIMIT 1")
	}
}

// 日志降噪：缺遥测的 WARN 只在状态变化时打。
func TestIncompleteTelemetryLogOnlyOnChange(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "devicelimit.go", "func (s *Server) checkDeviceLimits(")
	warn := strings.Index(body, `"device limit: incomplete node telemetry`)
	if warn < 0 {
		t.Fatal("找不到缺遥测日志")
	}
	if !strings.Contains(body[:warn], "!deviceLimits.incomplete[subID]") {
		t.Error("缺遥测 WARN 必须只在订阅从完整变为不完整时打，不能每 30 秒一条")
	}
}

// M2：概览页的 24h 指标和 /api/runtime 的最新采样时间都必须按主机走
// (server_id, sampled_at) 索引，不能对 server_metrics 只按 sampled_at 过滤（会全表扫）。
func TestServerMetricsReadsArePerHost(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "server.go", "func (s *Server) recentMetrics(")
	if !strings.Contains(body, "WHERE m.server_id = s.id AND m.sampled_at > now() - interval '24 hours'") {
		t.Error("recentMetrics 必须按主机 LATERAL 过滤 server_id，否则全表扫 server_metrics")
	}
	rt := funcBlock(t, "manage.go", "func (s *Server) runtimeStatus(")
	if strings.Contains(rt, "`SELECT MAX(sampled_at) FROM server_metrics`") {
		t.Error("runtimeStatus 不能对 server_metrics 做全表 MAX")
	}
}

// M4 行为测试：用真实路由表，operator 调这四个写接口必须拿到 403。
func TestSensitiveWriteRoutesRejectOperator(t *testing.T) {
	t.Parallel()
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret", JWTTTL: time.Minute})
	s.sessionUser = func(context.Context, string) (string, string, int64, error) {
		return "operator", "op@example.test", 0, nil
	}
	token, err := s.signer.Issue("user-op", "op@example.test", "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/servers",
		"/api/servers/00000000-0000-0000-0000-000000000001/credential",
		"/api/servers/00000000-0000-0000-0000-000000000001/write-ops",
		"/api/xui/panels",
		// 2026-11-04：补删孤儿账号会用留存密钥删节点上的买家账号，同样只给管理员。
		"/api/orphans/00000000-0000-0000-0000-000000000001/retry",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("operator POST %s：want 403, got %d", path, rec.Code)
		}
	}
}

// 2026-11-03：operator 改主机 SSH 地址/端口/用户或重置指纹必须 403（会清空指纹，凭据随即发往新主机）；
// 只改名称等日常字段不受影响（这里不连库，能走到「没有可更新字段」之外的 DB 路径即说明没被角色拦下）。
func TestOperatorCannotRetargetHostSSH(t *testing.T) {
	t.Parallel()
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret", JWTTTL: time.Minute})
	s.sessionUser = func(context.Context, string) (string, string, int64, error) {
		return "operator", "op@example.test", 0, nil
	}
	token, err := s.signer.Issue("user-op", "op@example.test", "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"host":"203.0.113.9"}`, `{"ssh_port":2222}`, `{"ssh_user":"admin"}`, `{"reset_host_key":true}`} {
		req := httptest.NewRequest(http.MethodPatch, "/api/servers/00000000-0000-0000-0000-000000000001", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("operator PATCH %s：want 403, got %d", body, rec.Code)
		}
	}
	body := funcBlock(t, "manage.go", "func (s *Server) updateServer(")
	guard := strings.Index(body, `touchesEndpoint && claims.Role != "admin"`)
	update := strings.Index(body, `query := "UPDATE servers SET "`)
	if guard < 0 || update < 0 || guard > update {
		t.Fatal("the endpoint role check must run before any UPDATE")
	}
}

// 2026-11-06：核对会话时数据库出错要回 503（前端据此保留会话），不能回 401 把所有人踢回登录页；
// 用户确实不存在 / 令牌版本过期仍是 401。
func TestSessionCheckDatabaseErrorIsNot401(t *testing.T) {
	t.Parallel()
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, Options{JWTSecret: "test-secret-test-secret-test-secret", JWTTTL: time.Minute})
	token, err := s.signer.Issue("user-a", "a@example.test", "admin")
	if err != nil {
		t.Fatal(err)
	}
	call := func(lookup func(context.Context, string) (string, string, int64, error)) int {
		s.sessionUser = lookup
		req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.authenticated(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(rec, req)
		return rec.Code
	}
	if code := call(func(context.Context, string) (string, string, int64, error) {
		return "", "", 0, errors.New("connection refused")
	}); code != http.StatusServiceUnavailable {
		t.Fatalf("a database error while checking the session must be 503, got %d", code)
	}
	if code := call(func(context.Context, string) (string, string, int64, error) {
		return "", "", 0, pgx.ErrNoRows
	}); code != http.StatusUnauthorized {
		t.Fatalf("a deleted user stays 401, got %d", code)
	}
	if code := call(func(context.Context, string) (string, string, int64, error) {
		return "admin", "a@example.test", 7, nil
	}); code != http.StatusUnauthorized {
		t.Fatalf("a revoked token version stays 401, got %d", code)
	}
}
