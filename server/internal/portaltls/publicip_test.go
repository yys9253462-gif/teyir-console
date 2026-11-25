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
		"::127.0.0.1":     false, // ::/96 已废弃 IPv4 兼容段
		"100::1":          false, // 100::/64 丢弃前缀
		"fec0::1":         false, // fec0::/10 废弃站点本地段
	} {
		if got := PublicIP(net.ParseIP(ip)); got != want {
			t.Errorf("PublicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
