package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 「节点不稳定」这条链路的测试（2026-10-02 接通）。
//
// 修之前的状态：前端有「不稳定」文案与指标卡、manage.go 有 node_unstable
// 告警规则，**但后端从来没有任何代码路径写 hysteria_nodes.status='warning'** ——
// 一条永远不会触发的死规则，而没有任何地方会告诉用户这一点。
//
// 这里用「读源码断言」钉住三件容易在重构中丢掉的事：
//  1. 成功探测必须把连续失败计数归零（否则计数只增不减，所有节点迟早"不稳定"）
//  2. 失败探测必须让计数自增（否则永远到不了阈值）
//  3. 判定 warning 时**不能覆盖 offline**（那会把 critical 降级成 warning）
//
// 为什么不做真实数据库测试：Collector.db 是具体的 *pgxpool.Pool，
// 没有可注入的接口。为这一个改动引入 sqlmock 属于超出范围的重构。
// 端到端验证在真机完成（构造连续失败，看状态是否变 warning）。
func sourceOf(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", name, err)
	}
	return string(raw)
}

func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("找不到 %s", sig)
	}
	open := strings.Index(src[start:], "{")
	if open < 0 {
		t.Fatalf("%s 没有函数体", sig)
	}
	depth, i := 0, start+open
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("%s 函数体未闭合", sig)
	return ""
}

// 成功路径必须归零。少了它，计数只增不减 —— 每个节点跑够轮数就会
// 被判成"不稳定"，整个 warning 状态变成噪音。
func TestSuccessfulProbeResetsFailureCounter(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "collector.go")
	body := funcBody(t, src, "func (c *Collector) updateNodeStatus(")

	if !strings.Contains(body, "probeOK") {
		t.Fatal("updateNodeStatus 必须接收 probeOK —— 才能区分「探通了」与「没探通」")
	}
	if !strings.Contains(body, "consecutive_probe_failures = 0") {
		t.Error("成功探测必须把 consecutive_probe_failures 归零；" +
			"否则计数只增不减，所有节点迟早被误判成不稳定")
	}
	// 归零那一支必须真的挂在 probeOK 上，不能两边都归零。
	if !strings.Contains(body, "if probeOK {") {
		t.Error("归零逻辑必须由 probeOK 分支控制")
	}
}

// 失败路径必须自增且能到达阈值。
func TestFailedProbeIncrementsCounterAndCanReachWarning(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "collector.go")
	body := funcBody(t, src, "func (c *Collector) updateNodeStatus(")

	if !strings.Contains(body, "consecutive_probe_failures + 1") &&
		!strings.Contains(body, "consecutive_probe_failures = consecutive_probe_failures + 1") {
		t.Error("失败探测必须让计数自增，否则阈值永远达不到")
	}
	if !strings.Contains(body, "'warning'") {
		t.Error("失败路径必须能把状态置为 warning —— 这正是这次接通的东西")
	}
	// 阈值必须是个具名常量，而不是散落的魔法数。
	if !strings.Contains(src, "const unstableAfterFailures") {
		t.Error("阈值应当是具名常量，便于调整与解释")
	}
}

// ⚠️ 最关键的一条：判定 warning 时不能覆盖 offline。
//
// offline 是 critical 级（node_offline 规则），把它降级成 warning
// 属于"报警降级"—— 比不报更危险：用户看到黄色以为不严重，
// 实际节点已经挂了。
func TestWarningNeverDowngradesOffline(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "collector.go")

	// 两条失败路径都要有这个保护。
	for _, sig := range []string{
		"func (c *Collector) updateNodeStatus(",
		"func (c *Collector) markNodeProbeFailed(",
	} {
		body := funcBody(t, src, sig)
		if !strings.Contains(body, "WHEN status = 'offline' THEN 'offline'") &&
			!strings.Contains(body, "WHEN $1 = 'offline' THEN 'offline'") {
			t.Errorf("%s 里缺少「offline 不被降级」的保护 —— "+
				"那会把 critical 报警显示成 warning，比不报更危险", sig)
		}
	}
}

// SSH 路径的失败也必须计数。
//
// 这条是修的过程中发现的真漏洞：SSH 路径原来用**裸 SQL** 写状态，
// 绕过了 updateNodeStatus，于是有 SSH 凭据的节点永远不会被判定为不稳定 ——
// 而三台主力节点恰好都走 SSH。等于这个功能对最重要的节点不生效。
func TestSSHProbeFailureAlsoCounts(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "collector.go")

	if !strings.Contains(src, "func (c *Collector) markNodeProbeFailed(") {
		t.Fatal("SSH 探测失败必须有专门的记录函数 —— 否则它绕过了计数逻辑")
	}
	body := funcBody(t, src, "func (c *Collector) markNodeProbeFailed(")
	if !strings.Contains(body, "consecutive_probe_failures = consecutive_probe_failures + 1") {
		t.Error("markNodeProbeFailed 必须累计连续失败计数")
	}

	// 并且 probeNodes 里失败那一支必须调它，而不是继续用裸 SQL。
	probe := funcBody(t, src, "func (c *Collector) probeNodes(")
	if !strings.Contains(probe, "c.markNodeProbeFailed(") {
		t.Error("probeNodes 的失败分支必须调用 markNodeProbeFailed —— " +
			"用裸 SQL 会绕过计数，SSH 节点的失败被静默")
	}
}

// unknown（密钥解不开）不该被算成"不稳定"。
//
// 那类问题的处理动作完全不同：不稳定 → 查线路；
// 密钥坏了 → 换密钥。混在一起会把人引向错误方向。
func TestKeyDecryptFailureIsNotCountedAsUnstable(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "collector.go")

	idx := strings.Index(src, "API 密钥解密失败")
	if idx < 0 {
		t.Fatal("找不到密钥解密失败的分支")
	}
	// 该调用应当传 probeOK=true（我们明确得出了结论，只是结论是"没法测"）。
	tail := src[idx : idx+120]
	if !strings.Contains(tail, "true)") {
		t.Error("密钥解密失败应传 probeOK=true —— 它不是「探不通」，" +
			"算成不稳定会让人去查线路而不是换密钥")
	}
}

// 2026-11-03：API 探测失败分支原来按传入值 `$1 = 'offline'` 判断，
// 唯一调用方总传 offline ⇒ 一次超时即离线、warning 永远到不了。
// 必须按当前值判断，并且只有连续达到阈值才离线。
func TestAPIProbeFailureEscalatesThroughWarning(t *testing.T) {
	t.Parallel()
	body := funcBody(t, sourceOf(t, "collector.go"), "func (c *Collector) updateNodeStatus(")
	if strings.Contains(body, "WHEN $1 = 'offline' THEN 'offline'") {
		t.Fatal("失败分支不能按传入的 status 判断：调用方总传 offline，会跳过 warning")
	}
	keep := strings.Index(body, "WHEN status = 'offline' THEN 'offline'")
	off := strings.Index(body, "WHEN consecutive_probe_failures + 1 >= $5 THEN 'offline'")
	warn := strings.Index(body, "ELSE 'warning'")
	if keep < 0 || off < 0 || warn < 0 || !(keep < off && off < warn) {
		t.Fatal("expected: keep offline → offline at threshold → otherwise warning")
	}
}

// 2026-11-03：用量采集读 users/list 原来上限 1 MiB 且不检测截断。
func TestUsageReadDetectsOversizedResponse(t *testing.T) {
	t.Parallel()
	src := sourceOf(t, "usage.go")
	if strings.Contains(src, "io.LimitReader(resp.Body, 1<<20)") {
		t.Fatal("用量采集不能再用 1 MiB 上限：大节点的 users/list 会被截断成「非法 JSON」")
	}
	if !strings.Contains(src, "io.LimitReader(resp.Body, usageResponseLimit+1)") || !strings.Contains(src, "len(body) > usageResponseLimit") {
		t.Fatal("必须多读 1 字节并在超限时明确报错，而不是把截断的 JSON 交给解析器")
	}
	if usageResponseLimit < 8<<20 {
		t.Fatalf("上限需与 portalCall 一致（8 MiB），当前 %d", usageResponseLimit)
	}
}

// 2026-11-03：采集器三处读库循环原来 Scan 失败就 continue、也不查 rows.Err()，
// 读不全时拿半份清单写状态（或把主机写成 unknown）。
func TestCollectorRowLoopsFailClosed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, sig, scan string }{
		{"collector.go", "func (c *Collector) probeNodesViaAPI(", "rows.Scan(&n.id, &n.name, &n.apiURL, &n.apiKey)"},
		{"collector.go", "func (c *Collector) probeNodes(", "rows.Scan(&n.id, &n.port)"},
		{"usage.go", "func (c *Collector) collectSubscriptionUsage(", "rows.Scan(&subID, &accountsRaw, &apiURL, &apiKey, &nodeLabel, &rowNodeID)"},
	} {
		body := funcBody(t, sourceOf(t, c.file), c.sig)
		i := strings.Index(body, c.scan)
		if i < 0 {
			t.Fatalf("%s: scan not found", c.sig)
		}
		after := body[i:]
		next := strings.Index(after, "}")
		if strings.Contains(after[:next], "continue") {
			t.Errorf("%s: Scan 失败不能 continue", c.sig)
		}
		if !strings.Contains(after, "rows.Err(); err != nil") {
			t.Errorf("%s: 循环后必须检查 rows.Err()", c.sig)
		}
	}
}

// 2026-11-06：节点「没法测」（invalid：未绑 API、配置不对）不算探不通；
// 只有至少一个节点真的探不通，主机才写 offline，否则写 unknown 并说明原因。
func TestAPIProbeOnlyMarksHostOfflineWhenUnreachable(t *testing.T) {
	t.Parallel()
	body := funcBlockOf(t, "collector.go", "func (c *Collector) probeNodesViaAPI(")
	inv := strings.Index(body, `case result.ErrorKind == "invalid":`)
	if inv < 0 || !strings.Contains(body[inv:inv+600], `"unknown", "api", "invalid", result.Detail, true)`) {
		t.Fatal("invalid 必须记 unknown 且 probeOK=true，不能累计不稳定计数")
	}
	if !strings.Contains(body, "anyUnreachable = true") || !strings.Contains(body, "} else if !anyUnreachable {") {
		t.Fatal("主机只有在至少一个节点探不通时才能写 offline")
	}
}

func funcBlockOf(t *testing.T, file, sig string) string {
	t.Helper()
	src := sourceOf(t, file)
	at := strings.Index(src, sig)
	if at < 0 {
		t.Fatalf("%s: 找不到 %s", file, sig)
	}
	end := strings.Index(src[at:], "\n}\n")
	if end < 0 {
		return src[at:]
	}
	return src[at : at+end]
}

// 2026-11-06：SSH 连不上（非认证类）时，节点绑了 API 就改走 API 探测，不能只把主机写 offline、节点不动。
func TestSSHDialFailureFallsBackToAPIProbe(t *testing.T) {
	t.Parallel()
	body := funcBlockOf(t, "collector.go", "func (c *Collector) collectServer(")
	fallback := strings.Index(body, `"collector: SSH 连不上，改用 API 探测节点状态"`)
	offline := strings.Index(body, "UPDATE servers SET status='offline', latency_ms=NULL, status_error=$2")
	if fallback < 0 || offline < 0 || fallback > offline {
		t.Fatal("SSH 拨号失败时必须先尝试 API 探测，再考虑把主机写 offline")
	}
	if !strings.Contains(body[fallback:offline], "c.probeNodesViaAPI(hostCtx, t)") {
		t.Fatal("回退分支必须调用 probeNodesViaAPI")
	}
	// 2026-11-06：查不到节点 API 绑定（数据库错误）时不能走到写 offline 的分支。
	check := strings.Index(body, "hasAPI, apiErr := c.nodeHasAPI(hostCtx, t.id)")
	guard := strings.Index(body, "if apiErr != nil {")
	if check < 0 || guard < check || guard > offline || !strings.Contains(body[guard:offline], "return") {
		t.Fatal("nodeHasAPI 出错时必须在写 offline 之前返回")
	}
}

// 2026-11-06：API 探测路径不写 latency_ms / net_latency_ms（那是 SSH 握手与纯 TCP 建连的口径）。
func TestAPIProbeDoesNotOverwriteNetworkLatency(t *testing.T) {
	t.Parallel()
	body := funcBlockOf(t, "collector.go", "func (c *Collector) probeNodesViaAPI(")
	if strings.Contains(body, "net_latency_ms=") || strings.Contains(body, "latency_ms=$2") {
		t.Fatal("API 探测不能用 HTTPS 往返耗时覆盖 latency_ms / net_latency_ms")
	}
}
