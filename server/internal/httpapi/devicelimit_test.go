package httpapi

import (
	"testing"
	"time"
)

// 先上线的先占名额：新来的网络不能把已在用的挤掉；同时上线时按 IP 稳定排序。
func TestDeviceLimitVerdictFirstComeFirstServed(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	first := map[string]time.Time{
		"1.1.1.1": base, "2.2.2.2": base.Add(time.Minute), "3.3.3.3": base.Add(2 * time.Minute),
		"9.9.9.9": base.Add(3 * time.Minute),
	}
	online := []deviceLimitOnline{{IP: "9.9.9.9"}, {IP: "3.3.3.3"}, {IP: "1.1.1.1"}, {IP: "2.2.2.2"}}
	allowed, extra := deviceLimitVerdict(online, first, 3)
	if len(allowed) != 3 || allowed[0] != "1.1.1.1" || allowed[2] != "3.3.3.3" {
		t.Fatalf("allowed=%v", allowed)
	}
	if len(extra) != 1 || extra[0] != "9.9.9.9" {
		t.Fatalf("最后上线的才是超额: %v", extra)
	}
	if _, extra := deviceLimitVerdict(online, first, 0); len(extra) != 0 {
		t.Fatal("ip_limit=0 表示不限")
	}
	same := map[string]time.Time{"b": base, "a": base, "c": base}
	_, extra = deviceLimitVerdict([]deviceLimitOnline{{IP: "c"}, {IP: "b"}, {IP: "a"}}, same, 2)
	if len(extra) != 1 || extra[0] != "c" {
		t.Fatalf("同时上线时结果要稳定: %v", extra)
	}
}

// 2026-11-06：名额按「网络」算，不按精确 IP —— 手机换地址（4G/Wi-Fi 切换、重连拨号）后
// 同一个网络不该占第二个名额，否则 ip_limit=1 的客户只有一台设备也会被拦。
func TestDeviceLimitVerdictCountsNetworksNotIPs(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	// 同一 /24 里的两个地址（换 IP 的典型形态）+ 另一个网络。
	first := map[string]time.Time{
		"203.0.113.7": base, "203.0.113.9": base.Add(10 * time.Second), "198.51.100.5": base.Add(time.Minute),
	}
	online := []deviceLimitOnline{{IP: "203.0.113.7"}, {IP: "203.0.113.9"}, {IP: "198.51.100.5"}}
	allowed, extra := deviceLimitVerdict(online, first, 1)
	if len(allowed) != 2 || len(extra) != 1 || extra[0] != "198.51.100.5" {
		t.Fatalf("one /24 must occupy a single slot: allowed=%v extra=%v", allowed, extra)
	}
	// IPv6 按 /64 归并。
	v6 := map[string]time.Time{"2001:db8:1:2::1": base, "2001:db8:1:2:ffff::9": base.Add(time.Second), "2001:db8:1:3::1": base.Add(2 * time.Second)}
	allowed6, extra6 := deviceLimitVerdict([]deviceLimitOnline{{IP: "2001:db8:1:2::1"}, {IP: "2001:db8:1:2:ffff::9"}, {IP: "2001:db8:1:3::1"}}, v6, 1)
	if len(allowed6) != 2 || len(extra6) != 1 || extra6[0] != "2001:db8:1:3::1" {
		t.Fatalf("one /64 must occupy a single slot: allowed=%v extra=%v", allowed6, extra6)
	}
}
