package portaltls

import (
	"net"
	"net/netip"
)

// specialUseBlocks 是 net.IP 的 IsPrivate / IsLoopback / IsLinkLocal* 没覆盖、
// 但同样不该被当作「节点公网地址」去连的段（2026-11-03 补）。
//
//   - 0.0.0.0/8：本网络保留段（RFC 1122/6890）；Go 对 0.1.2.3 的 IsGlobalUnicast 返回 true。
//   - 100.64.0.0/10：运营商级 NAT（CGNAT）；阿里云元数据 100.100.100.200 就在这里，Tailscale 也用它。
//   - 192.0.0.0/24：IETF 协议保留。
//   - 198.18.0.0/15：基准测试保留。
//   - 240.0.0.0/4：未来保留段（RFC 1112）；Go 对 240.0.0.1 的 IsGlobalUnicast 返回 true。
//   - ::/96：已废弃的 IPv4 兼容段（RFC 4291），避免 ::127.0.0.1 之类绕过回环检查。
//   - 64:ff9b::/96、64:ff9b:1::/48：NAT64 —— 64:ff9b::7f00:1 在 NAT64 网络里就是 127.0.0.1。
//   - 100::/64：丢弃专用前缀（RFC 6666）。
//   - fec0::/10：已废弃的站点本地单播段（RFC 3879）。
//   - ::ffff:0:0/96 已由 net.IP 归一成 IPv4 后判断，这里不重复。
var specialUseBlocks = func() []netip.Prefix {
	out := []netip.Prefix{}
	for _, s := range []string{
		"0.0.0.0/8",
		"100.64.0.0/10",
		"192.0.0.0/24",
		"198.18.0.0/15",
		"240.0.0.0/4",
		"::/96",
		"64:ff9b::/96",
		"64:ff9b:1::/48",
		"100::/64",
		"fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// PublicIP 判断一个解析结果能不能作为节点 / 外部服务的公网地址去连接。
// 所有对用户可填地址发起的出站连接都应该用它（与各处原有的 IsPrivate 等判断同口径，再加上 specialUseBlocks）。
func PublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range specialUseBlocks {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}
