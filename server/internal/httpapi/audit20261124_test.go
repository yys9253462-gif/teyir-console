package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func init() {
	// 允许既有单元测试中的本地明文回环桩服务通过 checkDeliveryBase；HTTPS 仍强制走公网地址校验。
	allowPlainHTTPLoopbackForTest = true
}

// TestBindNodeAPIReturns503WithoutCryptoBox 验证未配置加密密钥时绑定节点接口直接返回 503，
// 避免 s.box 为 nil 触发空指针崩溃。
func TestBindNodeAPIReturns503WithoutCryptoBox(t *testing.T) {
	s := &Server{}
	body := `{"url":"https://203.0.113.10:8443","api_key":"fake-api-key-test-only"}`
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/00000000-0000-0000-0000-000000000001/api", strings.NewReader(body))
	req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()

	s.bindNodeAPI(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未启用凭据加密时应返回 503，实际状态码 %d，响应体: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "凭据加密未启用") {
		t.Fatalf("响应文案应明确提示凭据加密未启用，实际: %s", rec.Body.String())
	}
}

// TestDeliveryDomainRejectsNonPublicTargets 验证交付域名归一化与连通探针拒绝内网、回环与云元数据地址。
func TestDeliveryDomainRejectsNonPublicTargets(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "https://10.0.0.1", "https://169.254.169.254", "https://100.100.100.200"} {
		if _, err := normalizeDeliveryBase(raw); err == nil {
			t.Errorf("normalizeDeliveryBase(%q) 应当拒绝非公网地址", raw)
		}
	}
	for _, target := range []string{"https://127.0.0.1", "https://169.254.169.254", "https://10.0.0.1"} {
		err := checkDeliveryBase(context.Background(), target)
		if err == nil {
			t.Errorf("checkDeliveryBase(%q) 应当拒绝访问非公网地址", target)
			continue
		}
		if !strings.Contains(err.Error(), "内网或保留地址") {
			t.Errorf("checkDeliveryBase(%q) 报错应明确指出内网或保留地址，实际: %v", target, err)
		}
	}
	if deliveryCheckClient == nil || deliveryCheckClient.Transport == nil {
		t.Fatal("交付域名探针应复用包级连接池客户端")
	}
}

// TestLockNodeWriteCtxReleaseIsIdempotent 验证节点写锁释放闭包可重复调用而不阻塞或破坏锁语义。
func TestLockNodeWriteCtxReleaseIsIdempotent(t *testing.T) {
	s := &Server{}
	release, err := s.lockNodeWriteCtx(context.Background(), "node-idempotent-test")
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	done := make(chan struct{})
	go func() {
		release()
		release()
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("重复调用节点写锁释放函数发生阻塞死锁")
	}
	again, err := s.lockNodeWriteCtx(context.Background(), "node-idempotent-test")
	if err != nil {
		t.Fatalf("释放后再次加锁失败: %v", err)
	}
	again()
}

// TestInstallJobStoreAppendLogsMasksAndTracksProgress 验证安装任务批量日志脱敏与增量脱敏进度追踪。
func TestInstallJobStoreAppendLogsMasksAndTracksProgress(t *testing.T) {
	st := &installJobStore{
		jobs:     map[string]*installJob{},
		byServer: map[string]string{},
		owned:    map[string]bool{},
	}
	st.jobs["job-1"] = &installJob{
		ID:        "job-1",
		ServerID:  "srv-1",
		Status:    "running",
		StartedAt: time.Now(),
	}
	st.appendLogs("job-1", []string{
		"\x1b[32m正常安装步骤\x1b[0m\n  password=fake-secret-value  \n通信密钥 hy2_sec_FAKE0000TEST",
	})
	st.mu.Lock()
	job := st.jobs["job-1"]
	if len(job.Log) != 3 {
		st.mu.Unlock()
		t.Fatalf("期望拆出 3 行日志，实际 %d 行: %#v", len(job.Log), job.Log)
	}
	if job.Log[0] != "正常安装步骤" {
		st.mu.Unlock()
		t.Fatalf("首行应去除颜色控制码，实际: %q", job.Log[0])
	}
	if strings.Contains(job.Log[1], "fake-secret-value") {
		st.mu.Unlock()
		t.Fatalf("含密码行未被脱敏: %q", job.Log[1])
	}
	if strings.Contains(job.Log[2], "FAKE0000TEST") {
		st.mu.Unlock()
		t.Fatalf("含通信密钥行未被脱敏: %q", job.Log[2])
	}
	if job.cleanCount != 3 {
		st.mu.Unlock()
		t.Fatalf("批量写入后 cleanCount 应为 3，实际 %d", job.cleanCount)
	}
	st.evictLocked(time.Now())
	if job.cleanCount != 3 {
		st.mu.Unlock()
		t.Fatalf("evictLocked 后 cleanCount 应保持 3，实际 %d", job.cleanCount)
	}
	st.mu.Unlock()
}

// TestInvalidateHealthGateCacheResetsTimestampAndGeneration 验证健康闸门缓存主动失效函数行为。
func TestInvalidateHealthGateCacheResetsTimestampAndGeneration(t *testing.T) {
	healthGateCache.Lock()
	healthGateCache.at = time.Now()
	prevGen := healthGateCache.generation
	healthGateCache.Unlock()

	invalidateHealthGateCache()

	healthGateCache.Lock()
	afterAt := healthGateCache.at
	afterGen := healthGateCache.generation
	healthGateCache.Unlock()

	if !afterAt.IsZero() {
		t.Fatalf("健康闸门缓存失效后时间戳应归零，实际 %v", afterAt)
	}
	if afterGen != prevGen+1 {
		t.Fatalf("健康闸门缓存失效后代数应加 1：旧=%d，新=%d", prevGen, afterGen)
	}
}

// TestNodesQueryUsesSingleTrafficLateral 验证 /api/nodes 仅对 traffic_samples 做一次侧向扫描。
func TestNodesQueryUsesSingleTrafficLateral(t *testing.T) {
	body := funcBlock(t, "server.go", "func (s *Server) nodes(")
	if n := strings.Count(body, "FROM traffic_samples ts"); n != 1 {
		t.Fatalf("/api/nodes 应合并为单次 traffic_samples 侧向扫描，实际 %d 次", n)
	}
	for _, src := range []string{"'node_iface'", "'portal_used'", "'hysteria_traffic'"} {
		if !strings.Contains(body, src) {
			t.Errorf("/api/nodes 合并聚合查询缺少流量来源 %s", src)
		}
	}
}

// TestNodeMutationsInvalidateSubscriptionAndHealthCaches 验证改变节点可见性/参数的写路径均补齐缓存失效。
func TestNodeMutationsInvalidateSubscriptionAndHealthCaches(t *testing.T) {
	cases := []struct {
		file, sig string
		wants     []string
	}{
		{"manage.go", "func (s *Server) deleteNode(", []string{"invalidateHealthGateCache()", "InvalidateSubscriptionCache()"}},
		{"nodeapi.go", "func (s *Server) bindNodeAPI(", []string{"InvalidateSubscriptionCache()"}},
		{"nodeapi.go", "func (s *Server) refreshNodeAPI(", []string{"InvalidateSubscriptionCache()"}},
		{"nodeapi.go", "func (s *Server) unbindNodeAPI(", []string{"InvalidateSubscriptionCache()"}},
		{"nodetraffic.go", "func (s *Server) setNodeSuspension(", []string{"invalidateHealthGateCache()", "InvalidateSubscriptionCache()"}},
		{"migratenode.go", "func (s *Server) runMigrateNode(", []string{"invalidateHealthGateCache()", "InvalidateSubscriptionCache()"}},
	}
	for _, tc := range cases {
		body := funcBlock(t, tc.file, tc.sig)
		for _, want := range tc.wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s 中的 %s 缺少缓存失效调用 %s", tc.file, tc.sig, want)
			}
		}
	}
}

// TestReadHandlersEnforceReadLimitAndPortalBatchOrder 验证补齐的只读限流及批量门户写库前置校验顺序。
func TestReadHandlersEnforceReadLimitAndPortalBatchOrder(t *testing.T) {
	for _, tc := range []struct{ file, sig string }{
		{"server.go", "func (s *Server) serverMetrics("},
		{"server.go", "func (s *Server) credentialMeta("},
		{"server.go", "func (s *Server) taskLogs("},
		{"manage.go", "func (s *Server) alertEvents("},
		{"manage.go", "func (s *Server) trafficHourly("},
		{"healthgate.go", "func (s *Server) getAutoHide("},
		{"deliverydomain.go", "func (s *Server) getDeliveryDomain("},
		{"alertnotify.go", "func (s *Server) alertWebhookGet("},
		{"installhy2.go", "func (s *Server) getInstallJob("},
		{"scheduler.go", "func (s *Server) listSchedules("},
	} {
		body := funcBlock(t, tc.file, tc.sig)
		if !strings.Contains(body, "enforceReadLimit(") {
			t.Errorf("%s 中的 %s 缺少读限流 enforceReadLimit", tc.file, tc.sig)
		}
	}
	if body := funcBlock(t, "hostops.go", "func (s *Server) terminalCookie("); !strings.Contains(body, "enforceWriteLimit(") {
		t.Error("hostops.go 中的 terminalCookie 缺少写限流 enforceWriteLimit")
	}
	for _, sig := range []string{"func (s *Server) startPortalBatch(", "func (s *Server) getPortalBatch("} {
		body := funcBlock(t, "portalbatch.go", sig)
		expireAt := strings.Index(body, "s.expirePortalBatches(")
		limitAt := strings.Index(body, "enforce")
		if expireAt < 0 || limitAt < 0 || expireAt < limitAt {
			t.Errorf("portalbatch.go 中的 %s 必须先限流与校验，再调用 expirePortalBatches", sig)
		}
	}
}

// TestTelegramBanAndDeviceLimitHardening 验证宿主机封禁联动客户端加固与设备上限无锁查库。
func TestTelegramBanAndDeviceLimitHardening(t *testing.T) {
	for _, sig := range []string{"func (s *Server) telegramBanIPAction(", "func (s *Server) telegramBanQuery("} {
		body := funcBlock(t, "telegram_control.go", sig)
		if strings.Contains(body, "http.DefaultClient") {
			t.Errorf("telegram_control.go 中的 %s 不得使用全局 http.DefaultClient", sig)
		}
		if !strings.Contains(body, "banWebhookClient.Do(") || !strings.Contains(body, "io.LimitReader(") {
			t.Errorf("telegram_control.go 中的 %s 必须使用 banWebhookClient 与 io.LimitReader", sig)
		}
	}
	dl := funcBlock(t, "devicelimit.go", "func (s *Server) checkDeviceLimits(")
	if !strings.Contains(dl, "queryDeviceLimitAdmissions(ctx)") {
		t.Error("checkDeviceLimits 应在互斥锁外调用 queryDeviceLimitAdmissions 查库")
	}
	if !strings.Contains(dl, "delete(deviceLimits.warned,") {
		t.Error("checkDeviceLimits 应清理过期的 deviceLimits.warned 记录")
	}
	native := funcBlock(t, "ippurity.go", "func (s *Server) checkAndStoreNative(")
	if n := strings.Count(native, "UPDATE node_ip_purity SET"); n != 1 {
		t.Errorf("checkAndStoreNative 应合并为单条原子 UPDATE，实际 %d 条", n)
	}
	profit := funcBlock(t, "reports.go", "func (s *Server) profitReport(")
	if !strings.Contains(profit, "WHERE n.deleted_at IS NULL AND s.deleted_at IS NULL") {
		t.Error("profitReport 节点列表查询必须过滤已软删主机")
	}
	xui := funcBlock(t, "xuihandlers.go", "func (s *Server) xuiCreatePanel(")
	if strings.Contains(xui, `"name": input.Name`) {
		t.Error("xuiCreatePanel 审计日志应记录实际入库的 name，而非可能为空的 input.Name")
	}
}
