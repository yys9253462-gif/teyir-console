package httpapi

// 半下线状态巡检（halfdecommissioned.go）的回归测试。
//
// 盯的是 2026-10-23 美国-阿什本事件的直接后果：
// 一台已软删的节点下挂着 15 条活跃订阅的买家账号映射，
// 而 orphan_node_accounts 里该节点 0 条 —— 撤销从未触发，
// 且 manage.go 的 lingering 检测 SQL 带 `AND n.deleted_at IS NULL`
// 恰好把它排除，于是「映射指向已删节点」在整个系统里无处可发现。
//
// 这一组的核心是**不许误伤**：清理逻辑会改生产库的订阅映射，
// 一旦把正常节点也当成"已删"摘掉，客户会突然少一个可用节点。
// 所以边界条件的断言比正向断言更重要。

import (
	"os"
	"strings"
	"testing"
)

// TestDetectHalfDecommissionedSQLTargetsDeletedNodes 断言检测 SQL 的三个关键条件。
//
// 反向验证要点：
//   - 漏掉 `hn.deleted_at IS NOT NULL` → 会把**所有正常节点**当成半下线，
//     把客户的可用节点全部摘掉（生产事故级别）；
//   - 漏掉 `st.deleted_at IS NULL` → 会去动已删除订阅的映射，无谓地改历史数据；
//   - 漏掉 `user_id <> ''` → 会去登记没有 user_id 的条目，孤儿表被垃圾数据污染。
func TestDetectHalfDecommissionedSQLTargetsDeletedNodes(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	// 取出 detectHalfDecommissioned 里那条 SELECT ... WHERE 段落
	start := strings.Index(code, "SELECT hn.id::text")
	if start < 0 {
		t.Fatal("halfdecommissioned.go 里找不到检测 SQL")
	}
	end := strings.Index(code[start:], "`)")
	if end < 0 {
		t.Fatal("检测 SQL 段落未正常结束")
	}
	sql := code[start : start+end]

	required := []struct {
		fragment string
		why      string
	}{
		{"hn.deleted_at IS NOT NULL", "必须只匹配**已软删**的节点 —— 漏掉它会把正常节点全当半下线摘掉"},
		{"st.deleted_at IS NULL", "必须只处理**未删除**的订阅 —— 动已删除订阅的映射属于无谓改历史数据"},
		{"user_id", "必须取订阅里真实的买家账号 user_id —— 否则登记孤儿时没有主体"},
	}
	for _, r := range required {
		if !strings.Contains(sql, r.fragment) {
			t.Errorf("检测 SQL 缺少 %q：%s", r.fragment, r.why)
		}
	}
}

// TestDetectHalfDecommissionedUsesNodeAccountsJoin 断言 JOIN 方式正确。
//
// 必须用 `st.node_accounts ? hn.id::text` 这种「JSON 里含这个键」的判断，
// 而不是 `jsonb_each` 展开后按 user_id 匹配 ——
// 后者会把「该订阅在这台节点上没有账号」也算成命中，误报成一片。
func TestDetectHalfDecommissionedUsesNodeAccountsJoin(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	if !strings.Contains(code, "st.node_accounts ? hn.id::text") {
		t.Error("检测 SQL 必须用 `st.node_accounts ? hn.id::text` 判断「映射里含这个节点」—— " +
			"按 user_id 匹配会把「该订阅在此节点无账号」误判为命中")
	}
	// 展开整张映射表的写法会把该订阅在**所有**节点上的账号都拉进来，
	// 造成大面积误报。
	if strings.Contains(code, "jsonb_each") {
		t.Error("检测 SQL 不应展开整张 node_accounts —— " +
			"那会把每条订阅在所有节点上的账号都拉进来，造成大面积误报")
	}
}

// TestDetectHalfDecommissionedJoinConditionIsBoolean 断言 JOIN 条件是**布尔**表达式。
//
// ⚠️ 这条是被生产事故逼出来的（2026-10-24 部署后立刻暴露）：
//
// 原写法 `ON hn.id = st.node_accounts ? hn.id::text` 里，
// `?` 是 jsonb 的 **exist 操作符**，返回的是 **boolean**。
// 于是整个条件变成 `uuid = boolean` —— PostgreSQL 直接报
// 「operator does not exist: uuid = boolean」，巡检每轮都失败。
//
// 而上一条测试断言的是「代码里含 `st.node_accounts ? hn.id::text`」——
// 错误写法**也含这个子串**，所以测试全绿，缺陷溜过去了。
// 这就是「永远绿的测试等于没有测试」的又一例。
//
// 正确写法：`ON st.node_accounts ? hn.id::text`（整个条件就是个布尔表达式）。
// 这里断言 JOIN 子句的**整体形态**，而不是某个子串。
func TestDetectHalfDecommissionedJoinConditionIsBoolean(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	// 取 JOIN 子句原文。
	i := strings.Index(code, "JOIN hysteria_nodes hn ON ")
	if i < 0 {
		t.Fatal("检测 SQL 里找不到 hysteria_nodes 的 JOIN 子句")
	}
	rest := code[i+len("JOIN hysteria_nodes hn ON "):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		t.Fatal("JOIN 子句未正常结束")
	}
	join := strings.TrimSpace(rest[:end])

	if join != "st.node_accounts ? hn.id::text" {
		t.Errorf("JOIN 条件是 %q，应为 %q —— "+
			"`?` 是 jsonb 的 exist 操作符（返回 boolean），"+
			"把它跟 uuid 用 `=` 连起来会得到 `uuid = boolean`，"+
			"PostgreSQL 直接报「operator does not exist: uuid = boolean」",
			join, "st.node_accounts ? hn.id::text")
	}
}

// TestHalfDecommissionedDoesNotClaimPhysicalRevocation 断言文案不谎称已注销。
//
// 这是这套代码一贯的诚实性要求：节点凭据已被抹除，**物理上无法**连接节点
// 去删买家账号。登记为 orphan 是留痕，不是"已注销"。
// 若文案写成"已注销"，运维会以为买家账号已经清理干净 ——
// 而实际上账号可能还留在机器上能连（"钱退了货还在"的同一类病）。
func TestHalfDecommissionedDoesNotClaimPhysicalRevocation(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	for _, forbidden := range []string{"已注销", "注销成功", "已物理删除"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("文案里出现 %q —— 节点凭据已抹除，物理上无法连接节点注销买家账号，"+
				"登记为孤儿只是留痕，不能谎称已注销", forbidden)
		}
	}
	// 应当明确说明"无法注销"这个事实
	if !strings.Contains(code, "无法连接节点注销") {
		t.Error("文案必须明确说明「无法连接节点注销」—— " +
			"否则运维会误以为买家账号已清理干净")
	}
}

// TestHalfDecommissionedVerifiesMappingRemoval 断言清理后会核对映射是否真被摘掉。
//
// recordOrphanAccounts 只在**登记成功**时才摘映射；登记失败会保留映射
// （至少还有一处线索）。这种情况下若静默当作已清理，
// 就是「以为清了其实没清」—— 正是这套代码一直在防的那类病。
func TestHalfDecommissionedVerifiesMappingRemoval(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	if !strings.Contains(code, "node_accounts") {
		t.Fatal("清理逻辑里应包含对 node_accounts 的操作")
	}
	// 必须有"核对残留"的查询：SELECT ... ? $1 的形式
	if !strings.Contains(code, "? $1 FROM subscription_tokens") {
		t.Error("清理后必须回查该订阅里是否还残留该节点的映射 —— " +
			"不核对就会把「登记失败导致映射没摘掉」当作已清理")
	}
}

// TestHalfDecommissionedReasonMentionsRootCause 断言孤儿登记原因说清了根因。
//
// 运维在「节点回收」里看到这条记录时，必须能一眼看出它是"半下线"，
// 而不是误以为节点只是临时连不上。
func TestHalfDecommissionedReasonMentionsRootCause(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "halfdecommissioned.go"))

	if !strings.Contains(code, "半下线") {
		t.Error("孤儿登记原因里应点明「半下线」—— " +
			"运维在「节点回收」里必须能一眼看出这是节点已删除导致的残留")
	}
	if !strings.Contains(code, "已软删节点") && !strings.Contains(code, "已软删") {
		t.Error("孤儿登记原因里应说明节点已软删 —— " +
			"否则会被误读成「节点只是暂时连不上」")
	}
}

// TestHalfDecommissionedSweepIsWiredIntoReconcile 断言巡检确实被调用。
//
// 写了函数但没接到 reconcile 循环里，等于没实现 ——
// 这是最容易发生的「看起来做了其实没做」。
//
// ⚠️ 断言必须匹配**真正的调用语句**，不能只查函数名出现过：
// 只查「源码里有没有这个名字」会被注释里的提及骗过去
// （2026-10-24 反向验证实测：把调用改名后测试仍然全绿，
//   因为注释里还留着同一个名字）。
// 所以这里要求出现 `s.sweepHalfDecommissioned(` 这种带括号的形式 ——
// 注释里写的是不带括号的「sweepHalfDecommissioned(ctx) 时」之类，
// 且 stripGoComments 已经把注释剔掉了。
func TestHalfDecommissionedSweepIsWiredIntoReconcile(t *testing.T) {
	t.Parallel()

	manage := stripGoComments(readSourceFile(t, "manage.go"))
	if !strings.Contains(manage, "s.sweepHalfDecommissioned(") {
		t.Fatal("manage.go 的 ReconcileAlerts 里没有真正调用 sweepHalfDecommissioned —— " +
			"巡检函数写了却没接到循环里，等于没实现")
	}
}

// TestStripGoCommentsRemovesCommentOnlyMatches 顺带验证 stripGoComments 本身可靠。
//
// 它是上面几条断言的基础：若它不剔除注释，这些断言会因为注释里
// 提到的关键字而误判为通过（永远绿的测试 = 没有测试）。
func TestStripGoCommentsRemovesCommentOnlyMatches(t *testing.T) {
	t.Parallel()

	src := "package x\n// 这行注释里有 KEYWORD\nvar a = 1 // 行尾注释\n/* 块注释\nKEYWORD\n*/\nvar b = 2\n"
	out := stripGoComments(src)

	if strings.Contains(out, "KEYWORD") {
		t.Error("stripGoComments 没有剔除注释 —— " +
			"基于它做的断言会因为注释里的关键字而误判为通过")
	}
	if !strings.Contains(out, "var a = 1") || !strings.Contains(out, "var b = 2") {
		t.Error("stripGoComments 把代码也删掉了")
	}
}

// TestHalfDecommissionedFileExists 兜底：文件必须在（防止误删后测试仍绿）。
func TestHalfDecommissionedFileExists(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("halfdecommissioned.go"); err != nil {
		t.Fatalf("halfdecommissioned.go 不存在：%v", err)
	}
}
