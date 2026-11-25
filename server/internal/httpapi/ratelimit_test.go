package httpapi

// ratelimit_test.go —— 分层配额与节点读闸门的单测。
//
// 这两件事都不是「顺手加的功能」，各自对应一个实测到的失效：
//
//  1. 读写共用一个 20 次/10 秒的桶。节点运维页签每刷新一次就是一条
//     portal-status（内含七路并发探测），连点几下再开一个标签页，就把
//     写操作的配额一起吃掉了 —— 于是「切换 Reality」返回 429，而它自己
//     一次都还没发过。
//  2. 只读探测的并发没有上界。写侧早就有 lockNodeWrite 串行化，读侧却
//     一直敞着，七路 × 两个标签页 = 十四路并发砸向节点侧那个单线程的
//     Python http.server。
//
// 所以下面的断言都是「机制是否真的在生效」，不是「函数返回了非 nil」。

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer 只装限流器 —— 这些测试不碰数据库。
func newTestServer(window time.Duration, readLimit, writeLimit int) *Server {
	return &Server{
		readLimiter:   newSlidingLimiter(window, readLimit),
		writeLimiter:  newSlidingLimiter(window, writeLimit),
		publicLimiter: newSlidingLimiter(window, writeLimit),
	}
}

func TestReadQuotaIsNotConsumedByWrites(t *testing.T) {
	// 这是本功能的核心断言：写被限，读照样能读。
	s := newTestServer(time.Minute, 5, 3)
	const user = "0e3f1b6a-6f5a-4a0e-9c2f-1d2e3f4a5b6c"

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		if !s.enforceWriteLimit(rec, user) {
			t.Fatalf("第 %d 次写不该被限（配额 3）", i+1)
		}
	}

	rec := httptest.NewRecorder()
	if s.enforceWriteLimit(rec, user) {
		t.Fatal("写配额应当已经用尽")
	}

	// 同一个人，紧接着读 —— 必须还能读满自己的 5 次。
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		if !s.enforceReadLimit(rec, user) {
			t.Fatalf("写被限不该影响读：第 %d 次读被误限", i+1)
		}
	}
}

func TestWriteQuotaIsNotConsumedByReads(t *testing.T) {
	// 反向也要成立。否则「刷新几下状态，写入就不可用」这个老毛病还在。
	s := newTestServer(time.Minute, 50, 3)
	const user = "0e3f1b6a-6f5a-4a0e-9c2f-1d2e3f4a5b6c"

	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		if !s.enforceReadLimit(rec, user) {
			t.Fatalf("第 %d 次读不该被限（配额 50）", i+1)
		}
	}

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		if !s.enforceWriteLimit(rec, user) {
			t.Fatalf("读被限不该影响写：第 %d 次写被误限", i+1)
		}
	}
}

func TestPublicQuotaIsIsolatedFromAuthenticatedTraffic(t *testing.T) {
	// 公开订阅端点和写操作曾经共用同一个 limiter 实例。键不同所以当下不会
	// 互相挤占，但那是"碰巧"—— 这里把隔离钉成契约：公开流量打满，
	// 管理端写操作必须不受影响。
	s := newTestServer(time.Minute, 50, 2)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		if !s.enforcePublicLimit(rec, "sub:"+strings.Repeat("a", 32)) {
			t.Fatalf("第 %d 次公开读取不该被限", i+1)
		}
	}
	rec := httptest.NewRecorder()
	if s.enforcePublicLimit(rec, "sub:"+strings.Repeat("a", 32)) {
		t.Fatal("公开配额应当已经用尽")
	}

	rec = httptest.NewRecorder()
	if !s.enforceWriteLimit(rec, "0e3f1b6a-6f5a-4a0e-9c2f-1d2e3f4a5b6c") {
		t.Error("公开订阅把配额用尽后，管理端写操作仍应可用")
	}
}

func TestReadQuotaIsLooserThanWriteQuota(t *testing.T) {
	// 数值上的契约：读必须比写宽。写死这条，是为了让"为了省事把两者调成一样"
	// 这种改动在校验阶段就红，而不是等操作者又碰到 429 才发现。
	s := New(nil, nil, nil, Options{})
	if s.readLimiter.limit <= s.writeLimiter.limit {
		t.Errorf("读配额(%d)应当明显宽于写配额(%d)", s.readLimiter.limit, s.writeLimiter.limit)
	}
	if s.readLimiter.window != s.writeLimiter.window {
		t.Errorf("读写窗口应当一致，实际 %v / %v", s.readLimiter.window, s.writeLimiter.window)
	}
	// 登录桶必须与前三者完全独立：管理端被限流不该顺手把登录也锁死。
	if s.loginLimiter == s.readLimiter || s.loginLimiter == s.writeLimiter || s.loginLimiter == s.publicLimiter {
		t.Error("登录桶不能与读/写/公开桶共用实例")
	}
	if s.publicLimiter == s.writeLimiter || s.publicLimiter == s.readLimiter {
		t.Error("公开桶必须是独立实例")
	}
}

func TestLimit429CarriesRetryAfterAndScope(t *testing.T) {
	s := newTestServer(time.Minute, 1, 1)

	rec := httptest.NewRecorder()
	if !s.enforceReadLimit(rec, "u1") {
		t.Fatal("第一次读应当放行")
	}
	rec = httptest.NewRecorder()
	if s.enforceReadLimit(rec, "u1") {
		t.Fatal("第二次读应当被限")
	}

	if rec.Code != 429 {
		t.Fatalf("状态码应为 429，实际 %d", rec.Code)
	}
	// Retry-After 是标准头，客户端（和运维脚本）靠它决定什么时候重试。
	header := rec.Header().Get("Retry-After")
	if header == "" {
		t.Fatal("429 必须带 Retry-After 头")
	}
	headerSeconds, err := strconv.Atoi(header)
	if err != nil {
		t.Fatalf("Retry-After 必须是整数秒，实际 %q", header)
	}
	if headerSeconds < 1 || headerSeconds > 60 {
		t.Errorf("Retry-After 应落在 (0, 窗口秒数] 内，实际 %d", headerSeconds)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 响应体不是合法 JSON: %v", err)
	}
	if body["limit_scope"] != string(scopeRead) {
		t.Errorf("limit_scope 应为 %q，实际 %v", scopeRead, body["limit_scope"])
	}
	// 头与体必须说同一件事。两处各算一次时间是很容易分叉的写法。
	if int(body["retry_after_seconds"].(float64)) != headerSeconds {
		t.Errorf("响应体里的 retry_after_seconds(%v) 与 Retry-After 头(%d) 不一致",
			body["retry_after_seconds"], headerSeconds)
	}
}

func TestWrite429SaysTheActionWasNotSent(t *testing.T) {
	// 写被限和读被限对操作者的含义完全不同：读只是"点太快"，写意味着
	// **刚才那个操作没有下发到节点**。如果文案不说这件事，操作者会以为
	// 开关已经切过去了 —— 那是一个会误导人的沉默失败。
	s := newTestServer(time.Minute, 10, 1)
	rec := httptest.NewRecorder()
	s.enforceWriteLimit(rec, "u2")
	rec = httptest.NewRecorder()
	s.enforceWriteLimit(rec, "u2")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if body["limit_scope"] != string(scopeWrite) {
		t.Fatalf("limit_scope 应为 %q，实际 %v", scopeWrite, body["limit_scope"])
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "未下发") {
		t.Errorf("写的 429 文案必须说明操作没有下发，实际 %q", message)
	}
}

func TestRetryAfterUsesOldestAttemptNotNewest(t *testing.T) {
	// Retry-After 必须按「最老那次尝试滑出窗口」算，而不是「最后一次 + 窗口」。
	// 两者只有在历史里有多条记录时才会分叉，所以这里刻意让两条记录拉开距离：
	//
	//	窗口 3s，配额 2
	//	t≈0.0s  第 1 次 → 放行，记录 t0
	//	t≈1.5s  第 2 次 → 放行，记录 t1
	//	t≈1.5s  第 3 次 → 被限
	//	          正确：t0 + 3 - 1.5 = 1.5s → 上取整 2
	//	          写错：t1 + 3 - 1.5 = 3.0s → 上取整 3
	//
	// 所以断言 ≤ 2。写成「最后一次 + 窗口」会让操作者照着一个虚高的数字
	// 干等一倍时间。
	const window = 3 * time.Second
	s := newTestServer(window, 10, 2)
	const user = "u-oldest"

	rec := httptest.NewRecorder()
	if !s.enforceWriteLimit(rec, user) {
		t.Fatal("第 1 次应当放行")
	}
	time.Sleep(1500 * time.Millisecond)
	rec = httptest.NewRecorder()
	if !s.enforceWriteLimit(rec, user) {
		t.Fatal("第 2 次应当放行（配额 2）")
	}
	rec = httptest.NewRecorder()
	if s.enforceWriteLimit(rec, user) {
		t.Fatal("第 3 次应当被限")
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	seconds := int(body["retry_after_seconds"].(float64))
	if seconds < 1 {
		t.Errorf("retry_after_seconds 不应小于 1，实际 %d", seconds)
	}
	if seconds > 2 {
		t.Errorf("等待时间按最老那次算应当 ≤ 2 秒，实际 %d（疑似改成了「最后一次 + 窗口」）", seconds)
	}
}

func TestLimiterSweepsSubjectsWhoseWindowHasPassed(t *testing.T) {
	// attempts 的键是「见过的主体」：认证用户是 UUID，公开订阅端点是每个
	// 订阅各自的 token，都不会重复。逐次清理只能回收"又出现了的主体"，
	// 回收不了那些再也不出现的，所以必须有一个按规模触发的全量清理，
	// 否则这是一份只增不减的内存账。
	// 窗口给到 60ms：够长，保证下面 4104 次插入都发生在同一个窗口内
	// （一次 map 插入是微秒级，整轮远早于 60ms）；又够短，等 80ms 就能
	// 让它们全部过期。
	l := newSlidingLimiter(60*time.Millisecond, 1_000_000)

	for i := 0; i < limiterSweepThreshold+8; i++ {
		l.allow("stale-" + strconv.Itoa(i))
	}
	l.mu.Lock()
	before := len(l.attempts)
	l.mu.Unlock()
	if before <= limiterSweepThreshold {
		t.Fatalf("准备阶段应当积压超过阈值 %d，实际 %d", limiterSweepThreshold, before)
	}

	time.Sleep(80 * time.Millisecond)

	// 这一次调用会越过阈值并触发全量清理，把上面那些已经过期的键全部丢掉。
	//
	// ⚠️ 2026-10-30：全量清理现在**限频**（5 秒最多一次，避免超阈值后每次请求
	// 都做 O(n) 扫描、把登录串行化）。这里把"上次清理时刻"拨回去，模拟间隔已到；
	// 被测的契约仍然是「按规模触发清理」，不是「每次调用都清理」。
	l.mu.Lock()
	l.lastSweep = time.Time{}
	l.mu.Unlock()
	l.allow("fresh")
	l.mu.Lock()
	after := len(l.attempts)
	l.mu.Unlock()

	if after != 1 {
		t.Errorf("全量清理后应当只剩刚写入的那一个主体，实际 %d（清理没生效）", after)
	}
}

func TestLimiterDropsKeyWhenHistoryEmpties(t *testing.T) {
	// 同一主体隔了一个窗口再回来：旧记录应当被丢弃，而不是与新记录累积。
	l := newSlidingLimiter(20*time.Millisecond, 2)
	l.allow("a")
	l.allow("a")
	if !l.allow("b") {
		t.Fatal("不同主体应当各有配额")
	}

	time.Sleep(40 * time.Millisecond)
	// 窗口已过，前两条记录应当不再计入。
	if !l.allow("a") {
		t.Fatal("窗口滑过后配额应当恢复")
	}
	l.mu.Lock()
	history := len(l.attempts["a"])
	l.mu.Unlock()
	if history != 1 {
		t.Errorf("过期记录应当被丢弃，实际保留 %d 条", history)
	}
}

func TestNodeReadGateBoundsConcurrency(t *testing.T) {
	// 上界必须是 nodeReadConcurrency，而不是退化成 1。
	// 断言"恰好等于"而不是"小于等于"：如果闸门实现成了全局互斥，
	// 「≤4」仍然成立，但那会让 portal-status 从一次往返变成七次 ——
	// 那是把保护措施做成了性能事故。
	s := &Server{}
	const node = "27de1727-1f07-4881-b183-45a5ad03e976"
	ctx := context.Background()

	var concurrent, peak int32
	var wg sync.WaitGroup
	for i := 0; i < nodeReadConcurrency*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.lockNodeRead(ctx, node)
			if err != nil {
				t.Errorf("取读闸门失败: %v", err)
				return
			}
			defer release()
			n := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
		}()
	}
	wg.Wait()

	if peak != nodeReadConcurrency {
		t.Errorf("同节点读并发峰值应为 %d，实际 %d", nodeReadConcurrency, peak)
	}
}

func TestNodeReadGateIsPerNode(t *testing.T) {
	// 一个节点被探测卡住，不该拖住其他节点的状态页。
	s := &Server{}
	ctx := context.Background()
	const busy = "27de1727-1f07-4881-b183-45a5ad03e976"
	const other = "9c8f4d2e-5a6b-4c7d-8e9f-0a1b2c3d4e5f"

	held := make([]func(), 0, nodeReadConcurrency)
	for i := 0; i < nodeReadConcurrency; i++ {
		release, err := s.lockNodeRead(ctx, busy)
		if err != nil {
			t.Fatalf("占位失败: %v", err)
		}
		held = append(held, release)
	}
	defer func() {
		for _, release := range held {
			release()
		}
	}()

	done := make(chan struct{})
	go func() {
		release, err := s.lockNodeRead(ctx, other)
		if err == nil {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("另一个节点不该被占满的节点阻塞")
	}
}

func TestNodeReadGateGivesUpWhenContextExpires(t *testing.T) {
	// 槽位取不到时必须能放弃。否则一个卡住的门户会让请求无限期挂着，
	// 占住 HTTP 连接直到客户端超时 —— 那是把节点故障放大成面板故障。
	s := &Server{}
	const node = "27de1727-1f07-4881-b183-45a5ad03e976"

	held := make([]func(), 0, nodeReadConcurrency)
	for i := 0; i < nodeReadConcurrency; i++ {
		release, err := s.lockNodeRead(context.Background(), node)
		if err != nil {
			t.Fatalf("占位失败: %v", err)
		}
		held = append(held, release)
	}
	defer func() {
		for _, release := range held {
			release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := s.lockNodeRead(ctx, node); err == nil {
		t.Fatal("槽位已满且 ctx 超时，应当返回错误而不是一直等下去")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("放弃得太慢：%v", elapsed)
	}

	// 全部释放后应当立刻可取 —— 证明上面那次失败没有把槽位弄丢。
	// 注意先把 held 清空：defer 里还会再释放一遍，同一个槽位释放两次
	// 会把通道抽空并挂死在这里（这正是本测试第一次写完就挂 10 分钟的原因）。
	for _, release := range held {
		release()
	}
	held = held[:0]

	release, err := s.lockNodeRead(context.Background(), node)
	if err != nil {
		t.Fatalf("释放槽位后应当能取到: %v", err)
	}
	release()
}

func TestNodeReadGateReleaseIsIdempotent(t *testing.T) {
	// 释放函数被重复调用必须无害。信号量是「发送即获取、接收即释放」，
	// 多接收一次就把通道抽空了，之后这个节点所有释放都会阻塞 ——
	// 即读闸门永久卡死，且只在释放路径上表现，极难定位。
	s := &Server{}
	const node = "27de1727-1f07-4881-b183-45a5ad03e976"

	release, err := s.lockNodeRead(context.Background(), node)
	if err != nil {
		t.Fatalf("取槽位失败: %v", err)
	}
	release()
	release() // 第二次必须无事发生，而不是吃掉一个别人的槽位
	release()

	// 抽空与否可以直接看出来：连续取满 nodeReadConcurrency 个槽位应当都能成功。
	again := make([]func(), 0, nodeReadConcurrency)
	for i := 0; i < nodeReadConcurrency; i++ {
		got, err := s.lockNodeRead(context.Background(), node)
		if err != nil {
			t.Fatalf("第 %d 次取槽位失败（通道可能被多次释放抽空了）: %v", i+1, err)
		}
		again = append(again, got)
	}
	for _, r := range again {
		r()
	}
}

func TestAmbiguousRateLimitHelperIsGone(t *testing.T) {
	// 这里曾经有一个 requireRateLimit(subject) 作为"默认桶"的入口。
	// 它的存在本身就是隐患：新增一个 handler 时随手调它，就悄悄落进了
	// 写桶或读桶，而调用处看不出是哪一桶。改成明确的
	// enforceReadLimit / enforceWriteLimit 之后，把这个名字钉死在门外，
	// 防止有人"顺手"把它加回来。
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if name == "ratelimit_test.go" {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if strings.Contains(string(raw), "requireRateLimit") {
			t.Errorf("%s 里还留着 requireRateLimit —— 请改用 enforceReadLimit 或 enforceWriteLimit", name)
		}
	}
}
