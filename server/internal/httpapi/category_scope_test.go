package httpapi

import (
	"testing"
)

// 分类圈定的**行为验证**（纯函数，不需要数据库）。
//
// 这些用例直接构造节点列表与分类集合，验证 applySubscriptionAccounts
// 在各种组合下的输出 —— 上面的 categories_test.go 是源码级守卫，
// 这里是行为级验证，两者互补。
//
// 需求来源（2026-10-03 管理员）：
//   商城商品选「AI 专用」→ 买家只拿到 AI 专用的节点；
//   选「跨境专用」→ 只拿到跨境专用的节点。

func catFleet() []subscriptionNode {
	return []subscriptionNode{
		{ID: "node-ai-1", Name: "AI-1", InStore: true, Password: "SHARED"},
		{ID: "node-ai-2", Name: "AI-2", InStore: true, Password: "SHARED"},
		{ID: "node-cross-1", Name: "CROSS-1", InStore: true, Password: "SHARED"},
		{ID: "node-general-1", Name: "GEN-1", InStore: true, Password: "SHARED"},
		{ID: "node-ai-private", Name: "AI-PRIV", InStore: false, Password: "SHARED"},
	}
}

func catRec() *subscriptionRecord {
	return &subscriptionRecord{
		OrderID: "SHOP-1",
		nodeAccounts: map[string]subscriptionNodeAccount{
			"node-ai-1":       {UserID: "u1", Password: "P1"},
			"node-ai-2":       {UserID: "u2", Password: "P2"},
			"node-cross-1":    {UserID: "u3", Password: "P3"},
			"node-general-1":  {UserID: "u4", Password: "P4"},
			"node-ai-private": {UserID: "u5", Password: "P5"},
		},
	}
}

func idsOf(nodes []subscriptionNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func has(nodes []subscriptionNode, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

// 指定 AI 分类 → 只拿到 AI 专用节点。
func TestCategoryScopeAiOnly(t *testing.T) {
	t.Parallel()
	scope := map[string]bool{"node-ai-1": true, "node-ai-2": true}
	got := applySubscriptionAccounts(catFleet(), catRec(), scope)

	if len(got) != 2 {
		t.Fatalf("AI 分类应只给 2 个节点，实际 %d 个: %v", len(got), idsOf(got))
	}
	if !has(got, "node-ai-1") || !has(got, "node-ai-2") {
		t.Errorf("应包含两个 AI 节点，实际 %v", idsOf(got))
	}
	if has(got, "node-cross-1") {
		t.Error("跨境节点不应出现在 AI 专用订阅里 —— 这正是分类要解决的问题")
	}
	if has(got, "node-general-1") {
		t.Error("普通节点不应出现在 AI 专用订阅里")
	}
	// 未上架节点即使属于该分类也不给买家（in_store 仍是独立一关）。
	if has(got, "node-ai-private") {
		t.Error("未上架商城的节点不应下发给买家，即使它属于所选分类")
	}
}

// 换个分类 → 拿到的是另一批节点（验证"选谁给谁"）。
func TestCategoryScopeCrossOnly(t *testing.T) {
	t.Parallel()
	scope := map[string]bool{"node-cross-1": true}
	got := applySubscriptionAccounts(catFleet(), catRec(), scope)

	if len(got) != 1 || got[0].ID != "node-cross-1" {
		t.Fatalf("跨境分类应只给 node-cross-1，实际 %v", idsOf(got))
	}
}

// nil = 不圈定 → 给全部**已上架**节点（与加分类功能之前完全一致）。
//
// 这条是向后兼容的核心：现有订阅与订单的行为一个字都不能变。
func TestNilScopeMeansAllStoreNodes(t *testing.T) {
	t.Parallel()
	got := applySubscriptionAccounts(catFleet(), catRec(), nil)

	if len(got) != 4 {
		t.Fatalf("不圈定时应给全部 4 个已上架节点，实际 %d 个: %v", len(got), idsOf(got))
	}
	if has(got, "node-ai-private") {
		t.Error("未上架节点在任何情况下都不该给买家")
	}
}

// 空集合 ≠ nil：分类下确实没有节点时，订阅应当拿到 **0 个**节点，
// 而不是"退化成不圈定、把全部节点发出去"。
//
// 这是本功能最危险的失效模式：分类被删/配错时反而发得更多。
func TestEmptyScopeYieldsNothingNotEverything(t *testing.T) {
	t.Parallel()
	got := applySubscriptionAccounts(catFleet(), catRec(), map[string]bool{})

	if len(got) != 0 {
		t.Fatalf("空分类集合应给 0 个节点，实际 %d 个: %v\n"+
			"空集合被当成 nil 会让配置写错时反而把全部节点发给买家",
			len(got), idsOf(got))
	}
}

// 分类圈定与 in_store 是**与**的关系，两个都要满足。
func TestCategoryAndStoreAreBothRequired(t *testing.T) {
	t.Parallel()
	// 分类里有 4 个节点，但其中一个未上架。
	scope := map[string]bool{
		"node-ai-1": true, "node-ai-2": true,
		"node-cross-1": true, "node-ai-private": true,
	}
	got := applySubscriptionAccounts(catFleet(), catRec(), scope)

	if len(got) != 3 {
		t.Fatalf("应为 3 个（分类内 ∩ 已上架），实际 %d: %v", len(got), idsOf(got))
	}
	for _, n := range got {
		if !n.InStore {
			t.Errorf("下发了未上架节点 %s", n.ID)
		}
	}
}

// 管理员自用订阅（无订单号）同样受分类圈定 —— 这正是"按分类建订阅"要的。
func TestCategoryAppliesToManualSubscriptions(t *testing.T) {
	t.Parallel()
	rec := catRec()
	rec.OrderID = "" // 手工订阅
	scope := map[string]bool{"node-ai-private": true}

	got := applySubscriptionAccounts(catFleet(), rec, scope)
	// 手工订阅不受 in_store 限制，所以能拿到那个未上架的 AI 节点。
	if len(got) != 1 || got[0].ID != "node-ai-private" {
		t.Fatalf("手工订阅按分类圈定后应拿到 node-ai-private，实际 %v", idsOf(got))
	}
}

// 分类圈定后，节点上的凭证仍要是**该订阅专属**的，不能退回共享密码。
func TestScopedNodesStillCarryOwnCredentials(t *testing.T) {
	t.Parallel()
	scope := map[string]bool{"node-ai-1": true}
	got := applySubscriptionAccounts(catFleet(), catRec(), scope)

	if len(got) != 1 {
		t.Fatalf("期望 1 个节点，实际 %d", len(got))
	}
	if got[0].Password != "P1" {
		t.Errorf("必须用订阅专属密码，实际 %q（共享密码 = 客户端连不上）", got[0].Password)
	}
	if got[0].AccountUserID != "u1" {
		t.Errorf("必须带上账号 user_id 以便回收，实际 %q", got[0].AccountUserID)
	}
}
