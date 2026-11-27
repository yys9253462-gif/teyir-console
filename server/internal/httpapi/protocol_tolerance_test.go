package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 2026-10-25 五轮排查第一轮：未知协议段不得拖垮已知协议。
//
// 回归的是真实缺陷：fetchNodeProtocols 曾把 protocols 解成
// map[string]map[string]any，遇到 {"future_protocol": true} 时整个
// Unmarshal 失败 ⇒ Reality / VMess / VLESS Encryption 三条通道一起消失，
// 且没有任何日志（错误按设计吞成 nil）。
const testPortalAPIKey = "0123456789abcdef0123456789abcdef"

func TestUnknownProtocolShapeDoesNotBreakKnownProtocols(t *testing.T) {
	var payload struct {
		Protocols map[string]any `json:"protocols"`
	}
	body := `{"protocols":{
		"hysteria2":{"installed":true,"active":true},
		"vless_reality":{"installed":true,"active":true,"configured":true,
			"public_key":"pbk","short_id":"sid","dest_sni":"www.example.com","port":443},
		"vmess":{"installed":true,"active":true,"configured":true,
			"domain":"vm.example.com","port":8443,"ws_path":"/ws"},
		"vlessenc":{"installed":true,"active":true,"configured":true,
			"domain":"enc.example.com","port":2088},
		"future_protocol":true,
		"another_future":["a","b"]
	}}`
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("未知协议形状不应让整包解析失败: %v", err)
	}
	protocols := protocolSections(payload.Protocols)
	if len(protocols) != 4 {
		t.Fatalf("已知协议段数量 = %d, want 4 (未知形状应被忽略)", len(protocols))
	}
	if _, ok := protocols["future_protocol"]; ok {
		t.Fatal("非对象形状的协议段不应进入结果")
	}
	if !capabilityBool(protocols["vless_reality"], "active") {
		t.Fatal("Reality 段被未知协议拖垮了")
	}
	if vmessFromCapability(protocols["vmess"]) == nil {
		t.Fatal("VMess 段被未知协议拖垮了")
	}
	if vlessEncFromCapability(protocols["vlessenc"]) == nil {
		t.Fatal("VLESS Encryption 段被未知协议拖垮了")
	}
}

// 端到端：节点返回未知协议形状时，已知协议仍能被解析出来。
//
// 注意这里刻意**不放** vmess 段：现代门户带 vmess 时，Reality 必须靠
// Basic 会话确认入站（共享 Xray 进程不能作为证据），而本用例的 nodeID 为空、
// 拿不到 Basic 凭据 —— 那是另一条设计好的规则，与本用例要验证的
// 「未知协议不拖垮解析」无关。
func TestFetchNodeProtocolsToleratesUnknownProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/capabilities" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"protocols":{
			"vless_reality":{"installed":true,"active":true,"configured":true,
				"public_key":"pbk","short_id":"sid","dest_sni":"www.example.com","port":443},
			"future_protocol":true,
			"another_future":["a","b"]
		}}`))
	}))
	defer server.Close()
	old := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = old })

	// nodeID 传空：跳过需要 Basic 凭据的两步，只验证解析本身。
	reality, _, _ := (&Server{}).fetchNodeProtocols(context.Background(), "", server.URL, testPortalAPIKey)
	if reality == nil {
		t.Fatal("未知协议形状让 Reality 丢失了")
	}
	if reality.PublicKey != "pbk" || reality.Port != 443 {
		t.Fatalf("Reality 参数解析错误: %#v", reality)
	}
}

// 同一节点同时有 vmess 与未知协议时，VMess 也必须解析成功（Reality 是否下发
// 由 Basic 入站判据单独决定，不在此断言）。
func TestFetchNodeProtocolsKeepsVMessWithUnknownProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"protocols":{
			"vmess":{"installed":true,"active":true,"configured":true,
				"domain":"vm.example.com","port":8443,"ws_path":"/ws"},
			"future_protocol":true
		}}`))
	}))
	defer server.Close()
	old := portalClient
	portalClient = server.Client()
	t.Cleanup(func() { portalClient = old })

	_, vmess, _ := (&Server{}).fetchNodeProtocols(context.Background(), "", server.URL, testPortalAPIKey)
	if vmess == nil {
		t.Fatal("未知协议形状让 VMess 丢失了")
	}
	if vmess.Domain != "vm.example.com" || vmess.Port != 8443 || vmess.WSPath != "/ws" {
		t.Fatalf("VMess 参数解析错误: %#v", vmess)
	}
}

// 2026-10-25 五轮排查第一轮：portal-status 的整批预算必须随探测路数增长。
//
// 原实现写死 15s，而探测从 7 路增到 9 路后要排三轮（nodeReadConcurrency=4），
// 单路上限本身也是 15s —— 最后一轮会被整批超时截断，表现为
// 「版本 / VMess / VLESS Encryption 状态偶发 unknown」。
func TestPortalStatusBudgetScalesWithProbeCount(t *testing.T) {
	// 整批预算必须覆盖「路数 / 并发」轮，且每轮都要有单路上限那么多时间。
	rounds := (portalStatusProbeCount + nodeReadConcurrency - 1) / nodeReadConcurrency
	if portalStatusTimeout < time.Duration(rounds)*portalStatusProbeTimeout {
		t.Fatalf("默认预算 %v 不足以覆盖 %d 轮", portalStatusTimeout, rounds)
	}
	// 单路预算必须严格小于整批预算：否则一个卡住的端点会吃干整批
	// （2026-10-25 五轮排查第二轮修的就是这个）。
	if portalStatusProbeTimeout >= portalStatusTimeout {
		t.Fatalf("单路 %v 不应 >= 整批 %v", portalStatusProbeTimeout, portalStatusTimeout)
	}
	if portalStatusBudget(0) <= 0 {
		t.Fatal("非法路数应退化为至少一轮，而不是 0")
	}
	// 路数越多，预算只增不减。
	if portalStatusBudget(portalStatusProbeCount) < portalStatusBudget(nodeReadConcurrency) {
		t.Fatal("预算必须随路数单调不减")
	}
}

// 2026-10-25 五轮排查第二轮：portal-status 的响应体不得携带管理员 URI / UUID。
//
// 上游 vmess-status / vlessenc-status 的 config.uri 是**管理员**链接
// （userinfo 是管理员 UUID）。这个结构体会被原样序列化给浏览器，
// 声明了 URI 字段就等于把管理员身份发给每个打开面板的会话 ——
// 而前端只用 domain / sni / ws_path / port，从不读 uri。
func TestPortalStatusPayloadsCarryNoAdminIdentity(t *testing.T) {
	for name, payload := range map[string]any{
		"vmess":    &portalVMessInfo{Installed: true, Active: true},
		"vlessenc": &portalVLESSEncInfo{Installed: true, Active: true},
	} {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{`"uri"`, `"uuid"`, `"qr_svg"`} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("%s 的 portal-status 响应泄漏了 %s: %s", name, banned, raw)
			}
		}
	}
}

// portalStatusProbeCount 必须与 probes 表的路数一致 —— 两者是同一件事。
// 加端点时忘了改常量，整批预算就会停留在旧路数上（第一轮踩到的正是这个）。
func TestPortalStatusProbeCountMatchesTable(t *testing.T) {
	// probes 表定义在 portalNodeStatus 内，无法直接引用；这里用源码级断言
	// 之外的等价办法：portalStatusProbeCount 必须覆盖「已知端点」的数量。
	// 端点清单与 probes 表一一对应，新增端点时这里也要加。
	known := []string{
		"check-version", "bbr-status", "reality-status", "vmess-status",
		"vlessenc-status", "anytls-status", "warp-status", "awg-state", "proxy-services", "traffic-speed",
	}
	if len(known) != portalStatusProbeCount {
		t.Fatalf("portalStatusProbeCount = %d, 已知端点 %d 个", portalStatusProbeCount, len(known))
	}
}

// 2026-10-25 五轮排查第三轮：端点缺失判据不能把端口号里的 404 当成状态码。
//
// 原来用裸子串 `strings.Contains(msg, "404")`，于是
// 「dial tcp 1.2.3.4:40401: connection refused」被判成「门户过旧」，
// 运维会去升级门户，而真实原因是节点连不上 —— 误诊方向完全相反。
func TestEndpointMissingIsNotFooledByPortNumbers(t *testing.T) {
	misleading := []string{
		"连接节点 API 失败: dial tcp 1.2.3.4:40401: connect: connection refused",
		"连接节点 API 失败: dial tcp 1.2.3.4:14040: i/o timeout",
		"节点 API 报错: 端口 40401 未监听",
		"连接节点 API 失败: dial tcp 10.0.4.04:443: no route to host",
	}
	for _, msg := range misleading {
		if isPortalEndpointMissing(msg) {
			t.Errorf("被端口号误导，误判为门户过旧: %s", msg)
		}
	}
	genuine := []string{
		"节点 API 报错: API endpoint not found",
		"节点 API 返回 404",
		"HTTP 404 Not Found",
		"节点 API 返回 404: {\"ok\":false}",
	}
	for _, msg := range genuine {
		if !isPortalEndpointMissing(msg) {
			t.Errorf("真实端点缺失未识别: %s", msg)
		}
	}
}

// 2026-10-25 五轮排查第三轮：安装后的重试判据不能被端口号里的 502/503/504 触发。
//
// 原来用裸子串匹配 502/503/504，`dial tcp 10.5.3.5:5040` 会命中 ⇒
// 一次确定性失败（例如密钥不对）被当成「服务还没起来」，白等约 2 分钟
// 重试 12 次，最后报「节点服务没就绪」掩盖真实原因。
func TestTransientJoinErrorIgnoresPortNumbers(t *testing.T) {
	deterministic := []string{
		"节点 API 报错: api_key 不被接受 (401): 端口 5040 配置错误",
		"节点 API 返回 5021 字节的异常响应",
		"portal_access.json 里的密钥长度 5030 不合法",
	}
	for _, msg := range deterministic {
		if transientJoinError(errors.New(msg)) {
			t.Errorf("确定性失败被当成临时错误，会白等重试: %s", msg)
		}
	}
	transient := []string{
		"节点 API 返回 502",
		"upstream returned 503 Service Unavailable",
		"gateway timeout 504",
		"connection refused",
		"i/o timeout",
	}
	for _, msg := range transient {
		if !transientJoinError(errors.New(msg)) {
			t.Errorf("临时错误未被识别: %s", msg)
		}
	}
}

// 2026-10-25 五轮排查第三轮：「没绑凭据」与「凭据读取失败」必须分开说。
//
// 两者处理动作不同（去设置页补 vs 查服务端密钥/数据库），
// 原来 targetErr 被 `_` 吞掉，一律报「未绑定 portal 网页凭据」。
func TestVLESSDeliveryErrorDistinguishesCredentialFailure(t *testing.T) {
	noBasic := map[string]any{"installed": true, "active": true, "configured": true,
		"domain": "enc.example.com", "port": float64(2088)}
	applyVLESSEncryptionCapabilityWithTarget(context.Background(), noBasic, nil, errPortalNoBasic)
	if reason, _ := noBasic["delivery_error"].(string); !strings.Contains(reason, "未绑定") {
		t.Fatalf("未绑凭据的措辞不对: %q", reason)
	}

	broken := map[string]any{"installed": true, "active": true, "configured": true,
		"domain": "enc.example.com", "port": float64(2088)}
	applyVLESSEncryptionCapabilityWithTarget(context.Background(), broken, nil, errors.New("凭据解密失败"))
	if reason, _ := broken["delivery_error"].(string); !strings.Contains(reason, "凭据解密失败") {
		t.Fatalf("凭据读取失败被说成未绑定: %q", reason)
	}
}

// 2026-10-25 五轮排查第四轮：派生名（-Reality / -VMess / -VLESS-Enc）
// 也必须参与去重。
//
// 原来只对主名去重：一台叫「东京」的节点，它的 Reality 派生名
// 「东京-Reality」可能正好等于另一台节点的主名 ⇒ 客户端里两个代理同名，
// Clash 会静默丢掉其中一个，用户只看到少了一台。
func TestDerivedNamesAreDeduped(t *testing.T) {
	nodes := dedupeNodeNames([]subscriptionNode{
		{Name: "东京", Host: "192.0.2.1", Port: 1, AccountUserID: "sub_a",
			Reality: &nodeReality{PublicKey: "p", ShortID: "s", DestSNI: "d", Port: 443}},
		{Name: "东京-Reality", Host: "192.0.2.2", Port: 2, AccountUserID: "sub_b"},
	})
	body, _, err := renderSubscription(nodes, subscriptionFormatClash)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, p := range doc.Proxies {
		name, _ := p["name"].(string)
		count[name]++
	}
	for name, n := range count {
		if n > 1 {
			t.Errorf("客户端里出现重名代理 %q ×%d（Clash 会静默丢弃一个）", name, n)
		}
	}
	// 正常节点（无冲突）的名字不能被动过 —— 去重只应在冲突时生效。
	plain := dedupeNodeNames([]subscriptionNode{
		{Name: "首尔", Host: "192.0.2.3", Port: 3},
		{Name: "大阪", Host: "192.0.2.4", Port: 4},
	})
	if plain[0].Name != "首尔" || plain[1].Name != "大阪" {
		t.Fatalf("无冲突的名字被改动了: %q %q", plain[0].Name, plain[1].Name)
	}
}

// 2026-10-25 五轮排查第四轮：单节点探测预算必须覆盖「一次慢 capabilities
// + 一次 3s qrencode」的最坏路径。
//
// 原来是 5s（按一节点两次请求估的），而现代节点还要读 reality-status 与
// vlessenc-status，后者在节点侧会现跑 qrencode（上游 timeout=3s）。
// 5s 下这些节点会稳定地拿不到备用通道。
func TestSubscriptionProbeTimeoutCoversEncryptionStatus(t *testing.T) {
	// 节点侧 qrencode 的上游上限是 3s；capabilities 与状态读取各留一次往返。
	const qrencodeBudget = 3 * time.Second
	if subscriptionNodeProbeTimeout < 2*time.Second+qrencodeBudget {
		t.Fatalf("单节点预算 %v 不足以覆盖 capabilities + qrencode(3s)",
			subscriptionNodeProbeTimeout)
	}
	// 但也不能无限放大：买家同步等待要靠它兜底。
	if subscriptionNodeProbeTimeout > 20*time.Second {
		t.Fatalf("单节点预算 %v 过大，冷启动会拖垮买家请求", subscriptionNodeProbeTimeout)
	}
}

// 2026-10-25 五轮排查第四轮：能力聚合的总预算必须覆盖所有批次。
//
// 原来写死 2*provisionTimeout(30s)，而单节点现在最坏要三次往返
// （capabilities + reality-status + vlessenc-status，后者含 3s qrencode）。
// 节点一多，最后一整批必然被截断 —— 表现为「最后几台总是能力读取失败」。
func TestCapabilitiesTotalBudgetCoversAllBatches(t *testing.T) {
	if capabilitiesNodeTimeout <= provisionTimeout {
		t.Fatalf("单节点预算 %v 必须大于建账号的 %v（多了两次状态读取）",
			capabilitiesNodeTimeout, provisionTimeout)
	}
	batches := (capabilitiesMaxNodesPerFetch + provisioningConcurrency - 1) / provisioningConcurrency
	if got := capabilitiesTotalBudget(); got < time.Duration(batches)*capabilitiesNodeTimeout {
		t.Fatalf("总预算 %v 不足以覆盖 %d 批 × %v", got, batches, capabilitiesNodeTimeout)
	}
	if capabilitiesMaxNodesPerFetch < 13 {
		t.Fatal("节点上限不能小于当前生产规模")
	}
}

// 2026-10-25 五轮排查第五轮：三个格式渲染器共用同一套「发不发」判据。
//
// 原来三处各写一遍 if，唯一的格式差异（sing-box 不支持 VLESS Encryption
// 的 encryption 字段）散落在注释里 —— 新增格式或通道时必然漂移。
func TestSubscriptionChannelMatrix(t *testing.T) {
	cases := []struct {
		format, channel string
		want            bool
	}{
		{subscriptionFormatURI, channelReality, true},
		{subscriptionFormatURI, channelVMess, true},
		{subscriptionFormatURI, channelVLESSEnc, true},
		{subscriptionFormatClash, channelReality, true},
		{subscriptionFormatClash, channelVMess, true},
		{subscriptionFormatClash, channelVLESSEnc, true},
		{subscriptionFormatSingBox, channelReality, true},
		{subscriptionFormatSingBox, channelVMess, true},
		// 官方 sing-box 无 encryption 字段：塞进去会被静默忽略 ⇒ 连不上。
		{subscriptionFormatSingBox, channelVLESSEnc, false},
	}
	for _, c := range cases {
		if got := subscriptionEmitsChannel(c.format, c.channel); got != c.want {
			t.Errorf("%s/%s = %v, want %v", c.format, c.channel, got, c.want)
		}
	}
}

// 2026-10-30：备用交付域名去重必须归一化主域名。
//
// 原来 seen 里放的是 strings.ToLower(primary)，而候选值经过
// normalizeDeliveryBase（补 https://、去尾斜杠、小写 host）。
// primary 带尾斜杠或没写协议时两者对不上，同一个域名会被当成"备用线路"
// 再列一次 —— 客户看到两条一模一样的链接。
func TestBackupDeliveryBasesDedupesNormalizedPrimary(t *testing.T) {
	cases := []struct{ primary, env string }{
		{"https://sub.example.com/", "sub.example.com"},
		{"sub.example.com", "https://sub.example.com/"},
		{"https://Sub.Example.com", "https://sub.example.com"},
	}
	for _, c := range cases {
		t.Setenv("SUBSCRIPTION_BACKUP_BASE_URLS", c.env)
		got := backupDeliveryBases(c.primary)
		if len(got) != 0 {
			t.Errorf("primary=%q env=%q → 仍列出重复域名 %v", c.primary, c.env, got)
		}
	}
	// 不同域名必须保留，且按 normalize 后的形式输出。
	t.Setenv("SUBSCRIPTION_BACKUP_BASE_URLS", "https://backup.example.com/, https://other.example.com")
	got := backupDeliveryBases("https://sub.example.com")
	if len(got) != 2 || got[0] != "https://backup.example.com" {
		t.Fatalf("备用域名解析错误: %v", got)
	}
}

// 2026-10-30：Clash 分组里 proxies 类型异常时不得把该组清空。
//
// withInfoInSelect 原来用 `members, _ := g["proxies"].([]string)` 吞掉 ok，
// 断言失败会得到 nil ⇒ 组里只剩占位信息条目。Clash 对异常组会拒绝整份配置，
// 比"少一个组"严重得多（买家整份订阅导入失败）。
func TestWithInfoInSelectKeepsGroupWhenProxiesShapeChanges(t *testing.T) {
	groups := []map[string]any{
		{"name": groupSelect, "type": "select", "proxies": []any{"东京", "大阪"}},
	}
	out := withInfoInSelect(groups, []string{"🔄 连不上？先更新订阅"})
	if len(out) != 1 {
		t.Fatalf("组数量变了: %d", len(out))
	}
	members, ok := out[0]["proxies"].([]any)
	if !ok || len(members) != 2 {
		t.Fatalf("类型异常时不应改写该组，实际: %#v", out[0]["proxies"])
	}
}

// 2026-10-30：登录封禁表必须能回收键，否则公开端点被持续扫描时内存单调增长。
//
// 原实现只在单个 IP 内部裁剪时间戳，fails / banned 的**键永不删除**。
// 2026-10-29 那轮爆破用了约 1250 个 IP，之后还有日常扫描 —— 每见一个源 IP
// 就永久占一个 map 条目。
func TestLoginGuardSweepsExpiredEntries(t *testing.T) {
	now := time.Now()
	g := &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}

	// 造 200 个"很久以前失败过、再也没回来"的 IP，外加一个刚失败的。
	for i := 0; i < 200; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i)
		g.fails[ip] = []time.Time{now.Add(-2 * loginFailWindow)}
	}
	g.banned["203.0.113.9"] = now.Add(-time.Minute) // 早已解封
	g.banned["203.0.113.10"] = now.Add(time.Hour)   // 仍在封禁

	// 触发一次清理（新 IP 的一次失败）。
	g.fail("192.0.2.7", now)

	if len(g.fails) != 1 {
		t.Fatalf("过期失败记录未被回收：fails 仍有 %d 个键", len(g.fails))
	}
	if _, ok := g.fails["192.0.2.7"]; !ok {
		t.Fatal("当前 IP 的记录被误删")
	}
	if _, ok := g.banned["203.0.113.9"]; ok {
		t.Fatal("已解封的 IP 仍留在 banned")
	}
	if _, ok := g.banned["203.0.113.10"]; !ok {
		t.Fatal("仍在封禁期内的 IP 被误删")
	}
}

// 2026-10-30：限流器的全量清理必须限频。
//
// 原来只要 len(attempts) > 阈值，**每次请求**都在持锁状态下扫全表。
// 分布式爆破正好会把 map 推到阈值以上 ⇒ 所有登录请求被串行化并做 O(n) 扫描，
// 防爆破的机制反过来放大了攻击。现在 5 秒最多清一次；超 4 倍阈值立即清。
func TestSlidingLimiterSweepsAtMostOncePerInterval(t *testing.T) {
	l := newSlidingLimiter(time.Minute, 100)
	// 造出超过阈值的历史主体（都早已过期）。
	stale := time.Now().Add(-2 * time.Minute)
	for i := 0; i < limiterSweepThreshold+10; i++ {
		l.attempts[fmt.Sprintf("subject-%d", i)] = []time.Time{stale}
	}
	// 第一次调用会清理（超过阈值且从未清理过）。
	if !l.allow("first") {
		t.Fatal("首次调用不应被限流")
	}
	if len(l.attempts) != 1 {
		t.Fatalf("首次调用应清掉过期主体，实际剩 %d", len(l.attempts))
	}
	// 再灌满到阈值以上，但刚刚清理过 —— 不应立刻再清（清理被限频）。
	for i := 0; i < limiterSweepThreshold+10; i++ {
		l.attempts[fmt.Sprintf("again-%d", i)] = []time.Time{stale}
	}
	before := len(l.attempts)
	_ = l.allow("second")
	if len(l.attempts) < before-1 {
		t.Fatalf("5 秒内不应再次全量清理：清理前 %d，清理后 %d", before, len(l.attempts))
	}
	// 但内存爆涨到 4 倍阈值时必须马上清，不能等 5 秒。
	//
	// 2026-11-06：这个分支改为「最多每秒一次」而不是「每个请求都清」。随机 key 洪水（公开订阅端点用
	// sub:<token> 作主体，随便发不存在的 token 即可）会让 map 始终高于 4 倍阈值，而记录都在窗口内、
	// sweep 删不掉 —— 原来的「立即清」退化成每请求一次 O(n) 全扫并持锁，防滥用变成 DoS 放大器。
	l.lastSweep = time.Now().Add(-2 * time.Second)
	for i := 0; i < limiterSweepThreshold*4; i++ {
		l.attempts[fmt.Sprintf("flood-%d", i)] = []time.Time{stale}
	}
	_ = l.allow("third")
	if len(l.attempts) > limiterSweepThreshold {
		t.Fatalf("超过 4 倍阈值应马上清理，实际剩 %d", len(l.attempts))
	}
}

// 2026-11-06：随机 subject 洪水（记录都新鲜、sweep 删不掉）不能演变成「每个请求全表扫描」。
func TestSlidingLimiterSweepRateLimitedUnderFlood(t *testing.T) {
	l := newSlidingLimiter(time.Minute, 100)
	live := time.Now()
	for i := 0; i < limiterSweepThreshold*5; i++ {
		l.attempts[fmt.Sprintf("attacker-%d", i)] = []time.Time{live}
	}
	// 同一秒内打 200 个新 subject（模拟每请求一个新随机 token）。
	for i := 0; i < 200; i++ {
		_ = l.allow(fmt.Sprintf("random-%d", i))
	}
	if l.sweepRuns > 1 {
		t.Fatalf("洪水期间每秒最多清理一次，实际 %d 次", l.sweepRuns)
	}
	// 内存硬上限：达到 65536 个 key 时整表清空，不无限增长。
	l2 := newSlidingLimiter(time.Minute, 100)
	for i := 0; i < limiterHardResetLimit; i++ {
		l2.attempts[fmt.Sprintf("flood-%d", i)] = []time.Time{live}
	}
	l2.lastSweep = time.Now()
	_ = l2.allow("one-more")
	if len(l2.attempts) > 2 {
		t.Fatalf("达到内存硬上限时应整表清空，实际剩 %d", len(l2.attempts))
	}
}

// 2026-10-30：交付域名的自检必须认 JSON 字段，不能只搜子串 "ok"。
//
// 这一步决定「发给买家的订阅链接指向的到底是不是本面板」。原来用
// strings.Contains(body, "ok")：任何含 "ok" 的页面（book / cookie / 停靠页）
// 都能通过校验，于是错误域名会被写进交付链接 —— 买家拿到一批打不开的地址。
func TestCheckDeliveryBaseRequiresPanelHealthShape(t *testing.T) {
	cases := []struct {
		name, body string
		wantOK     bool
	}{
		{"本面板健康响应", `{"status":"ok","time":"2026-10-30T00:00:00Z"}`, true},
		{"含 ok 子串但不是面板", `<!doctype html><title>booking</title><p>cookie</p>`, false},
		{"JSON 但状态不对", `{"status":"degraded"}`, false},
		{"不是 JSON", `ok`, false},
		{"空响应", ``, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/health" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(c.body))
		}))
		err := checkDeliveryBase(context.Background(), srv.URL)
		srv.Close()
		if c.wantOK && err != nil {
			t.Errorf("%s：应当通过，却报错 %v", c.name, err)
		}
		if !c.wantOK && err == nil {
			t.Errorf("%s：应当拒绝，却通过了", c.name)
		}
	}
}

// 2026-10-31：错误原因必须按**字符**截断，不能按字节。
//
// queuePendingNodeAction / bumpPendingNodeAction / recordRevokeBackoff 原来用
// `reason[:300]`。这些 reason 里几乎一定含中文（"节点门户报错: ..."），
// 按字节切很可能落在三字节汉字的中间 ⇒ 非法 UTF-8 ⇒ Postgres 报
// invalid byte sequence for encoding "UTF8" 并拒绝整条写入。
// 对 queuePendingNodeAction 而言，写入失败意味着这条订阅的补做动作
// 再也不会被重试 —— 退款/停用后账号永久留在节点上。
func TestTruncateRunesKeepsValidUTF8ForChineseReason(t *testing.T) {
	// 构造一个"第 300 字节正好落在汉字中间"的字符串：前缀 1 字节 ASCII
	// 之后全是 3 字节汉字 ⇒ 300-1=299，299 % 3 = 2 ⇒ 会切断。
	reason := "x" + strings.Repeat("节点门户报错", 80)
	if len(reason) <= 300 {
		t.Fatal("用例构造错误：字符串应超过 300 字节")
	}
	got := truncateRunes(reason, 300)
	if !utf8.ValidString(got) {
		t.Fatalf("按字符截断后仍是非法 UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 300 {
		t.Fatalf("字符数 = %d, want 300", n)
	}
	// 反证：按字节切确实会产生非法 UTF-8（说明这个测试不是空转）。
	if utf8.ValidString(reason[:300]) {
		t.Fatal("本用例依赖的场景未复现：按字节截断居然仍是合法 UTF-8")
	}
}

// 源码级守卫：错误原因不得按字节截断。
//
// 这三处都写 DB（pending 待办 / 退避记录）。按字节切中文会产生非法 UTF-8，
// Postgres 拒绝整条写入 —— 表现是"待办没建起来"或"退避不生效"，
// 而且只在含中文的报错上触发，平时测不出来。
func TestReasonTruncationUsesRunesNotBytes(t *testing.T) {
	for _, file := range []string{"pending_node_actions.go", "provisioning.go"} {
		src := readSourceFile(t, file)
		if strings.Contains(src, "reason[:300]") {
			t.Errorf("%s 仍按字节截断 reason（[:300]）；中文被切断会产生非法 UTF-8，"+
				"Postgres 拒绝写入 ⇒ 待办/退避失效", file)
		}
		if !strings.Contains(src, "truncateRunes(reason, 300)") {
			t.Errorf("%s 必须用 truncateRunes(reason, 300)", file)
		}
	}
}

// 2026-10-31：没有节点 API 的节点不得进入商城。
//
// 买家交付必须先在节点上开专属账号；没有 api_key 就开不了。
// 一旦这种节点被标成 in_store：
//
//	needsAccountProvisioning 对每条订阅恒为 true → 每小时重排一次同步；
//	商城订单的 delivery_status 永远停在 pending，而节点页看起来一切正常。
//
// 两个入口都要挡住：更新时上架（admin.go）、新建时默认上架（server.go）。
func TestNodesWithoutAPICannotEnterStore(t *testing.T) {
	update := readSourceFile(t, "admin.go")
	if !strings.Contains(update, "hasNodeAPI") || !strings.Contains(update, "need_api") {
		t.Error("updateNode 必须拒绝把「没有节点 API」的节点上架（need_api 提示）")
	}
	create := readSourceFile(t, "server.go")
	if !strings.Contains(create, "storeAutoDisabled") {
		t.Error("createNode 必须把没有 api_url 的节点强制设为自用（storeAutoDisabled）")
	}
	// 手工节点（无 api_url）不允许以 in_store=true 落库。
	if !strings.Contains(create, "if apiURL == \"\" && inStore {") {
		t.Error("createNode 缺少「无 API ⇒ 不进商城」的判断")
	}
}

// 2026-10-31：改端口/换主机后必须清空连续失败计数，否则下一次失败
// 会立刻被判成 warning——而这时服务可能只是刚重启还没起来。
func TestStatusResetClearsProbeFailures(t *testing.T) {
	src := readSourceFile(t, "admin.go")
	if !strings.Contains(src, "consecutive_probe_failures=0") {
		t.Error("updateNode 重置状态时必须同时清零 consecutive_probe_failures")
	}
}

// 2026-10-31：API 响应默认必须不可缓存。
//
// 面板用 Bearer token 而不是 cookie，浏览器缓存按 URL 存 —— 同一台机器换人登录后，
// 上一任的 /api/nodes、/api/subscriptions 仍可能从磁盘缓存读到（无缓存头的 JSON
// 会被浏览器启发式缓存）。这些响应里有订阅链接、节点清单、审计与告警。
func TestWriteJSONSetsNoStore(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"ok": "1"})
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// 调用方显式设置时不应被覆盖。
	rec2 := httptest.NewRecorder()
	rec2.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(rec2, http.StatusOK, map[string]string{"ok": "1"})
	if got := rec2.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("显式缓存头被覆盖: %q", got)
	}
}

// 2026-10-31：不要"假装限流"——创建了信号量却只写 `_ = sem`。
// subIdsAggregate 原来就是这样：所有节点的 /users/list 同时打出去，
// 而注释写着"限流到 provisioningConcurrency"。
func TestNoFakeSemaphore(t *testing.T) {
	src := readSourceFile(t, "subids.go")
	// 只检查**代码行**：注释里提到这个写法是正常的（说明性文字），
	// 按整段文本搜会把"解释为什么不能这么写"的注释也判成违规。
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "_ = sem") {
			t.Errorf("subids.go 存在假限流: %s", trimmed)
		}
	}
	if !strings.Contains(src, "sem <- struct{}{}") {
		t.Error("subids.go 必须真的获取信号量槽位")
	}
}
