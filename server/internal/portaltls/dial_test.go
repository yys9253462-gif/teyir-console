package portaltls

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestOrderDialIPsPrefersIPv4AndDeduplicates(t *testing.T) {
	in := []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("198.51.100.2")},
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("198.51.100.3")},
		{IP: net.ParseIP("198.51.100.2")},
		{IP: net.ParseIP("2001:db8::2")},
	}
	got := OrderDialIPs(in)
	want := []string{"198.51.100.2", "198.51.100.3", "2001:db8::1", "2001:db8::2"}
	if len(got) != len(want) {
		t.Fatalf("排序结果数量不符：得到 %d，期望 %d（%v）", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].String() != w {
			t.Fatalf("第 %d 项不符：得到 %s，期望 %s", i, got[i].String(), w)
		}
	}
}

func TestDialCheckedIPsFallsBackToReachableAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地回环失败: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("拆分端口失败: %v", err)
	}
	// 先放一个同端口未监听的回环 IPv6 地址，再放可达的 IPv4 地址，验证优先拨通且具备回退能力。
	ips := []net.IPAddr{
		{IP: net.ParseIP("::1")},
		{IP: net.ParseIP("127.0.0.1")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := DialCheckedIPs(ctx, &net.Dialer{Timeout: time.Second}, "tcp", port, ips)
	if err != nil {
		t.Fatalf("多地址拨号应当回退并连上监听地址，实际失败: %v", err)
	}
	_ = conn.Close()
}

func TestDialCheckedIPsRejectsEmptyList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := DialCheckedIPs(ctx, &net.Dialer{Timeout: time.Second}, "tcp", "443", nil); err == nil {
		t.Fatal("空地址列表应当返回错误")
	}
}

// 验证显式指定 tcp4/tcp6 时只拨对应地址族，避免异族拨号把真实连接错误覆盖成协议不匹配。
func TestDialCheckedIPsFiltersByNetworkFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	onlyV6 := []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}}
	if _, err := DialCheckedIPs(ctx, &net.Dialer{Timeout: 200 * time.Millisecond}, "tcp4", "443", onlyV6); err == nil {
		t.Fatal("tcp4 下仅有 IPv6 候选时应直接报错")
	}
	onlyV4 := []net.IPAddr{{IP: net.ParseIP("198.51.100.1")}}
	if _, err := DialCheckedIPs(ctx, &net.Dialer{Timeout: 200 * time.Millisecond}, "tcp6", "443", onlyV4); err == nil {
		t.Fatal("tcp6 下仅有 IPv4 候选时应直接报错")
	}
}
