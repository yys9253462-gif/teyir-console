package httpapi

import (
	"strings"
	"testing"
)

// 交付页显示的节点数必须等于买家实际拿到的节点数。
//
// 实测过的 BUG：交付页报 node_count=5，而订阅配置里只有 4 个 ——
// 因为交付页走的是 build.Nodes（全量共享构建），
// 而 /sub/{token} 走的是过滤后的集合。两条路径各算一遍必然漂移。
//
// ⚠️ 2026-10-03 演进：判据从「两条路径都直接调 applySubscriptionAccounts」
// 收敛成「两条路径都走 subscriptionNodesFor」——
// 后者是「这条订阅能看到哪些节点」的**唯一出口**，内部才调
// applySubscriptionAccounts 并把分类圈定一起算进去。
//
// 收敛的理由：这次加「分类圈定」时，如果还保持两个调用点各自组装参数，
// 就又要改两处、又可能漏一处。现在只有 subscriptionNodesFor 一处需要改。
//
// 断言随之更新 —— 这不是"迁就代码"，而是把检查点移到**新的唯一出口**上，
// 意图（两条路径同源）完全没变。

func TestDeliveryViewUsesSameNodeSetAsFeed(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) subscriptionView(")

	if !strings.Contains(body, "subscriptionNodesForChecked(") {
		t.Fatal("交付页必须走 subscriptionNodesFor —— 它是节点集合的唯一出口，" +
			"直接用 build.Nodes 会报出「全部在役节点」，而订阅里实际只给过滤后的，" +
			"买家看到 N 个、导入后 N-1 个")
	}
	// 不能又退回裸的 build.Nodes。
	if strings.Contains(body, "redactSubscriptionNodes(build.Nodes)") {
		t.Error("不得直接用 build.Nodes 渲染交付页 —— 那与下发路径不同源")
	}
}

// 两条路径必须都经过**同一个**出口。
func TestBothPathsUseTheSingleScopeExit(t *testing.T) {
	t.Parallel()

	feed := funcBlock(t, "subscription.go", "func (s *Server) subscriptionFeed(")
	if !strings.Contains(feed, "subscriptionNodesForChecked(") {
		t.Fatal("subscriptionFeed 必须走 subscriptionNodesFor")
	}

	view := funcBlock(t, "store.go", "func (s *Server) subscriptionView(")
	if !strings.Contains(view, "subscriptionNodesForChecked(") {
		t.Fatal("subscriptionView 必须走 subscriptionNodesFor —— " +
			"两条路径共用同一个判据，才不会再漂移")
	}
}

// 唯一的出口内部必须真的调用过滤函数，而不是自己另写一套。
func TestScopeExitCallsTheFilter(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "subscription.go", "func (s *Server) subscriptionNodesForChecked(")

	if !strings.Contains(body, "applySubscriptionAccounts(") {
		t.Error("subscriptionNodesFor 必须调用 applySubscriptionAccounts，不能另写一套过滤")
	}
	if !strings.Contains(body, "categoryScopeFor(") {
		t.Error("subscriptionNodesFor 必须解析分类圈定 —— " +
			"否则「商城选了 AI 专用」不会生效")
	}
}

// ⚠️ 分类圈定的三态语义（最易出错的地方）：
//
//	nil        → 不圈定（全部分类）—— 加分类功能之前的行为
//	空 map     → 圈定，但该分类下确实没有节点 ⇒ 订阅应当拿到 0 个节点
//	分类查不到 → 同样按空集合处理，**绝不放行全部**
//
// 「空集合」被误当成 nil 的后果是：配置写错（分类被删）时
// 反而把全部节点发给买家 —— 那是"配置错误导致多发"的最坏情况。
func TestCategoryScopeDistinguishesEmptyFromUnset(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "subscription.go", "func (s *Server) categoryScopeFor(")

	// 未指定分类时返回 nil（不圈定）
	if !strings.Contains(body, "return nil, nil") {
		t.Error("未指定分类时必须返回 nil —— 表示「不圈定」，与「空集合」含义不同")
	}
	// 出错时返回空 map（圈定但无节点），不能返回 nil
	if !strings.Contains(body, "return map[string]bool{}, subscriptionScopeUnavailable{err}") || strings.Contains(body, "return nil, err") {
		t.Error("分类解析失败时必须返回**空集合**而不是 nil —— " +
			"返回 nil 会被下游当作「不圈定」，于是把全部节点发给买家")
	}
}

// 过滤函数必须真的用上分类集合。
func TestFilterHonoursCategoryScope(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "subscription.go", "func applySubscriptionAccounts(")

	if !strings.Contains(body, "categoryNodeIDs[n.ID]") {
		t.Error("applySubscriptionAccounts 必须按 categoryNodeIDs 过滤节点")
	}
	// nil 表示不圈定 —— 这条判断必须有，否则不传分类的订阅会一个节点都拿不到。
	if !strings.Contains(body, "categoryNodeIDs != nil && !categoryNodeIDs[n.ID]") {
		t.Error("必须先判断 categoryNodeIDs 是否为 nil：" +
			"nil = 不圈定（全部分类），不判断会把老订阅全部过滤成空")
	}
}

// 2026-11-06：合并订阅页的「包含的节点」也要与 /sub 实际下发同源。
func TestMergedSubscriptionStateUsesTheScopeExit(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "subscription.go", "func (s *Server) subscriptionState(")
	if !strings.Contains(body, "s.subscriptionNodesFor(") || strings.Contains(body, "redactSubscriptionNodes(build.Nodes)") {
		t.Fatal("subscriptionState 的 included 必须走 subscriptionNodesFor，不能直接用全站构建")
	}
	preview := funcBlock(t, "subscription.go", "func (s *Server) subscriptionPreview(")
	if !strings.Contains(preview, "s.subscriptionNodesFor(") || strings.Contains(preview, "renderSubscription(build.Nodes") {
		t.Fatal("subscriptionPreview 必须渲染 /sub 实际下发的节点，不能直接用全站构建")
	}
}
