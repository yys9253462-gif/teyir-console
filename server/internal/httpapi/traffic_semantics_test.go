package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readRepoFile 读仓库里任意相对路径的文件（用于跨语言断言前端源码）。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", rel))
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", rel, err)
	}
	return string(raw)
}

// 24h 流量的口径测试（2026-10-02 修 BUG）。
//
// 现象：节点监控显示阿什本 11.88 TB、东京阿里云 538 GB，而真实值分别是
// 91 MB、157 MB —— 差 13 万倍。
//
// 根因：traffic_samples 有两种 source，语义**完全不同**，而原实现无差别 SUM：
//
//	portal_used      —— portal.json 的 used_bytes 是**累计计数器**，
//	                    每轮覆盖写同一个分钟桶。24h 用量 = max - min。
//	                    SUM 它 = 把 322 个几乎相同的累计值相加 = 那个 11.88 TB。
//	hysteria_traffic —— /traffic?clear=true 每轮清空，每次读到的是
//	                    "上轮以来的字节数"，是真增量。24h 用量 = SUM（正确）。
//
// 为什么久未被发现：哥伦布那台只有 hysteria_traffic，用 SUM 恰好正确 ——
// **列表里有一台显示正常，看着就不像坏了**。
//
// 这类 BUG 极容易在后续重构里被重新引入（"求和"看起来太自然了），
// 所以这里用源码断言钉死口径。

func TestTrafficSumSeparatesSourceSemantics(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring(")

	// 必须按 source 分别处理。
	if !strings.Contains(body, "p.source = 'portal_used'") {
		t.Fatal("24h 流量必须区分 portal_used —— 它是累计计数器，" +
			"直接 SUM 会把 322 个累计值相加（实测放大 13 万倍）")
	}
	if !strings.Contains(body, "ts.source = 'hysteria_traffic'") {
		t.Error("必须区分 hysteria_traffic —— 它是真增量，求和对")
	}
	// portal 口径必须是「相邻样本的正向增量之和」，不是 SUM 原值。
	// 2026-11-03 起不再用 max-min：删用户会让累计和下降，max-min 会把下降算成流量。
	if !strings.Contains(body, "SUM(GREATEST(d.delta, 0))") ||
		!strings.Contains(body, "p.tx_bytes - LAG(p.tx_bytes) OVER (PARTITION BY p.node_id ORDER BY p.sampled_at)") {
		t.Error("portal_used 的 24h 用量必须逐节点累加正向增量；" +
			"SUM 原值就是 2026-10-02 的 bug，max-min 会把删用户算成流量")
	}
}

// 保证没有人把裸的 SUM 又加回来 —— 那正是原 BUG 的形态。
func TestTrafficSumDoesNotSumPortalCounters(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring(")

	// 允许 `SUM(ts.tx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic')`，
	// 但**不允许**任何不带 source 限定的 SUM(ts.tx_bytes)。
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "SUM(ts.tx_bytes)") && !strings.Contains(trimmed, "SUM(ts.rx_bytes)") {
			continue
		}
		if !strings.Contains(trimmed, "FILTER") {
			t.Errorf("出现了不带 source 限定的求和：%q\n"+
				"traffic_samples 混合了累计值（portal_used）与增量（hysteria_traffic），"+
				"无差别 SUM 会把累计值反复相加 —— 这正是 2026-10-02 那个 11.88 TB 的成因", trimmed)
		}
	}
}

// 两种 source 同时存在时，hysteria_traffic 优先（它分 tx/rx，是真实上下行）。
func TestHysteriaTrafficPreferredWhenBothPresent(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring(")

	hysteriaAt := strings.Index(body, "SUM(ts.tx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic')")
	portalAt := strings.Index(body, "SUM(GREATEST(d.delta, 0))")
	if hysteriaAt < 0 || portalAt < 0 {
		t.Fatal("两种 source 的口径都必须出现在查询里")
	}
	// COALESCE 的参数顺序即优先级：hysteria 必须在前。
	if !(hysteriaAt < portalAt) {
		t.Error("COALESCE 里 hysteria_traffic 必须排在 portal_used 之前 —— " +
			"前者分 tx/rx、是真增量，后者只有出站累计值，应仅作兜底")
	}
}

// 入站为 0 时，前端要说明原因，而不是显示一个光秃秃的「0 B 入站」。
//
// portal_used 口径**没有入站数据**（那张表里 rx_bytes 恒为 0）。
// 用户看到「出站 91 MB / 入站 0 B」会以为节点坏了，实际只是没开 trafficStats。
func TestFrontendExplainsZeroInbound(t *testing.T) {
	t.Parallel()
	// 前端不在 Go 包内，这里读的是仓库里的 tsx 源码。
	// 路径相对 httpapi 包：../../../src/components/NodeMonitoringPage.tsx
	src := readRepoFile(t, "../../../src/components/NodeMonitoringPage.tsx")

	if !strings.Contains(src, "仅记账出站，无入站") {
		t.Error("入站为 0 时必须说明「仅记账出站，无入站」—— " +
			"portal_used 口径没有入站数据，光显示 0 B 会让人以为节点坏了")
	}
}
