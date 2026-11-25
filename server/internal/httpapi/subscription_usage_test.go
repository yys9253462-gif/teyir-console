package httpapi

import (
	"reflect"
	"strings"
	"testing"
)

// 订阅用量的**聚合口径**测试（2026-10-02 管理员要求"显示用量"）。
//
// 为什么用读源码断言而不是真库测试：
// loadSubscriptionUsage 依赖 *pgxpool.Pool（具体类型，无可注入接口），
// 为它引入 sqlmock 属于超出范围的重构。而这里要钉住的其实是**口径**，
// 那是读代码就能验证的、且最容易在后续改动里悄悄走样的部分。
// 端到端验证在真机完成（看 subscription_usage 表被填上有意义的值）。
//
// 采集侧的不变量在 collector/instability_test.go 同款的 usage_test.go 里。

// 用量必须跨节点求和 —— 一条订阅在多个节点上各有账号，
// 只取一个节点会少算，而少算的用量在界面上看不出任何异常。
func TestUsageSumsAcrossNodes(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) loadSubscriptionUsage(")

	if !strings.Contains(body, "agg.MeasuredBytes += *used") {
		t.Error("用量必须跨节点**累加**：一条订阅在多个节点上各有账号，" +
			"只取一个会少算，而少算在界面上看不出异常")
	}
}

// 上限（limit）的合成规则与用量**不同**：
// 只要有一个节点是"不限"（0），整体就不限 —— 否则会出现
// "总上限比实际可用量还小"的荒谬结果。
func TestUsageLimitIsUnlimitedIfAnyNodeUnlimited(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func mergeSubscriptionLimit(")

	if !strings.Contains(body, "*current == 0 || limit == 0") {
		t.Error("上限合成必须把「任一节点不限」当作整体不限；" +
			"简单求和会得出比实际可用量还小的总上限")
	}
	if !strings.Contains(body, "if current == nil") {
		t.Error("必须把 nil（尚无样本）与 0（不限）区分开")
	}
}

func TestUsageLimitAggregationValues(t *testing.T) {
	t.Parallel()
	ptr := func(v int64) *int64 { return &v }
	cases := []struct {
		name     string
		initial  *int64
		incoming int64
		want     int64
	}{
		{name: "first finite sample", initial: nil, incoming: 100, want: 100},
		{name: "sum finite samples", initial: ptr(100), incoming: 200, want: 300},
		{name: "existing unlimited", initial: ptr(0), incoming: 200, want: 0},
		{name: "incoming unlimited", initial: ptr(100), incoming: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeSubscriptionLimit(tc.initial, tc.incoming)
			if got == nil || *got != tc.want {
				t.Fatalf("mergeSubscriptionLimit() = %v, want %d", got, tc.want)
			}
		})
	}
}

// 「从未采到」必须与「确实是 0」区分开 —— 用指针类型才表达得了 null。
//
// 这是本项目反复出现的那类坑（null vs 0）。用量场景尤其危险：
// 把"没采到"显示成 0 B，运维会以为客户没在用，而实际是采集失败。
func TestUsageDistinguishesNeverSampledFromZero(t *testing.T) {
	t.Parallel()

	field, ok := reflect.TypeOf(subscriptionUsageAgg{}).FieldByName("UsedBytes")
	if !ok || field.Type != reflect.TypeOf((*int64)(nil)) {
		t.Error("UsedBytes 必须为可空指针")
	}

	// 接口层要把这个 null 透出去，不能在这里就填 0。
	body := funcBlock(t, "store.go", "func (s *Server) listSubscriptionsHandler(")
	if !strings.Contains(body, "use.UsedBytes") {
		t.Error("接口必须把 UsedBytes 原样透出（可为 null），不能在服务端填 0")
	}
}

// 任何一台节点没采到，整体就不能算"完整"。
//
// 把"部分节点没采到"说成"用量就是这样"是谎报 —— 与本项目铁律①
// （回收/退款类操作必须验返回值是否诚实）同一条道理。
func TestUsageMarkedIncompleteWhenAnyNodeFails(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) loadSubscriptionUsage(")

	if !strings.Contains(body, "if !quality {") {
		t.Error("必须检查每个节点的 ok 标志")
	}
	if !strings.Contains(body, "agg.OK = false") {
		t.Error("任一节点采集失败都要把整体标为不完整（OK=false），" +
			"否则前端会把残缺数据当成完整用量展示")
	}
	if !strings.Contains(body, "agg.Reason") {
		t.Error("要把失败原因带出去 —— 只说「不完整」而不说为什么，运维无从下手")
	}
}

// 采样时间取**最晚**的一次：它代表"这份数据最新到什么时候"。
func TestUsageSampledAtTakesOldest(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "store.go", "func (s *Server) loadSubscriptionUsage(")

	if !strings.Contains(body, "sampled.Before(*agg.SampledAt)") {
		t.Error("采样时间必须以最陈旧节点为准，不能让新样本掩盖其他旧值")
	}
}
