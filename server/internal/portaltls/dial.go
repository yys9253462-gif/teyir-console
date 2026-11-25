package portaltls

import (
	"context"
	"errors"
	"net"
)

// OrderDialIPs 对已通过安全校验的解析结果去重并按 IPv4 优先、IPv6 其次的顺序排列，
// 保持各自原始相对顺序，避免双栈域名 AAAA 排在首位时在纯 IPv4 环境直接拨号失败。
func OrderDialIPs(ips []net.IPAddr) []net.IP {
	v4 := make([]net.IP, 0, len(ips))
	v6 := make([]net.IP, 0, len(ips))
	seen := make(map[string]struct{}, len(ips))
	for _, entry := range ips {
		if entry.IP == nil {
			continue
		}
		if ip4 := entry.IP.To4(); ip4 != nil {
			key := ip4.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			v4 = append(v4, ip4)
			continue
		}
		key := entry.IP.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		v6 = append(v6, entry.IP)
	}
	return append(v4, v6...)
}

// DialCheckedIPs 按 IPv4 优先顺序依次拨号已经全部通过安全校验的地址列表。
// 任一地址建连失败且上下文未结束时自动回退到下一个已校验地址，全部失败则返回最后一次错误。
func DialCheckedIPs(ctx context.Context, dialer *net.Dialer, network, port string, ips []net.IPAddr) (net.Conn, error) {
	ordered := OrderDialIPs(ips)
	if len(ordered) == 0 {
		return nil, errors.New("没有可拨号的已校验地址")
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	var lastErr error
	for _, ip := range ordered {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
