package httpapi

// sub-ids 聚合端点的源码级守卫（2026-10-03 加端点时的同步检查项）。
//
// 设计要求：
//   1. 路由不在 /api/nodes/{id} 下 —— 避免 Go ServeMux 的 {id} 占位符
//      把 "sub-ids" 解释成某个节点的 id
//   2. handler 必须 enforceReadLimit（admin 调时）—— 暴露 portal 全集账号
//   3. 输出：by_user_id（map user_id → [{node_id, node_name}]）+ node_errors
//
// 这一组只检查源码是否包含关键模式 —— 是写端点时的「保命金」，避免
// 下一次重构把它写错。

import (
	"os"
	"strings"
	"testing"
)

func TestSubIdsEndpointRegisteredSafely(t *testing.T) {
	t.Parallel()
	// 路由注册在 server.go，不是 func 签名 —— 直接读文件 + 字符串检查。
	raw, err := os.ReadFile(sourceFile(t, "server.go"))
	if err != nil {
		t.Fatalf("读取 server.go 失败：%v", err)
	}
	serverBody := string(raw)

	// 1. 路由路径必须在 /api/nodes/{id} 之外。
	if strings.Contains(serverBody, `"GET /api/nodes/sub-ids"`) {
		t.Error("路由注册路径 /api/nodes/sub-ids 会被 ServeMux 解释为节点 id，应放到 /api/sub-ids")
	}
	if !strings.Contains(serverBody, `"GET /api/sub-ids"`) {
		t.Error("路由 GET /api/sub-ids 必须注册在 server.go")
	}

	// handler 实现文件
	bodySubids := funcBlock(t, "subids.go", "func (s *Server) subIdsAggregate(")
	// 2. 必须有 enforceReadLimit 守卫。
	if !strings.Contains(bodySubids, "enforceReadLimit") {
		t.Error("聚合端点必须 enforceReadLimit —— 暴露 hysteria portal 全集账号")
	}
	// 3. 必须返 by_user_id（map user_id → [{node_id, node_name, ...}]）
	if !strings.Contains(bodySubids, "by_user_id") {
		t.Error("响应体必须含 by_user_id 字段")
	}
	// 4. 必须只过滤 sub_* 账号（避免暴露 hy2_<uuid> 等其他类型）。
	if !strings.Contains(bodySubids, "HasPrefix") || !strings.Contains(bodySubids, `"sub_"`) {
		t.Error("聚合时只取 sub_* 账号（HasPrefix sub_），避免暴露其他类型")
	}
}
