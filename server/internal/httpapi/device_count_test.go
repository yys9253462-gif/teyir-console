package httpapi

import (
	"encoding/json"
	"testing"
	"time"
)

// 「设备数」必须只统计**真实客户端地址**（2026-10-04）。
//
// 背景：请求不经过最外层 Caddy 覆盖时（容器内直连、docker 网桥转发），
// XFF 拿不到真实访问者，requestClientIP 回落到 RemoteAddr = 网桥地址，
// 于是 172.19.0.1 被当成「一台设备」记进 active_ips，设备数虚高。
// 实测：从 127.0.0.1:8081 拉一次订阅就多一个设备。
func TestIsPrivateOrLoopbackIP(t *testing.T) {
	// 必须判为「不是真实设备」的地址
	internal := []string{
		"172.19.0.1",   // docker 网桥网关（本次实测踩到的那个）
		"172.17.0.1",   // 默认 docker0
		"172.31.255.1", // RFC1918 上边界
		"10.0.0.5",
		"192.168.1.1",
		"127.0.0.1",
		"169.254.1.1", // 链路本地
		"::1",
		"fe80::1",
		"0.0.0.0",
		"    172.19.0.1    ", // 带空白也要识别
	}
	for _, ip := range internal {
		if !isPrivateOrLoopbackIP(ip) {
			t.Errorf("%q 应判为内部地址（不该算设备）", ip)
		}
	}

	// 必须**放行**的地址：真实客户可能就是这些
	public := []string{
		"198.51.100.10", // 本次实测的真实客户
		"198.51.100.20",
		"203.0.113.17",
		"8.8.8.8",
		"2001:4860:4860::8888",
		// ⚠️ CGNAT（运营商级 NAT）必须是公网：有些移动网络用户真从这里出来，
		// 过滤掉会把真人当空气。
		"100.64.0.1",
		// 解析失败的字符串一律保留 —— 宁可多算，也不能把真实用户抹掉
		"not-an-ip",
		"",
	}
	for _, ip := range public {
		if isPrivateOrLoopbackIP(ip) {
			t.Errorf("%q 应判为公网（必须计入设备）", ip)
		}
	}
}

// parseActiveIPs 必须同时做两件事：24h 窗口过滤 + 内部地址过滤。
func TestParseActiveIPsFiltersInternalAndExpired(t *testing.T) {
	now := time.Now()
	list := []subscriptionIP{
		{IP: "198.51.100.10", SeenAt: now.Add(-1 * time.Minute)},   // 保留
		{IP: "172.19.0.1", SeenAt: now.Add(-1 * time.Minute)},       // 内部 → 剔除
		{IP: "127.0.0.1", SeenAt: now.Add(-1 * time.Minute)},        // 回环 → 剔除
		{IP: "198.51.100.20", SeenAt: now.Add(-25 * time.Hour)},      // 过期 → 剔除
		{IP: "8.8.8.8", SeenAt: now.Add(-subscriptionIPWindow / 2)}, // 保留
	}
	raw, _ := json.Marshal(list)
	got := parseActiveIPs(raw, now)

	gotIPs := map[string]bool{}
	for _, e := range got {
		gotIPs[e.IP] = true
	}
	if len(got) != 2 {
		t.Fatalf("应保留 2 条，得到 %d 条: %+v", len(got), got)
	}
	if !gotIPs["198.51.100.10"] || !gotIPs["8.8.8.8"] {
		t.Fatalf("真实地址被误删: %+v", gotIPs)
	}
	for _, bad := range []string{"172.19.0.1", "127.0.0.1", "198.51.100.20"} {
		if gotIPs[bad] {
			t.Fatalf("%s 不该被保留", bad)
		}
	}
}

// 空/坏数据不应 panic。
func TestParseActiveIPsHandlesGarbage(t *testing.T) {
	if got := parseActiveIPs(nil, time.Now()); got != nil {
		t.Fatalf("nil 应返回 nil，得到 %+v", got)
	}
	if got := parseActiveIPs([]byte("not json"), time.Now()); got != nil {
		t.Fatalf("坏 JSON 应返回 nil，得到 %+v", got)
	}
	raw, _ := json.Marshal([]subscriptionIP{{IP: "", SeenAt: time.Now()}})
	if got := parseActiveIPs(raw, time.Now()); len(got) != 0 {
		t.Fatalf("空 IP 应被剔除，得到 %+v", got)
	}
}
