package geo

// offline_prefix_test.go — 验证「网段级离线库」真的能识别网段里的陌生 IP。
//
// 为什么这一条必须单独测
// ----------------------
// 生成工具（tools/gen_offline_db.py）默认把单 IP 放宽成 /24。这个设计的
// 全部价值就是：**收录一台机器，就能认出同网段的邻居** —— 也就是"新买的
// 那台 VPS"。
//
// 但这件事在实现上有两个会静默失效的点：
//   1. Lookup 用的是 net.IPNet.Contains，如果网段解析时 strict 语义用错，
//      可能只匹配到网络地址本身，邻居 IP 全部落空（表现为"库有数据但没用"）
//   2. 最长前缀匹配若排序反了，粗粒度条目会盖掉精确条目
// 两者都不会报错，只会让识别率悄悄下降。所以用测试钉死。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOfflinePrefixMatchesNeighbourIPs(t *testing.T) {
	// 模拟生成工具的产出：一条 /24 覆盖整段。
	path := filepath.Join(t.TempDir(), "prefix.json")
	body := `{
	  "_meta": {"source": "prefix-test", "ipv4_prefix": 24},
	  "entries": [
	    {"network": "198.51.100.0/24", "region": "Tokyo", "provider": "AWS", "country": "JP", "asn": "AS16509"}
	  ]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时库失败: %v", err)
	}
	db, err := LoadOfflineFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	// 收录的是 .17，但要能认出同段的任何邻居 —— 这正是"新机器"的形态。
	neighbours := []string{
		"198.51.100.17",  // 被收录的那个
		"198.51.100.1",   // 段内第一个
		"198.51.100.254", // 段内最后一个
		"198.51.100.100", // 陌生邻居（模拟新买的机器）
	}
	for _, ip := range neighbours {
		info := db.Lookup(ip)
		if info == nil {
			t.Errorf("%s 应当命中 /24，实际落空", ip)
			continue
		}
		if info.Region != "Tokyo" || info.Provider != "AWS" {
			t.Errorf("%s 命中但结果不对: %+v", ip, info)
		}
	}

	// 段外地址绝不能命中（放太宽就是假阳性，比不识别更糟）。
	outside := []string{"198.51.101.1", "198.51.99.255", "198.52.100.1", "199.51.100.1"}
	for _, ip := range outside {
		if info := db.Lookup(ip); info != nil {
			t.Errorf("%s 在 /24 之外，不该命中，却得到 %+v", ip, info)
		}
	}
}

// 边界：网络地址与广播地址也要能命中（它们是合法输入，不该被特殊对待）。
func TestOfflinePrefixIncludesNetworkAndBroadcast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.json")
	body := `{"entries":[{"network":"10.20.30.0/24","region":"Edge","provider":"Test"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时库失败: %v", err)
	}
	db, _ := LoadOfflineFile(path)
	for _, ip := range []string{"10.20.30.0", "10.20.30.255"} {
		if db.Lookup(ip) == nil {
			t.Errorf("%s 应当命中网段边界", ip)
		}
	}
}

// IPv6 网段同样要工作（生成工具对 v6 默认放宽到 /48）。
func TestOfflinePrefixIPv6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.json")
	body := `{"entries":[{"network":"2606:4700::/32","region":"Anycast","provider":"Cloudflare"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时库失败: %v", err)
	}
	db, _ := LoadOfflineFile(path)
	if info := db.Lookup("2606:4700:4700::1111"); info == nil || info.Provider != "Cloudflare" {
		t.Fatalf("IPv6 网段应当命中，得到 %+v", info)
	}
	if info := db.Lookup("2607:1::1"); info != nil {
		t.Fatalf("段外 IPv6 不该命中，得到 %+v", info)
	}
}
