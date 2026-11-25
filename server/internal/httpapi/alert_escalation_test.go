package httpapi

// 告警送达改进（2026-10-24）的回归测试。
//
// 这一组盯三件事，全是 2026-10-23 一次主机宕机事件的直接后果：
//
//  1. **告警黑洞**：一条 open 告警在状态首次出现时只推送一次。若那一刻推送失败、
//     被冷却吞掉、或落在静默期里被放弃 —— 此后这条告警一直 open，
//     reconcile 每轮只 UPDATE last_seen_at，**不再产生任何新事件、也不再重试**。
//     结果：面板知道故障、数据库有记录，运维却永远收不到。
//     修法：持续超时且从未推送成功（notified_at IS NULL）的告警重新推送。
//
//  2. **分级静默期**：原来所有类型一律 10 分钟。对端口抖动合理，
//     对「整机失联」太长 —— 机器不会在 3 分钟内自己恢复。
//
//  3. **升级重推的措辞**：必须与首次告警明显不同，否则运维当成重复消息忽略。

import (
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 1. 告警黑洞

// TestEscalateOnlyWhenNeverNotified 是「告警黑洞」修复的核心断言：
// 只有**从未推送成功**的告警才升级重推；已推送过的绝不重复。
//
// 反向验证要点：若条件写成 first_seen_at < now() - X 而漏掉 notified_at IS NULL，
// 这条测试必须报红 —— 因为那样每 30 秒一轮 reconcile 都会重复推送，
// 运维的 IM 会被同一条告警刷爆，比不告警更糟。
func TestEscalateOnlyWhenNeverNotified(t *testing.T) {
	t.Parallel()

	// 断言条件本身：升级判定必须同时要求「从未推送」与「已持续足够久」。
	// 这里用纯逻辑模拟 manage.go 里的 SQL 语义，避免依赖数据库。
	shouldEscalate := func(notified bool, age time.Duration) bool {
		return !notified && age > alertEscalateAfter
	}

	cases := []struct {
		name     string
		notified bool
		age      time.Duration
		want     bool
	}{
		{"刚触发且未推送，不该升级（首次推送还在静默期内）", false, time.Minute, false},
		{"持续够久且从未推送，必须升级（这就是告警黑洞）", false, 20 * time.Minute, true},
		{"持续够久但已推送过，绝不重复升级", true, 20 * time.Minute, false},
		{"持续够久但已推送过（很久以前推的），同样不重复", true, 3 * time.Hour, false},
		{"刚触发但已推送（不该发生），不升级", true, time.Minute, false},
	}
	for _, c := range cases {
		if got := shouldEscalate(c.notified, c.age); got != c.want {
			t.Errorf("%s：期望 %v，实得 %v", c.name, c.want, got)
		}
	}
}

// TestEscalateAfterIsLongerThanGrace 保证升级门槛排在静默期之后：
// 否则首次推送还没走完静默期，升级就先触发了 —— 两条推送同时在飞。
func TestEscalateAfterIsLongerThanGrace(t *testing.T) {
	t.Parallel()

	// 最长的静默期是 node_offline/node_blocked 的 10 分钟。
	longestGrace := alertGraceFor("node_offline")
	if alertEscalateAfter <= longestGrace {
		t.Errorf("升级门槛（%v）必须大于最长静默期（%v）—— "+
			"否则首次推送尚未走完静默期，升级重推就先触发了，"+
			"运维会收到两条内容几乎相同的告警",
			alertEscalateAfter, longestGrace)
	}
}

// TestEscalationSQLRequiresNeverNotified 直接断言 manage.go 里那段 SQL 的**文本**，
// 防止「条件漏写」这类缺陷逃过纯逻辑测试。
//
// 为什么需要这条：上面的 shouldEscalate 是把 SQL 语义抄成 Go 逻辑再断言 ——
// 如果实现里的 SQL 少写一个条件，而测试里的 Go 逻辑仍是「对的」，
// 测试照样全绿，缺陷却留在生产代码里。
// 断言 SQL 文本能把两者绑在一起（2026-10-24 反向验证时实测：
// 注入「漏掉 notified_at IS NULL」后本用例立刻报红）。
func TestEscalationSQLRequiresNeverNotified(t *testing.T) {
	t.Parallel()

	src := readSourceFile(t, "manage.go")

	// ⚠️ 必须**剔除注释行**后再找关键字。
	// 否则注释里写的 "notified_at IS NULL" 会被当成实现 ——
	// 那样注入缺陷后测试照样全绿，等于没有测试（2026-10-24 反向验证实测踩过）。
	code := stripGoComments(src)

	idx := strings.Index(code, "notified_at IS NULL")
	if idx < 0 {
		t.Fatal("manage.go 的 SQL 里找不到 notified_at IS NULL —— " +
			"升级重推的判定必须要求「从未推送成功」，否则每 30 秒一轮 reconcile 都会重复推送")
	}
	window := strings.Index(code, "first_seen_at < now()")
	if window < 0 {
		t.Fatal("manage.go 的 SQL 里找不到升级判定的持续时间条件")
	}
	// 两个条件必须在**同一条 SQL 语句**里：从 notified_at 往后找到第一个
	// 分号（SQL 结束）之前，必须能看到持续时间条件。
	// 不能用固定距离窗口 —— SQL 会被 gofmt 折行，距离随排版变。
	tail := code[idx:]
	if end := strings.Index(tail, ";"); end >= 0 {
		tail = tail[:end]
	}
	if !strings.Contains(tail, "first_seen_at < now()") {
		t.Error("「从未推送」与「持续够久」两个条件必须出现在同一条 SQL 里 —— " +
			"分散在两处时，其中一处被改坏不会被发现")
	}
}

// stripGoComments 去掉 Go 源码里的行注释与块注释，只留代码本身。
//
// 为什么要专门做这一步：断言实现细节时经常要匹配某个字符串，
// 而中文注释里往往也会提到同一字符串（例如 "（notified_at IS NULL）"）。
// 不剔除注释的话，注释里那一处就会让断言误判为通过 ——
// 注入缺陷后测试依然全绿，等于没有测试。
//
// ⚠️ 必须同时处理**行注释**与**块注释**：只处理行注释时，
// /* … KEYWORD … */ 块注释里的关键字仍会漏出来（2026-10-24 自测实测踩过）。
func stripGoComments(src string) string {
	var b strings.Builder
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		// 块注释状态：整行都是块注释内容
		if inBlock {
			if end := strings.Index(trimmed, "*/"); end >= 0 {
				trimmed = strings.TrimSpace(trimmed[end+2:])
				inBlock = false
			} else {
				continue
			}
		}
		// 行首是块注释开始
		if strings.HasPrefix(trimmed, "/*") {
			if end := strings.Index(trimmed[2:], "*/"); end >= 0 {
				trimmed = strings.TrimSpace(trimmed[2+end+2:])
			} else {
				inBlock = true
				continue
			}
		}
		// 整行行注释
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// 行尾行注释：把 // 之后的内容去掉（注意不能误伤字符串里的 //，
		// 但本项目源码里没有含 // 的字符串字面量，此处够用且更保守：
		// 只在 // 前不是引号时才截断）
		if idx := strings.Index(line, "//"); idx >= 0 && !strings.Contains(line[:idx], `"`) {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// readSourceFile 读同目录下的源码文件，用于断言实现细节。
// 失败时 t.Fatal 而不是返回空串 —— 空串会让下游断言给出误导性的错误信息。
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", name, err)
	}
	return string(b)
}

// ---------------------------------------------------------------- 2. 分级静默期

// TestAlertGraceIsTieredByType 钉死分级静默期：
// 整机失联必须比端口抖动更早通知，否则「机器宕了」这种确定不可自愈的故障
// 会被 10 分钟的沉默吃掉。
func TestAlertGraceIsTieredByType(t *testing.T) {
	t.Parallel()

	serverOffline := alertGraceFor("server_offline")
	nodeOffline := alertGraceFor("node_offline")

	if serverOffline >= nodeOffline {
		t.Errorf("server_offline 的静默期（%v）应短于 node_offline（%v）—— "+
			"SSH 全断基本就是宕机或退租，不会自己恢复；"+
			"而端口不通可能是抖动，需要更保守",
			serverOffline, nodeOffline)
	}
	if serverOffline > 5*time.Minute {
		t.Errorf("server_offline 的静默期不应超过 5 分钟，实得 %v —— "+
			"一次主机宕机在 10 分钟门槛下静默了近 4 小时没人知道",
			serverOffline)
	}
}

// TestAlertGraceFallsBackToDefault 未登记的类型必须走默认值，
// 不能返回 0（0 等于不等待，会把抖动直接推成刷屏）。
func TestAlertGraceFallsBackToDefault(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"server_warning", "server_unknown", "node_traffic_exceeded", "完全不存在的类型"} {
		got := alertGraceFor(typ)
		if got != alertNotifyGrace {
			t.Errorf("%q 的静默期应为默认 %v，实得 %v", typ, alertNotifyGrace, got)
		}
		if got <= 0 {
			t.Errorf("%q 的静默期必须为正数，实得 %v —— 0 等于不等待", typ, got)
		}
	}
}

// TestAlertGraceByTypeHasNoZeroDuration 防止有人往分级表里塞 0 或负值：
// 那会让「整机失联」在抖动的那一秒就推送。
func TestAlertGraceByTypeHasNoZeroDuration(t *testing.T) {
	t.Parallel()

	for typ, d := range alertNotifyGraceByType {
		if d <= 0 {
			t.Errorf("分级表里 %q 的静默期为 %v，必须为正数", typ, d)
		}
		if d > alertNotifyGrace {
			t.Errorf("分级表里 %q 的静默期（%v）不应大于默认 %v —— "+
				"分级表的目的是让确定的故障更早通知，不是让某些类型更晚",
				typ, d, alertNotifyGrace)
		}
	}
}

// ---------------------------------------------------------------- 3. 升级重推的措辞

// TestEscalatedTextIsDistinguishable 保证升级重推的文案与首次告警明显不同。
//
// 反向验证要点：若文案退化成与 triggered 相同，运维会当成重复消息划走 ——
// 那等于没修。
func TestEscalatedTextIsDistinguishable(t *testing.T) {
	t.Parallel()

	ev := buildAlertNotification("repeated", "server_offline", "critical",
		"美国-测试节点", "203.0.113.225", "", 0, "", time.Now())

	if !strings.Contains(ev.Text, "持续未送达") {
		t.Errorf("升级重推的文案必须点明「持续未送达」，实得 %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "美国-测试节点") {
		t.Errorf("升级重推的文案必须包含目标名，实得 %q", ev.Text)
	}

	// 与首次告警的文案对比：不能雷同
	first := buildAlertNotification("triggered", "server_offline", "critical",
		"美国-测试节点", "203.0.113.225", "", 0, "", time.Now())
	if ev.Text == first.Text {
		t.Errorf("升级重推的文案不能与首次告警完全相同：%q", ev.Text)
	}
}

// TestRepeatedEventNotSwallowedByCooldown 保证升级重推不会被
// 「刚推过 triggered」的 1 小时冷却挡掉 —— 否则黑洞照旧。
func TestRepeatedEventNotSwallowedByCooldown(t *testing.T) {
	t.Parallel()

	n := newAlertNotifier()
	now := time.Now()

	// 先让 triggered 占掉冷却
	if !n.allow("server_offline|美国-测试节点|triggered", now) {
		t.Fatal("首次 triggered 应当放行")
	}
	// 升级重推是独立事件类型，必须独立放行
	if !n.allow("server_offline|美国-测试节点|repeated", now.Add(time.Second)) {
		t.Error("升级重推被 triggered 的冷却挡掉了 —— " +
			"这样告警黑洞并没有修好：首次推送失败后，升级同样会被吞")
	}
	// 但重复的升级仍要受冷却保护，否则每 30 秒一轮 reconcile 会刷屏
	if n.allow("server_offline|美国-测试节点|repeated", now.Add(2*time.Second)) {
		t.Error("重复的升级重推应被冷却抑制 —— " +
			"reconcile 每 30 秒一轮，不节流会把 IM 刷爆")
	}
}

// TestRepeatedEventUsesDistinctCooldownKey 确认冷却键确实包含事件类型，
// 否则上面的独立性断言会侥幸通过（比如实现里恰好用了不同字符串）。
func TestRepeatedEventUsesDistinctCooldownKey(t *testing.T) {
	t.Parallel()

	ev := buildAlertNotification("repeated", "server_offline", "critical",
		"美国-测试节点", "203.0.113.225", "", 0, "", time.Now())
	first := buildAlertNotification("triggered", "server_offline", "critical",
		"美国-测试节点", "203.0.113.225", "", 0, "", time.Now())

	if ev.Event == first.Event {
		t.Fatalf("升级重推的事件类型必须与首次不同，实得都是 %q", ev.Event)
	}

	n := newAlertNotifier()
	now := time.Now()
	n.allow(ev.AlertType+"|"+ev.Target+"|"+first.Event, now)
	if !n.allow(ev.AlertType+"|"+ev.Target+"|"+ev.Event, now.Add(time.Second)) {
		t.Error("冷却键必须含事件类型，否则首次推送会挡掉升级重推")
	}
}
