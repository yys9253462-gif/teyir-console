package collector

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// 空节点主机不应调用SSH或数据库，也不应读取会清零的流量接口。
// 故意不给client/db：一旦漏掉提前返回，这个回归用例会失败。
func TestCollectUsageSkipsUnmanagedHost(t *testing.T) {
	for _, nodes := range [][]nodeRow{nil, {}} {
		c := &Collector{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		c.collectUsage(context.Background(), nil, target{
			name:        "主机指标专用",
			trafficURL:  "http://127.0.0.1:1",
			trafficFile: "/must-not-read",
		}, nodes)
	}
}

// 提前返回只适用于未纳管主机；已有节点的真实端口错配仍须可检测。
func TestRegisteredNodePortMismatchRemainsDetectable(t *testing.T) {
	nodes := []nodeRow{{id: "registered", port: 15825}}
	if hasNodePort(nodes, 36890) {
		t.Fatal("不应将不同端口视为已登记节点")
	}
	if !hasNodePort(nodes, 15825) {
		t.Fatal("已登记的正确端口应被识别")
	}
}
