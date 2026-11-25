package sshclient

// Run 的输出上限与取消路径（2026-10-30）。
//
// 两件事都是真机上"偶发但致命"的类型：
//
//  1. **输出无界**。Run 的 stdout 挂在 bytes.Buffer 上，而命令是运维脚本 ——
//     写错一条重定向就能让采集器内存跟着输出涨。采集器每 30 秒对每台机器
//     跑一遍，一台机器放出一份大输出就足以把进程推到 OOM，之后**所有**主机
//     停止采集：一台机器的错误变成全站故障。
//  2. **取消后立刻读缓冲**。发完 SIGKILL 就 `return stdout.String()`，而写
//     缓冲的是 ssh 会话的读循环 goroutine，它并没有停 —— 这是真实的数据竞争，
//     在 -race 下必报；同时 defer Close 与它并发操作同一连接会 panic，
//     日志里只留一条「采集主机时 panic」，看不出根因。

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCappedBufferKeepsPartialAndMarksTruncation(t *testing.T) {
	b := &cappedBuffer{limit: 8}
	if n, err := b.Write([]byte("0123456789ABC")); err != nil || n != 13 {
		t.Fatalf("超限写必须报告全部写入（否则上层会当成一次写失败）：n=%d err=%v", n, err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "01234567") {
		t.Fatalf("必须保留上限内的内容，实际 %q", out)
	}
	if !strings.Contains(out, "截断") {
		t.Fatalf("截断必须显式告知，否则上层会把它当作完整输出（%q）", out)
	}
	if len(out) > b.limit+64 {
		t.Fatalf("截断后仍保留过多内容：%d 字节", len(out))
	}
}

func TestCappedBufferConcurrentReadAndWrite(t *testing.T) {
	b := &cappedBuffer{limit: 128}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = b.Write([]byte("output"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = b.String()
		}
	}()
	wg.Wait()
}

func TestCappedBufferUnderLimitIsExact(t *testing.T) {
	b := &cappedBuffer{limit: 64}
	_, _ = b.Write([]byte("LISTEN_PORT=14617\nUSED_TOTAL=0\n"))
	if got := b.String(); got != "LISTEN_PORT=14617\nUSED_TOTAL=0\n" {
		t.Fatalf("未超限时不得追加任何标注，实际 %q", got)
	}
}

// 取消路径必须等写缓冲的 goroutine 退出后才读缓冲。
func TestRunWaitsForWriterBeforeReadingOnCancel(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("sshclient.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func Run(")
	if start < 0 {
		t.Fatal("找不到 Run")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("找不到 Run 的结尾")
	}
	body := text[start : start+end]
	if strings.Count(body, "<-done") < 2 {
		t.Error("取消路径必须等 session.Run 返回（同一个 done 通道要等两次）—— " +
			"否则读缓冲与写缓冲并发，是数据竞争且可能 panic")
	}
	if !strings.Contains(body, "maxCommandOutput") {
		t.Error("输出必须有上限 —— 一条失控的重定向会让采集器 OOM，" +
			"而 OOM 之后所有主机都停止采集")
	}
}

// 2026-11-03：换 IP 接口允许填 IPv6，而拨号原来用 "%s:%d" 拼地址，
// IPv6 得到 "2001:db8::1:22" —— Dial 直接报 too many colons，所有 SSH 采集/执行都失败。
func TestDialFormatsIPv6Address(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable")
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = Dial(ctx, "::1", port, "root", Credential{Type: "password", Secret: []byte("x")}, "")
	if err != nil && strings.Contains(err.Error(), "too many colons") {
		t.Fatalf("IPv6 host must be bracketed when dialing: %v", err)
	}
}
