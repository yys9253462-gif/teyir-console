package httpapi

// preview_suggested_name_test.go — 钉死「从 API 导入节点时，suggested_name
// 必须按 国家-城市 格式给出」这一行为。
//
// 历史沿革：
//   - 2026-10-02 管理员截图发现 API 导入与手动添加命名不一致，统一成
//     「城市-商家-端口」；
//   - 2026-10-09 管理员再次要求：改成「国家-城市」（如「日本-东京」），
//     名字里不再出现商家，也不再带端口后缀。同城市多节点靠域名/IP/商城
//     标签区分，这是明确接受的权衡。
//
// 测试策略：previewNodeAPI 用真实门户 HTTP 跑会被 safeDialContext 拒绝 127.0.0.1
// —— 测试里没法绕（也不能绕：那正是生产防护）。所以这里只测对应的纯函数
// suggestedNodeName，它把 applyGeo 与 joinGeoName 串起来，并负责兜底回退。
//
// 测试里传入的 Info Region 字段已是「中文」（模拟 Resolver.Lookup 出口
// 已翻译的形态），而不是源语种「Tokyo」。这是函数契约的一部分。

import (
	"testing"

	"nodeops/server/internal/geo"
)

// 识别成功 → "国家-城市"。Region 已是中文（Resolver 出口已翻译），
// Country 是 ISO 码（Name 拼接时翻成中文），Provider 不进名字。
func TestSuggestedNodeName_FullMatch(t *testing.T) {
	got := suggestedNodeName("node1.example.com", "203.0.113.10", 15825,
		geo.Info{Region: "东京", Provider: "AWS", Country: "JP"})
	want := "日本-东京"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 识别空（公网 API 失败 / 离线库未命中）→ 退回 server_name（不再拼端口）。
//
// 这是行为不阻断主流程的兜底 —— 识别是加分项，不能让操作者加不上节点。
func TestSuggestedNodeName_FallbackToServerName(t *testing.T) {
	got := suggestedNodeName("node1.example.com", "198.51.100.5", 15825, geo.Info{})
	want := "node1.example.com"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 只识别到 region（country 缺失）→ 名字只有城市，不出现孤儿连字符。
func TestSuggestedNodeName_OnlyRegion(t *testing.T) {
	got := suggestedNodeName("node1.example.com", "203.0.113.20", 15825,
		geo.Info{Region: "东京"})
	want := "东京"
	if got != want {
		t.Fatalf("got %q, want %q（无 country 时不出现孤儿连字符）", got, want)
	}
}

// 只识别到 country → 名字只有国家。
func TestSuggestedNodeName_OnlyCountry(t *testing.T) {
	got := suggestedNodeName("node1.example.com", "203.0.113.30", 15825,
		geo.Info{Country: "US"})
	want := "美国"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 只识别到 provider → 商家不进名字，名字算不出来 → 退回 server_name。
func TestSuggestedNodeName_OnlyProvider(t *testing.T) {
	got := suggestedNodeName("node1.example.com", "203.0.113.30", 15825,
		geo.Info{Provider: "Hetzner"})
	want := "node1.example.com"
	if got != want {
		t.Fatalf("got %q, want %q（商家不再进名字）", got, want)
	}
}

// server_name 空、识别空 → 退回 public_ip。
func TestSuggestedNodeName_FallbackToPublicIP(t *testing.T) {
	got := suggestedNodeName("", "198.51.100.5", 15825, geo.Info{})
	want := "198.51.100.5"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 端口不再进名字（2026-10-09 规则变更的反向钉死）：
// 不同端口的同机识别结果必须完全一致。
func TestSuggestedNodeName_PortNotInName(t *testing.T) {
	for _, port := range []int{15825, 443, 65535} {
		got := suggestedNodeName("", "", port,
			geo.Info{Region: "东京", Country: "JP"})
		if got != "日本-东京" {
			t.Errorf("port=%d: got %q, want %q（端口不该出现在名字里）", port, got, "日本-东京")
		}
	}
}
