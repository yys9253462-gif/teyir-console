package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

// 「面板自建的那条合并订阅」是 SELECT ... LIMIT 1 取出来的，这里盯住它的
// 两个必须条件 —— 缺任何一个都会重演一次线上事故：
//
//	事故现场（2026-09-30）：表里 22 行，其中 21 行是回归脚本软删除的残留。
//	原来的查询是 `SELECT ... FROM subscription_tokens LIMIT 1`，
//	既没有 WHERE 也没有 ORDER BY，于是 PostgreSQL 返回了
//	`回归-1-真删`（enabled=false、deleted_at 非空）那一行。
//	/api/subscription 把这条已撤销的 token 当成"当前合并订阅"下发，
//	客户端拿到的 /sub/<token> 直接 404；而节点收录、订阅构建全都正常，
//	界面上一点异常都看不出来。
//
// 这是个纯静态问题（SQL 文本里少一个子句），不需要数据库就能守住，
// 所以用源码级检查而不是集成测试 —— 与 store_columns_test.go 同一思路。

func readSubscriptionGo(t *testing.T) string {
	t.Helper()
	// 复用 readStoreGo 的读文件方式；这里单独读 subscription.go
	return readFileForTest(t, "subscription.go")
}

func TestReadSubscriptionTokenFiltersSoftDeleted(t *testing.T) {
	src := readSubscriptionGo(t)
	body := extractFuncBodyFrom(t, src, "readSubscriptionToken")

	// 取出这条查询的 SQL 文本
	sql := extractSQLInFunc(body)
	if sql == "" {
		t.Fatalf("在 readSubscriptionToken 里找不到 SQL 查询 —— " +
			"函数被重构了请同步更新本测试（它守着一次真实线上事故）")
	}
	flat := strings.Join(strings.Fields(sql), " ")

	if !strings.Contains(flat, "subscription_tokens") {
		t.Fatalf("readSubscriptionToken 的查询不再针对 subscription_tokens：%s", flat)
	}

	if !strings.Contains(flat, "deleted_at IS NULL") {
		t.Errorf("readSubscriptionToken 的查询没有过滤 deleted_at IS NULL。\n"+
			"subscription_tokens 是软删除表，不过滤就会把已撤销的订阅选成"+
			"「当前合并订阅」下发给客户端，而那个 token 在 /sub 上必然 404。\n"+
			"当前 SQL：%s", flat)
	}

	if !regexp.MustCompile(`(?i)ORDER BY`).MatchString(flat) {
		t.Errorf("readSubscriptionToken 的查询没有 ORDER BY。\n"+
			"LIMIT 1 在没有 ORDER BY 时，返回哪一行由物理存储顺序决定 —— "+
			"同一份数据在不同时刻可能给出不同的订阅链接。\n"+
			"当前 SQL：%s", flat)
	}
}

// TestSoftDeletedSelectsNeedOrderBy 扫一遍所有针对软删除表的 LIMIT 1 查询，
// 要求它们都带 ORDER BY。
//
// 不要求它们都过滤 deleted_at：是否过滤取决于语义（例如任务历史就**应该**
// 保留已删主机的记录）。但「挑一行」这件事必须是确定的。
//
// 注意：必须先剥掉注释再扫。第一版没剥，于是把函数注释里**引用**旧代码的那行
// `SELECT ... FROM subscription_tokens LIMIT 1` 当成了真查询，报了一个假失败。
// 这类守卫测试本身也得防"把自己写进注释的例子当成代码"。
func TestSoftDeletedSelectsNeedOrderBy(t *testing.T) {
	files := []string{"subscription.go", "store.go", "nodeapi.go", "server.go", "manage.go"}
	limRe := regexp.MustCompile("`([^`]*LIMIT 1[^`]*)`")

	checked := 0
	for _, f := range files {
		src := stripComments(readFileForTest(t, f))
		for _, m := range limRe.FindAllStringSubmatch(src, -1) {
			q := strings.Join(strings.Fields(m[1]), " ")
			// 只看会累积软删除行的表
			if !strings.Contains(q, "subscription_tokens") &&
				!strings.Contains(q, "hysteria_nodes") &&
				!strings.Contains(q, "servers") {
				continue
			}
			if !regexp.MustCompile(`(?i)ORDER BY`).MatchString(q) {
				t.Errorf("%s 里有一条 LIMIT 1 查询没有 ORDER BY，选行结果不确定：\n  %s", f, q)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Log("没有扫描到针对软删除表的 LIMIT 1 查询（结构可能变了）")
	}
}

// stripComments 去掉 Go 源码里的行注释与块注释。
//
// 刻意写得简单（不处理字符串字面量里的 // ）：本项目的 SQL 都在反引号里，
// 里面不会出现 //，而注释里出现的 ` 才是要排除的对象。
func stripComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	s := strings.Join(out, "\n")
	// 块注释
	blockRe := regexp.MustCompile(`(?s)/\*.*?\*/`)
	return blockRe.ReplaceAllString(s, "")
}
