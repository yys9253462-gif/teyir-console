package httpapi

// reconcile 的两个守卫：全量上限闸、返回值诚实性。

import (
	"strings"
	"testing"
)

// 全量对账必须有上限闸。
//
// 理由（2026-10-04 加）：reconcile-all 是**同步循环**，
// 每个订阅对每个节点发一次请求，单次最长 10s 超时。
// 订阅上量后最坏耗时 = 订阅数 × 节点数 × 10s ——
// 100 个订阅 × 5 节点就是 5000 秒，会打爆 HTTP 超时，
// 且同时向所有节点压上有意义的写负载。
//
// 生产当前只有 2 个订阅、秒级完成；这个闸是为**上量之后**准备的。
func TestReconcileAllHasSubscriptionCap(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "provisioning.go", "func (s *Server) reconcileAllSubscriptionsHandler(")
	if !strings.Contains(body, "reconcileAllMaxSubscriptions") {
		t.Fatal("全量对账必须有订阅数上限闸 —— " +
			"它是同步循环，订阅上量后会超时并压垮节点")
	}
	// 超限必须**拒绝并说明**，不能静默截断。
	// 截断会让运维以为"全都对好了"，实际只处理了一部分 ——
	// 这比直接报错危险得多。
	if !strings.Contains(body, "http.StatusConflict") {
		t.Error("超限应返回 409 并说明原因，不能静默截断或返回 200")
	}
	if !strings.Contains(body, "分批") {
		t.Error("拒绝时要说清怎么办（分批 / 用单条对账），" +
			"只说「超限」等于把问题丢回给运维")
	}
	// 拒绝时把实际数量带回去，运维才知道超了多少。
	if !strings.Contains(body, `"subscriptions"`) || !strings.Contains(body, `"limit"`) {
		t.Error("409 响应应带上实际订阅数与上限值")
	}
}

func TestReconcileAllDoesNotDoubleCountPersistFailures(t *testing.T) {
	body := funcBlock(t, "provisioning.go", "func (s *Server) reconcileAllSubscriptionsHandler(")
	start := strings.Index(body, "if persistErr != nil")
	if start < 0 {
		t.Fatal("missing persistence-failure branch")
	}
	end := strings.Index(body[start:], "continue")
	if end < 0 {
		t.Fatal("persistence-failure branch does not continue")
	}
	branch := body[start : start+end]
	if !strings.Contains(branch, "totalFailed += len(failures) + 1") {
		t.Fatal("failed persistence must count node failures once plus the persistence failure")
	}
	if strings.Contains(branch, "totalFailed += len(failures)\n") {
		t.Fatal("failed nodes are counted twice in the aggregate")
	}
}

func TestReconcileCapIsReasonable(t *testing.T) {
	t.Parallel()
	// 上限太大就失去了保护意义（100 × 5 × 10s = 5000s）。
	// 太小则日常没法用。50 是「2 个订阅的现状」与「一次别跑太久」之间的取舍。
	if reconcileAllMaxSubscriptions > 100 {
		t.Errorf("上限 %d 太大，失去了超时保护的意义", reconcileAllMaxSubscriptions)
	}
	if reconcileAllMaxSubscriptions < 10 {
		t.Errorf("上限 %d 太小，日常不可用", reconcileAllMaxSubscriptions)
	}
}

// 单条对账必须**如实报告失败节点**。
//
// 部分成功时若只说"完成"，运维会以为全好了，
// 而某个节点上的客户仍然连不上 —— 这是本项目反复出现的
// 「返回值不诚实」类问题（见铁律①）。
func TestReconcileReportsFailedNodes(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "provisioning.go", "func (s *Server) reconcileSubscriptionHandler(")
	if !strings.Contains(body, `"failed_nodes"`) {
		t.Error("必须返回 failed_nodes —— 部分成功不能报成全部成功")
	}
	if !strings.Contains(body, `"failed"`) {
		t.Error("必须带上失败节点的名字与原因，否则运维不知道要去修哪一台")
	}
	// 失败节点不能被写回映射（失败节点未改动）。
	if !strings.Contains(body, "失败节点未改动") {
		t.Error("message 里要说清失败节点未被改动 —— " +
			"否则运维会以为失败的那台也刷过了")
	}
}

// 密码必须是确定性推导的（对账幂等的前提）。
// 这条已在 password_forward_test.go 里钉过，这里只确认对账路径也用它。
func TestReconcileUsesDeterministicPassword(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "provisioning.go", "func (s *Server) reconcileSubscriptionAccounts(")
	if !strings.Contains(body, "reconcileOneNode") {
		t.Error("对账必须复用 provisionOneNode —— 那条路径用的是确定性密码，" +
			"自己另写一份会让两边推导规则漂移")
	}
}
