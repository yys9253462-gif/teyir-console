package httpapi

// 节点能力聚合（/api/nodes/capabilities）的守卫测试。
//
// 这一组钉三件事：
//   1. 路由必须在 /api/nodes/{id} 之前注册（占位符会吞掉 "capabilities"）
//   2. 必须区分「端点不存在（老门户）」与「取不到（网络/密钥）」
//      —— 两者处理动作完全不同：一个要升级节点，一个要查网络
//   3. 未绑 api_key 的节点不应被请求（必然失败的多余调用）

import (
	"os"
	"strings"
	"testing"
)

func TestCapabilitiesRouteOrderedBeforeNodeID(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(sourceFile(t, "server.go"))
	if err != nil {
		t.Fatalf("读取 server.go 失败：%v", err)
	}
	src := string(raw)

	capIdx := strings.Index(src, `"GET /api/nodes/capabilities"`)
	if capIdx < 0 {
		t.Fatal("找不到 capabilities 路由注册")
	}
	// 找任何 /api/nodes/{id} 形式的路由，确认都在 capabilities 之后。
	for _, pat := range []string{
		`"DELETE /api/nodes/{id}"`,
		`"PATCH /api/nodes/{id}"`,
		`"GET /api/nodes/{id}"`,
		`"PUT /api/nodes/{id}/categories"`,
	} {
		idx := strings.Index(src, pat)
		if idx < 0 {
			continue
		}
		if idx < capIdx {
			t.Errorf("路由 %s 注册在 capabilities 之前 —— "+
				"Go ServeMux 的 {id} 占位符会把 \"capabilities\" 当成节点 id", pat)
		}
	}
}

func TestCapabilitiesDistinguishesOutdatedFromUnreachable(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "capabilities.go", "func (s *Server) loadNodeCapabilities(")

	// 必须显式识别「门户没有这个端点」，而不是笼统的 unreachable。
	//
	// ⚠️ 这里断言的是**调用那个判据函数**，不是断言判据内部怎么写 ——
	// 判据本身由 TestIsPortalEndpointMissingMatchesRealWording 单独钉。
	if !strings.Contains(body, "isPortalEndpointMissing(") {
		t.Error("必须用专门的判据区分「门户过旧」与「网络不通」—— " +
			"两者处理动作完全不同（升级节点 vs 查网络）")
	}
	if !strings.Contains(body, "节点门户版本过旧") {
		t.Error("过旧节点的 reason 必须写清楚，前端要据此提示升级")
	}
	// Reachable 与 Supported 是两个维度，不能合并成一个 bool。
	if !strings.Contains(body, "Supported") {
		t.Error("需要 Supported 字段区分「端点不存在」与「取不到」")
	}
}

// 判据必须匹配 portal 的**实际措辞**。
//
// 背景（2026-10-04 真机实测）：第一版按 `strings.Contains(msg, "404")` 判断，
// 4 台老节点全被显示成「能力读取失败」（红色，像故障）——
// 因为老 portal 返回的是 {"ok":false,"error":"API endpoint not found"}，
// portalCall 把它包装成 "节点 API 报错: API endpoint not found"，
// **消息里根本没有状态码**。
func TestIsPortalEndpointMissingMatchesRealWording(t *testing.T) {
	t.Parallel()
	// 真机实测抓到的原文（逐字）
	real := "节点 API 报错: API endpoint not found"
	if !isPortalEndpointMissing(real) {
		t.Errorf("必须认得 portal 的真实措辞：%q", real)
	}
	// 状态码形态的兜底
	for _, s := range []string{"404", "节点 API 返回 404"} {
		if !isPortalEndpointMissing(s) {
			t.Errorf("应兼容状态码形态：%q", s)
		}
	}
	// 反向：这些**不能**被误判成"端点缺失"，否则网络故障会被建议去升级节点
	for _, s := range []string{
		"连接节点 API 失败: context deadline exceeded",
		"api_key 不被接受 (401)：确认该节点是用「商城集群 Agent 模式」安装的",
		"响应不是合法 JSON，确认这是 hysteria2-installer 的 portal",
		"节点 API 报错: User not found",
	} {
		if isPortalEndpointMissing(s) {
			t.Errorf("不能把 %q 判成端点缺失 —— 那是另一类故障", s)
		}
	}
}

func TestCapabilitiesSkipsNodesWithoutAPIKey(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "capabilities.go", "func (s *Server) loadNodeCapabilities(")
	// SQL 里必须带 api_key IS NOT NULL —— 没绑密钥的节点调了必然 401。
	if !strings.Contains(body, "api_key IS NOT NULL") {
		t.Error("只应请求绑了 api_key 的节点：没绑的调了必然 401，" +
			"白白产生失败日志")
	}
	if !strings.Contains(body, "deleted_at IS NULL") {
		t.Error("必须排除已删除节点")
	}
}

func TestCapabilitiesCachePropagatesInventoryReadFailures(t *testing.T) {
	body := funcBlock(t, "capabilities.go", "func (s *Server) cachedNodeCapabilities(")
	for _, want := range []string{
		"(nodeCapabilitiesResult, error)",
		"res, fetchErr := s.loadNodeCapabilities(fetchCtx)",
		"if fetchErr == nil",
		"return res, fetchErr",
		"case <-ctx.Done():\n\t\t\treturn nodeCapabilitiesResult{}, ctx.Err()",
		"lastErr = fetchErr",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capabilities cache must propagate DB read failures without caching partial results; missing %q", want)
		}
	}
}

func TestCapabilitiesDoesNotOmitNodesWithUnreadableCredentials(t *testing.T) {
	body := funcBlock(t, "capabilities.go", "func (s *Server) loadNodeCapabilities(")
	for _, want := range []string{
		"unavailable := make([]nodeCapabilities, 0)",
		"服务端未配置凭据解密器",
		"节点 API 密钥解密失败",
		"if err := rows.Err(); err != nil",
		"append(unavailable, out...)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capability aggregation must retain nodes whose credentials cannot be read; missing %q", want)
		}
	}
}

func TestCapabilitiesUsesBearerPortalCall(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "capabilities.go", "func (s *Server) loadNodeCapabilities(")
	if !strings.Contains(body, `"/capabilities"`) {
		t.Error("必须调用节点侧的 /capabilities 端点")
	}
	// 复用 portalCall 才能拿到与其它节点调用一致的超时/错误处理。
	if !strings.Contains(body, "portalCall(") {
		t.Error("应复用 portalCall，而不是自己发 HTTP 请求")
	}
	// 并发要有上限，否则节点多了会同时打满。
	if !strings.Contains(body, "provisioningConcurrency") {
		t.Error("并发拉取必须有上限（复用 provisioningConcurrency）")
	}
}
