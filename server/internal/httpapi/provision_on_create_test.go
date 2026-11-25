package httpapi

// 手工建订阅必须开户。
//
// 这是一个**真 BUG 的回归测试**（2026-10-04 端到端演练发现）：
//
//	createSubscriptionHandler 只插了一行 DB 记录就返回，
//	**完全没在节点上开户**；而商城下单路径（store.go:691）
//	是**会**调 provisionAndPersist 的。
//
// 两条路径的不对称造成一个很隐蔽的故障：手工开的订阅，
// 客户导入链接后连不上 —— 因为节点侧压根没有这个账号。
// 订阅链接本身有效（它只是把所有节点拼在一起），节点缺账号就认证失败。
//
// 实测复现：手工建订阅后 node_accounts 为空（0 个节点），
// 而对账（reconcile）能刷出 4 个 —— 说明拉取链路是通的，
// 只是开户从没被触发。

import (
	"strings"
	"testing"
)

func TestCreateSubscriptionProvisionsAccounts(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) createSubscriptionHandler(")

	if !strings.Contains(body, "s.provisionAndPersist(") {
		t.Fatal("手工建订阅必须调 provisionAndPersist —— " +
			"否则节点上没有账号，客户导入链接后认证失败。" +
			"商城下单路径是调了的，两条路径必须一致。")
	}
	// 必须在写响应之前 —— 响应要如实报告开成了几个节点。
	provIdx := strings.Index(body, "s.provisionAndPersist(")
	respIdx := strings.Index(body, "writeJSON(w, http.StatusOK")
	if provIdx > 0 && respIdx > 0 && provIdx > respIdx {
		t.Error("开户必须发生在写响应之前 —— " +
			"否则响应报不出真实开户数")
	}
}

func TestCreateSubscriptionReportsProvisionedCount(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) createSubscriptionHandler(")

	// 只报"成功"会让运维以为全好了，而某个节点上的客户仍然连不上。
	if !strings.Contains(body, `"provisioned_nodes"`) {
		t.Error("响应应带开成的节点数 —— 只报成功会让运维不知道有节点漏了")
	}
	if !strings.Contains(body, `"excluded"`) {
		t.Error("部分成功时应列出哪些节点没开成，" +
			"以及「可以用对账重试」这个补救办法")
	}
}

// 缺口描述要给出**可执行的补救办法**，不能只说"失败了"。
func TestProvisionGapTellsHowToFix(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "provisioning.go", "func scopedProvisionReport(")
	if !strings.Contains(body, "对账") {
		t.Error("缺口说明里要告诉运维「可以用账号对账重试」—— " +
			"只说失败等于把问题丢回去")
	}
	if !strings.Contains(body, "认证失败") {
		t.Error("要说清后果（客户端会认证失败），" +
			"否则运维不知道这件事急不急")
	}
}

// 缺口必须按**这条订阅的圈定范围**统计（2026-10-30）。
//
// 原判据拿 len(rec.nodeAccounts) 跟"全站可开户节点数"比：
// 分类圈定 / 按节点圈定的订阅只在一部分节点上开户，于是全站 8 台、
// 这条只圈 2 台且都开成了，仍被判成"有缺口"，对着一次完整成功的交付
// 报出"这些节点上的客户端会认证失败"。
//
// 反方向也错：可开户节点数读库失败时旧实现返回 0 ⇒ 真实缺口被静默跳过。
// 现在两条路径都走同一个范围判据，读库失败必须如实报 unverified。
func TestProvisionReportScopedAndHonestAboutUnknown(t *testing.T) {
	t.Parallel()
	report := funcBlock(t, "provisioning.go", "func scopedProvisionReport(")
	if !strings.Contains(report, "filterProvisionCandidates(") {
		t.Error("缺口统计必须按订阅圈定范围过滤 —— " +
			"拿全站节点数比会把完整成功的交付报成部分失败")
	}
	if !strings.Contains(report, "return provisionReport{}, err") {
		t.Error("读库失败必须返回错误，不能当成「没有缺口」—— " +
			"那会让真实缺口静默消失")
	}
	handler := funcBlock(t, "store.go", "func (s *Server) createSubscriptionHandler(")
	for _, want := range []string{"scopedProvisionReport(", `"unverified"`, `"partial"`, `"complete"`} {
		if !strings.Contains(handler, want) {
			t.Errorf("建订阅响应缺少 %s —— 「不知道」也必须说出来", want)
		}
	}
	// 旧的全站计数已无调用点，留着会诱使后来者再次接错判据。
	if strings.Contains(report, "provisionableNodeCount(") {
		t.Error("缺口统计不得退回全站节点数")
	}
}
