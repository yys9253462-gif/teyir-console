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

// maxDialCandidates 限制单次拨号最多尝试的去重候选地址数，防止异常海量 DNS 解析记录线性放大总超时。
const maxDialCandidates = 8

// DialCheckedIPs 按 IPv4 优先顺序依次拨号已经全部通过安全校验的地址列表。
// 当调用方指定了明确地址族（如 tcp4/tcp6）时仅尝试匹配地址族，避免用异族地址拨号覆盖真实连接错误。
// 任一地址建连失败且上下文未结束时自动回退到下一个已校验地址，全部失败则返回最后一次错误。
func DialCheckedIPs(ctx context.Context, dialer *net.Dialer, network, port string, ips []net.IPAddr) (net.Conn, error) {
	ordered := filterByNetwork(OrderDialIPs(ips), network)
	if len(ordered) == 0 {
		return nil, errors.New("没有可拨号的已校验地址")
	}
	if len(ordered) > maxDialCandidates {
		ordered = ordered[:maxDialCandidates]
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

func filterByNetwork(ordered []net.IP, network string) []net.IP {
	wantV4 := network == "tcp4" || network == "udp4" || network == "ip4"
	wantV6 := network == "tcp6" || network == "udp6" || network == "ip6"
	if !wantV4 && !wantV6 {
		return ordered
	}
	out := make([]net.IP, 0, len(ordered))
	for _, ip := range ordered {
		isV4 := ip.To4() != nil
		if (wantV4 && isV4) || (wantV6 && !isV4) {
			out = append(out, ip)
		}
	}
	return out
}
