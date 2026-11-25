package httpapi

import (
	"strings"
	"testing"
)

// 节点分类的**核心不变量**（2026-10-03 管理员需求）。
//
// 需求：分类既是分组，也是上架的圈定范围 ——
// 商城商品选「AI 专用」，买家就只能拿到 AI 专用的节点。
// 这些断言锁住最容易在后续改动里走样的几条。

// 分类圈定必须作用在**唯一的出口**上，而不是各个调用点自己过滤。
//
// 理由：上次「交付页与实际下发不一致」就是因为两条路径各写一遍。
// 这次加分类如果再分散，就会重演。
func TestCategoryFilteringHappensAtOnePlace(t *testing.T) {
	t.Parallel()

	// 2026-11-06：实现移到 subscriptionNodesForChecked（多报告「暂时给不出节点」），subscriptionNodesFor 只包一层。
	body := funcBlock(t, "subscription.go", "func (s *Server) subscriptionNodesForChecked(")
	if !strings.Contains(body, "categoryScopeFor(") {
		t.Error("subscriptionNodesFor 必须解析分类圈定")
	}
	if !strings.Contains(body, "applySubscriptionAccounts(") {
		t.Error("subscriptionNodesFor 必须调用 applySubscriptionAccounts")
	}

	// 调用点不得自己调 categoryScopeFor —— 那意味着过滤逻辑又分散了。
	for _, f := range []struct{ file, marker string }{
		{"subscription.go", "func (s *Server) subscriptionFeed("},
		{"store.go", "func (s *Server) subscriptionView("},
	} {
		fn := funcBlock(t, f.file, f.marker)
		if strings.Contains(fn, "categoryScopeFor(") {
			t.Errorf("%s 不应自己解析分类范围 —— 必须走 subscriptionNodesFor，"+
				"否则过滤逻辑又分散到多处", f.marker)
		}
	}
}

// 分类被停用后，**已售出的订阅必须继续工作**。
//
// 停用的语义是「不再用于新订单」，不是「让老客户断线」。
// 实现上：loadCategoryNodeIDs 不按 enabled 过滤，
// 所以停用分类不会让订阅突然变空。
func TestDisabledCategoryStillServesExistingSubscriptions(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "categories.go", "func (s *Server) loadCategoryNodeIDs(")

	// 查询里**不得**出现 c.enabled 条件。
	if strings.Contains(body, "enabled") {
		t.Error("loadCategoryNodeIDs 不应按 enabled 过滤 —— " +
			"停用分类的语义是「不再用于新订单」，不是「让已售出的订阅断线」。\n" +
			"加了 enabled 条件会让停用分类的瞬间，所有该分类的老客户一起掉线")
	}
}

// 已删除的节点不得出现在分类范围里（否则订阅会下发一个已不存在的节点）。
func TestCategoryScopeExcludesDeletedNodes(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "categories.go", "func (s *Server) loadCategoryNodeIDs(")

	if !strings.Contains(body, "n.deleted_at IS NULL") {
		t.Error("分类节点集合必须排除已删除的节点 —— " +
			"否则订阅会试图下发一个已经不在的节点")
	}
}

// 删除分类时，若仍被节点或订阅引用，必须拒绝并说明原因。
//
// 硬删会让引用悬空：订阅的 category_id 指向不存在的分类，
// 之后解析范围会失败 —— 而失败路径是「空集合」，客户会突然拿到 0 个节点。
//
// ⚠️ 这里断言的是**判断表达式本身**，不只是"函数里出现过表名"。
// 第一版只检查 `strings.Contains(body, "node_category_members")` ——
// 把 `if nodeCount > 0 || subCount > 0` 改成 `if false` 之后断言照样通过，
// 也就是说那条断言**抓不到真正的破坏**。注入验证时发现的。
func TestDeletingReferencedCategoryIsRefused(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "categories.go", "func (s *Server) categoryDelete(")

	// 两个引用来源都查了
	for _, marker := range []string{"node_category_members", "subscription_tokens"} {
		if !strings.Contains(body, marker) {
			t.Errorf("删除分类前必须检查 %s 的引用", marker)
		}
	}
	// ★ 关键：那个守卫必须**真的把两个计数用起来**。
	// 只检查表名存在是不够的 —— 守卫被改成 if false 时表名仍然在。
	if !strings.Contains(body, "nodeCount > 0 || subCount > 0") {
		t.Error("必须用实际计数做判断（nodeCount > 0 || subCount > 0）—— " +
			"只查询不判断等于没检查：硬删会让订阅的 category_id 悬空，" +
			"之后解析范围失败、客户突然拿到 0 个节点")
	}
	if !strings.Contains(body, "StatusConflict") {
		t.Error("被引用时必须返回 409 并说明原因，而不是静默删除")
	}
	if !strings.Contains(body, "停用") {
		t.Error("错误信息应引导操作者改用「停用」—— 那才是被引用时的正确操作")
	}
}

// slug 必须与迁移 026 的 CHECK 约束一致。
//
// 两处不一致会出现「API 说可以、数据库拒绝」的 500：
// 用户看到的是"服务器错误"，而不是"这个 slug 不能用"。
func TestCategorySlugPatternMatchesMigration(t *testing.T) {
	t.Parallel()

	// 迁移里的 CHECK：^[a-z0-9][a-z0-9_-]{0,31}$
	if !categorySlugPattern.MatchString("ai") {
		t.Error("'ai' 应被接受")
	}
	if !categorySlugPattern.MatchString("ai-dedicated") {
		t.Error("'ai-dedicated' 应被接受")
	}
	if !categorySlugPattern.MatchString("a1_b2") {
		t.Error("'a1_b2' 应被接受")
	}
	for _, bad := range []string{"", "AI", "有中文", "has space", "-lead", "_lead", "a/b"} {
		if categorySlugPattern.MatchString(bad) {
			t.Errorf("%q 应被拒绝（与迁移 026 的 CHECK 一致）", bad)
		}
	}
	// 长度上限 32（首字符 + 最多 31）
	if !categorySlugPattern.MatchString("a" + strings.Repeat("b", 31)) {
		t.Error("32 位 slug 应被接受")
	}
	if categorySlugPattern.MatchString("a" + strings.Repeat("b", 32)) {
		t.Error("33 位 slug 应被拒绝")
	}
}

// 商城按 slug 下单时，**老商城不带这个字段**必须当作「不圈定」而不是报错。
//
// 否则商城一升级（或一个老版本的商城实例）就会全部下单失败。
func TestOrderWithoutCategoryMeansNoScope(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) storeCreateOrder(")

	// 必须能在 payload 里接收 node_category
	if !strings.Contains(body, "node_category") {
		t.Fatal("storeCreateOrder 必须接收 node_category —— " +
			"商城按分类圈定节点靠它")
	}
	// 且为空时不能报错
	if strings.Contains(body, `"error": "node_category`) {
		t.Error("node_category 为空时不应报错 —— 老商城不带这个字段，" +
			"报错会让它全部下单失败。空 = 不圈定（全部分类）")
	}
}

// 商城传的分类 slug 若不存在，必须明确告知 —— 不能静默按「全部分类」处理。
//
// 静默降级 = 买家本该只拿到 AI 专用节点，却拿到了全部节点。
func TestUnknownCategorySlugIsRejected(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) storeCreateOrder(")

	if !strings.Contains(body, "resolveCategoryBySlug(") {
		t.Error("必须按 slug 解析分类 id")
	}
	if !strings.Contains(body, "StatusBadRequest") {
		t.Error("slug 不存在时必须返回 400 并说明 —— " +
			"静默按「全部分类」处理会让买家拿到超出预期的节点")
	}
}

// 2026-11-06：端口跳跃区间要校验并按段渲染，脏值不能让整份 sing-box 配置加载失败。
func TestHopRangeNormalizedAndRenderedPerSegment(t *testing.T) {
	for raw, want := range map[string]string{
		"20000-40000":               "20000-40000",
		" 20000-30000, 40000-50000": "20000-30000,40000-50000",
		"20000":                     "20000-20000",
		"40000-20000":               "",
		"0-100":                     "",
		"abc":                       "",
		"20000-70000":               "",
	} {
		if got := normalizeHopRange(raw); got != want {
			t.Errorf("normalizeHopRange(%q) = %q, want %q", raw, got, want)
		}
	}
	out := singBoxOutbound(subscriptionNode{Name: "n", Host: "1.2.3.4", Port: 443, Password: "p", HopRange: "20000-30000,40000-50000"})
	ports, _ := out["server_ports"].([]string)
	if len(ports) != 3 || ports[1] != "20000:30000" || ports[2] != "40000:50000" {
		t.Fatalf("multi-segment hop ranges must render one sing-box entry per segment, got %v", out["server_ports"])
	}
}
