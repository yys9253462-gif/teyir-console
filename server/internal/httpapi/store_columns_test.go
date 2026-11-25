package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// readStoreGo 读同目录下的 store.go 源码，供源码级检查使用。
//
// go test 的工作目录就是本包目录，所以直接读文件名即可。
func readStoreGo(t *testing.T) string {
	t.Helper()
	return readFileForTest(t, "store.go")
}

// readFileForTest 读本包目录下的任意源文件。几个源码级守卫测试共用。
func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读不到 %s：%v", name, err)
	}
	return string(b)
}

// subscriptionColumns 是一个被多处复用的 SELECT 列清单。它加一列，所有
// 读它的地方都必须跟着加一个 Scan 目标 —— 这个过程没有任何编译期保护：
//
//	SELECT 出 16 列、Scan 接 15 个，pgx 只在**运行时**报
//	  number of field descriptions must equal number of destinations, got 16 and 15
//
// 010 加 node_accounts 时就真的漏了一处：单行版 scanSubscription 补了，
// 列表版 listSubscriptions 没补。后果是整个「订阅中心」列表 500，
// 而单条订阅接口照常 200 —— 看起来像"订阅中心坏了，订阅功能正常"，
// 极具误导性（用户报的就是这个）。
//
// 这个测试把「列数必须等于 Scan 目标数」钉死在编译期之后的最近一道关卡上：
// 以后谁加了列忘了改 Scan，跑 go test 就会红，而不是等线上 500。

// countColumns 按顶层逗号切分列清单。
//
// 不能直接 strings.Split(s, ",")：COALESCE(order_id, 空串) 里就有逗号，
// 盲目切会多数一列。按括号深度判断才是对的。
//
// （这里刻意不把空串写成两个单引号：Go 1.19 起 gofmt 会把文档注释里连续
// 两个单引号规整成一个右弯引号，于是注释会被改成「COALESCE(order_id,”)」，
// 与要描述的 SQL 已经不是一回事了。改写措辞比接受那次"规整"更诚实。）
func countColumns(list string) []string {
	var (
		out   []string
		depth int
		cur   strings.Builder
	)
	for _, ch := range list {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(ch)
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	// 过掉空项
	var clean []string
	for _, c := range out {
		if c != "" {
			clean = append(clean, c)
		}
	}
	return clean
}

func TestSubscriptionColumnsCount(t *testing.T) {
	cols := countColumns(subscriptionColumns)

	// 这个数字是「常量本身的列数」，不是数据库表的列数 ——
	// 常量故意不选 created_by（它从不下发到 API）。
	// 每次给 subscription_tokens 加列并带进这个常量，都要同步改这里：
	// 它的作用就是让「加列忘了同步 Scan」在编译前暴露出来。
	const wantCols = 18 // 044 加了 node_ids
	if len(cols) != wantCols {
		t.Fatalf("subscriptionColumns 有 %d 列，期望 %d：\n%v", len(cols), wantCols, cols)
	}

	// 必须包含 node_accounts：010 加的列，漏在 Scan 里就是那次 500 的根因。
	// 必须包含 category_id：026 加的列，漏掉会让「按分类圈定订阅」静默失效
	// （读出来永远是 nil = 不圈定 = 把全部节点发给买家）。
	for _, need := range []string{"node_accounts", "category_id", "node_ids"} {
		found := false
		for _, c := range cols {
			// node_ids 以 COALESCE(node_ids::text[], '{}') 的形式出现，按「包含」判断。
			if c == need || strings.Contains(c, "("+need+"::") {
				found = true
			}
		}
		if !found {
			t.Errorf("subscriptionColumns 缺少 %s —— 见上面每个列的说明", need)
		}
	}
}

// TestSubscriptionScanTargetCount 用源码分析的方式，确认每一处
// `SELECT `+subscriptionColumns 的调用点，其 Scan 目标数都等于列数。
//
// 为什么不直接调函数：这些函数都需要一个活的 pgx 连接，而它们要防的
// 正是「列与目标的形状不匹配」这种纯静态问题 —— 用源码级检查更直接，
// 也与本项目既有的「源码级守卫」做法一致（见 store.go 里的同类注释）。
func TestSubscriptionScanTargetCount(t *testing.T) {
	cols := countColumns(subscriptionColumns)
	want := len(cols)

	// 逐个盯着这两个函数：它们是仅有的、手动列出 Scan 目标的地方。
	// 其余用到 subscriptionColumns 的路径都走 scanSubscription。
	cases := []struct {
		name string
		body string
	}{
		{"scanSubscription", extractFuncBody(t, "scanSubscription")},
		{"listSubscriptions", extractFuncBody(t, "listSubscriptions")},
	}

	for _, c := range cases {
		targets := countScanTargets(c.body)
		if targets != want {
			t.Errorf("%s 的 Scan 目标有 %d 个，但 subscriptionColumns 是 %d 列。\n"+
				"SELECT 出的列数必须与 Scan 目标数一致，否则 pgx 在运行时报\n"+
				"  number of field descriptions must equal number of destinations\n"+
				"加列时请同时更新 scanSubscription 与 listSubscriptions。",
				c.name, targets, want)
		}
	}
}

// extractFuncBody 从 store.go 的源码里抠出一个函数的函数体。
func extractFuncBody(t *testing.T, name string) string {
	t.Helper()
	return extractFuncBodyFrom(t, readStoreGo(t), name)
}

// extractFuncBodyFrom 从给定源码里抠出函数体。通用版，供多个守卫测试共用。
func extractFuncBodyFrom(t *testing.T, src, name string) string {
	t.Helper()

	// 匹配 `func <name>(`（含无接收者）或 `func (recv) <name>(`（含接收者）。
	// 本项目两种写法都有：scanSubscription 是自由函数，
	// listSubscriptions 挂在 *Server 上。
	startRe := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
	loc := startRe.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("找不到函数 %s —— 它被改名或移走了，"+
			"请同步更新本测试（这个测试的存在意义就是盯住它）", name)
	}
	rest := src[loc[0]:]
	endRe := regexp.MustCompile(`(?m)^func `)
	if next := endRe.FindStringIndex(rest[1:]); next != nil {
		return rest[:next[0]+1]
	}
	return rest
}

// extractSQLInFunc 从函数体里取出第一个反引号包裹的 SQL 文本。
func extractSQLInFunc(body string) string {
	re := regexp.MustCompile("`([^`]+)`")
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(m[1]), " ")
}

// countScanTargets 数出函数体里 `.Scan(...)` 的参数个数。
//
// 只看 `&xxx` 形式的取址参数 —— 这是本项目所有 Scan 调用的写法，
// 数它们比解析完整参数列表更稳。
func countScanTargets(body string) int {
	idx := strings.Index(body, ".Scan(")
	if idx < 0 {
		return 0
	}
	rest := body[idx+len(".Scan("):]
	// 找到与之配对的右括号
	depth, end := 1, -1
	for i, ch := range rest {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return 0
	}
	args := rest[:end]
	// 按顶层逗号切
	return len(countColumns(args))
}
