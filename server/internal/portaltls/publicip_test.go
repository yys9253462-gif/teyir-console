package portaltls

import (
	"net"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8":         true,
		"2001:4860::8888": true,
		"10.0.0.1":        false,
		"127.0.0.1":       false,
		"169.254.169.254": false,
		"100.100.100.200": false, // 阿里云元数据（CGNAT 段）
		"100.64.0.1":      false,
		"192.0.0.8":       false,
		"198.18.0.1":      false,
		"64:ff9b::7f00:1": false, // NAT64 映射的 127.0.0.1
		"::ffff:10.0.0.1": false,
		"fc00::1":         false,
		"0.0.0.0":         false,
		"0.1.2.3":         false, // 0.0.0.0/8 本网络保留段
		"240.0.0.1":       false, // 240.0.0.0/4 保留地址段
		"::127.0.0.1":        false, // ::/96 已废弃 IPv4 兼容段
		"::ffff:0:127.0.0.1": false, // ::ffff:0:0:0/96 IPv4 转换前缀（RFC 8215）
		"100::1":             false, // 100::/64 丢弃前缀
		"2001::1":              false, // 2001::/32 Teredo 隧道段
		"2001:10::1":           false, // ORCHID 不可路由前缀
		"2001:20::1":           false, // ORCHIDv2 不可路由前缀
		"2002:7f00:1::1":       false, // 6to4 内嵌回环 127.0.0.1
		"2002:a9fe:a9fe::1":    false, // 6to4 内嵌云元数据 169.254.169.254
		"fec0::1":              false, // fec0::/10 废弃站点本地段
	} {
		if got := PublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("PublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
