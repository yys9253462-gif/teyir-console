package httpapi

// 开户缺口判据必须落在订阅的圈定范围上（2026-10-30）。
//
// 原判据遍历 build.Nodes（全站在役节点），完全不看 category_id / node_ids：
// 一条只圈了 2 台节点的分类订阅，只要全站还有第 3 台节点，就**永远**返回 true。
//
// 后果不是"多做一次开户"（reconcile 会按范围过滤），而是三个连锁反应：
//   1. 买家每次拉订阅都进 JIT 分支，同步白等最多 8 秒；
//   2. gap-fill 每小时把这条订阅重新排进只有 3 个名额的同步队列，永远排不完；
//   3. 真正缺账号的订阅排在这些"假缺口"后面。
// 反方向也错：范围解析失败若被判成"没有缺口"，新节点上的买家拿不到可用节点。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 分类圈定：范围内的节点都有账号 ⇒ 无缺口；范围外的节点即使用全站视角缺号也不算。
func TestProvisionGapRespectsCategoryScope(t *testing.T) {
	rec := &subscriptionRecord{
		ID:         fixSub,
		Enabled:    true,
		ExpiresAt:  ptrTime(time.Now().Add(24 * time.Hour)),
		nodeAccounts: map[string]subscriptionNodeAccount{
			fixNode: {UserID: "sub_a", Password: "pw-a"},
		},
	}
	// 范围内节点已有账号；scope 由 categoryScopeFor 解析，这里直接验证判据本身：
	// build 里另有 fixHost 这台范围外节点且没有账号。
	nodes := []subscriptionNode{
		{ID: fixNode, InStore: true},
		{ID: fixHost, InStore: true},
	}
	s := &Server{}

	// 直接圈定（node_ids）时范围只含 fixNode。
	rec.NodeIDs = []string{fixNode}
	if s.needsAccountProvisioning(context.Background(), nodes, rec) {
		t.Fatal("范围外节点缺账号被判成缺口 —— " +
			"分类/按节点圈定的订阅会永远排队，把真缺口挤出 worker")
	}

	// 不圈定（nil scope）时全站视角：fixHost 确实缺号，应该报缺口。
	rec.NodeIDs = nil
	if !s.needsAccountProvisioning(context.Background(), nodes, rec) {
		t.Fatal("不圈定时范围外节点同样属于可下发节点，缺号必须报缺口")
	}
}

// 范围解析失败必须按「可能有缺口」处理，不能静默当成齐全。
func TestProvisionGapErrOnScopeFailureIsSafe(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	// 分类指向一个不存在的 id ⇒ loadCategoryNodeIDs 返回空集合 + 错误。
	// 判据必须保守返回 true：此时确实不知道有没有缺口，而"多排一次同步"
	// 的代价远小于"新节点漏开户、客户拿不到可用节点"。
	rec := &subscriptionRecord{
		ID:           fixSub,
		Enabled:      true,
		ExpiresAt:    ptrTime(time.Now().Add(24 * time.Hour)),
		nodeAccounts: map[string]subscriptionNodeAccount{fixNode: {UserID: "sub_a", Password: "pw-a"}},
		CategoryID:   ptrString("00000000-0000-0000-0000-0000000009ff"),
	}
	nodes := []subscriptionNode{{ID: fixNode, InStore: true}}
	if !s.needsAccountProvisioning(context.Background(), nodes, rec) {
		t.Fatal("范围解析失败被当成「没有缺口」—— 新节点上的买家会拿不到可用节点")
	}
}

// 判据必须走范围出口，不能退回遍历全站节点。
func TestProvisionGapUsesScopeExit(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "subscription.go", "func (s *Server) needsAccountProvisioning(")
	if !strings.Contains(body, "categoryScopeFor(") {
		t.Fatal("缺口判据必须走 categoryScopeFor —— " +
			"否则分类/按节点圈定的订阅恒为缺口")
	}
}

func ptrString(s string) *string { return &s }
// 2026-11-03：自用订阅（无 order_id）勾选的未上架节点缺账号也是缺口；买家订阅则不算。
// 原来判据与开户候选都写死 in_store=TRUE，界面说「未上架的节点手工订阅也能用」，
// 实际上永远不会开户，开户报告还显示「完整」。
func TestProvisionGapCoversUnlistedNodesForSelfUse(t *testing.T) {
	rec := &subscriptionRecord{
		ID: fixSub, Enabled: true, ExpiresAt: ptrTime(time.Now().Add(24 * time.Hour)),
		NodeIDs:      []string{fixNode},
		nodeAccounts: map[string]subscriptionNodeAccount{},
	}
	nodes := []subscriptionNode{{ID: fixNode, InStore: false}}
	s := &Server{}
	if !s.needsAccountProvisioning(context.Background(), nodes, rec) {
		t.Fatal("自用订阅圈定的未上架节点缺账号必须报缺口")
	}
	rec.OrderID = "shop-order"
	if s.needsAccountProvisioning(context.Background(), nodes, rec) {
		t.Fatal("买家订阅不能在未上架节点上开户")
	}
	all := []subscriptionCandidate{{id: fixNode, inStore: false}, {id: fixHost, inStore: true}}
	rec.NodeIDs = nil
	got, err := s.filterProvisionCandidates(context.Background(), rec, all)
	if err != nil || len(got) != 1 || got[0].id != fixHost {
		t.Fatalf("买家订阅的开户候选只能是已上架节点: %+v %v", got, err)
	}
	rec.OrderID = ""
	rec.NodeIDs = []string{fixNode}
	got, err = s.filterProvisionCandidates(context.Background(), rec, all)
	if err != nil || len(got) != 1 || got[0].id != fixNode {
		t.Fatalf("自用订阅圈定的未上架节点必须是开户候选: %+v %v", got, err)
	}
	if strings.Contains(funcBlock(t, "provisioning.go", "func (s *Server) listProvisionableNodes("), "in_store = TRUE") {
		t.Fatal("开户候选查询不能写死 in_store=TRUE：上架与否由 filterProvisionCandidates 按订阅类型判断")
	}
}

// 2026-11-04：--gapfill-dry-run 的缺口报告必须与真实补开户同一判据。
func TestGapReportUsesSameRuleAsGapFill(t *testing.T) {
	rec := &subscriptionRecord{ID: fixSub, OrderID: "", NodeIDs: []string{fixNode}, nodeAccounts: map[string]subscriptionNodeAccount{}}
	nodes := []subscriptionNode{{ID: fixNode, Name: "unlisted-self-use", InStore: false}, {ID: fixHost, Name: "out-of-scope", InStore: true}}
	got := missingAccountNodes(nodes, rec, map[string]bool{fixNode: true})
	if len(got) != 1 || got[0].Name != "unlisted-self-use" {
		t.Fatalf("self-use scoped unlisted node must be reported, out-of-scope node must not: %+v", got)
	}
	body := funcBlock(t, "provision_gapfill.go", "func (s *Server) ProvisioningGapReport(")
	if !strings.Contains(body, "missingAccountNodes(build.Nodes, rec, scope)") || strings.Contains(body, "n.InStore && (!ok") {
		t.Fatal("the dry-run report must reuse missingAccountNodes")
	}
}
