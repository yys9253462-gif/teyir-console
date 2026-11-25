package httpapi

import (
	"os"
	"strings"
	"testing"
)

// 缺口 B 修复的守卫测试（2026-10-04）。
//
// 背景：旧实现的 lapse 只跑一条 UPDATE 把 enabled 置 FALSE，节点账号原封不动；
// 而真正的 purge 带 `auto_delete=TRUE` 条件，于是 auto_delete=false 的订阅
// 过期后账号永久保留 —— 买家手里的 Reality 配置与 hy2 密码都还能用。
//
// 这里用**整个源文件**做断言，不用 funcBlock 切片 ——
// 函数体边界在格式化/重排后容易变，而「文件里存在某个必要构件」是稳定的。
func TestSweepExpiredRevokesNodeAccounts(t *testing.T) {
	raw, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("读不到 store.go: %v", err)
	}
	src := string(raw)

	// 1) 必须调用 revokeSubscriptionAccounts —— 这是「真的收回账号」的唯一途径。
	if !strings.Contains(src, "revokeSubscriptionAccounts") {
		t.Fatal("store.go 必须调用 revokeSubscriptionAccounts；" +
			"只把 enabled 置 FALSE 会让节点账号（含 Reality UUID）永久保留")
	}

	// 2) lapse 的待收回查询必须**不含 auto_delete**（原缺口就是被它挡住的），
	//    且必须以 node_accounts 非空为判据（保证幂等、可重试）。
	idx := strings.Index(src, "lapse select failed")
	if idx < 0 {
		t.Fatal("找不到 lapse 查询（期望有 'lapse select failed' 的日志文案）")
	}
	// 向前找该查询的起点（最近的 SELECT）
	head := src[:idx]
	qStart := strings.LastIndex(head, "SELECT")
	if qStart < 0 {
		t.Fatal("lapse 查询前找不到 SELECT")
	}
	// 向后找该查询的终点（最近的 '`' 闭合）
	tail := src[qStart:]
	qEnd := strings.Index(tail, "`,")
	if qEnd < 0 {
		qEnd = strings.Index(tail, "`\n")
	}
	if qEnd < 0 {
		t.Fatal("lapse 查询找不到闭合反引号")
	}
	q := tail[:qEnd]

	if strings.Contains(q, "auto_delete") {
		t.Fatalf("lapse 的待收回查询不应带 auto_delete 条件"+
			"（会漏掉 auto_delete=false 的订阅，这正是要修的缺口）：%q", q)
	}
	if !strings.Contains(q, "node_accounts") {
		t.Fatalf("待收回查询应以 node_accounts 非空为判据（幂等、可重试）：%q", q)
	}
	if !strings.Contains(q, "expires_at") || !strings.Contains(q, "enabled") {
		t.Fatalf("待收回查询应筛选「已过期且仍启用」的行：%q", q)
	}

	// 3) 收回失败必须保留该行供重试（不能继续标记 enabled=FALSE）。
	if !strings.Contains(src, "will retry") {
		t.Fatal("收回不完整时必须留下可检索的重试日志（含 will retry）")
	}
}

// 缺口 A 的 Go 侧守卫：停用/启用必须下发给节点 /users/set_status，
// 不能只改面板本地状态 —— Reality 走 xray 白名单，不经过 hy2 鉴权。
func TestSubscriptionStateUsesSetStatusEndpoint(t *testing.T) {
	raw, err := os.ReadFile("subscription_state.go")
	if err != nil {
		t.Fatalf("读不到 subscription_state.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "/users/set_status") {
		t.Fatal("停用/启用必须下发给节点 /users/set_status；" +
			"只改面板本地状态不会切断 Reality（xray 不经过 hy2 鉴权）")
	}
	// 必须把 active 作为参数传给节点（否则节点不知道该开还是该关）
	if !strings.Contains(src, "active") {
		t.Fatal("调用 /users/set_status 必须携带 active 参数")
	}
}

// 缺口 A 的真正修复点在 portal.py：reality_clients 必须按 status 过滤。
func TestPortalRealityClientsFiltersDisabled(t *testing.T) {
	candidates := []string{
		os.Getenv("PORTAL_PY_PATH"),
		"/etc/hysteria/portal.py",
		"/src/portal.py",
	}
	var src string
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if raw, err := os.ReadFile(p); err == nil {
			src = string(raw)
			break
		}
	}
	if src == "" {
		t.Skip("找不到 portal.py，跳过跨仓库守卫（用 PORTAL_PY_PATH 指定）")
	}

	start := strings.Index(src, "def reality_clients(")
	if start < 0 {
		t.Fatal("portal.py 里找不到 reality_clients")
	}
	rest := src[start:]
	// 函数体到下一个顶层 def 或 class 为止
	for _, marker := range []string{"\ndef ", "\nclass ", "\n_reality"} {
		if next := strings.Index(rest[1:], marker); next > 0 {
			if next+1 < len(rest) {
				rest = rest[:next+1]
			}
			break
		}
	}

	if !strings.Contains(rest, "status") {
		t.Fatalf("reality_clients 必须按 status 过滤：否则停用后 Reality 仍可连（实测 204）。函数体片段：%q",
			clipForGapTest(rest, 400))
	}
	if !strings.Contains(rest, "continue") {
		t.Fatalf("reality_clients 应跳过非 active 用户（continue）。函数体片段：%q",
			clipForGapTest(rest, 400))
	}
	if !strings.Contains(rest, "users") {
		t.Fatalf("reality_clients 需要读 users 才能知道 status。函数体片段：%q",
			clipForGapTest(rest, 400))
	}
}

// 「用户不存在」必须算收回成功（2026-10-04 补缺口 B 时被真实测出来的）。
//
// 收回的**目标**是「该账号在节点上不存在」。若它本来就不存在，
// 目标已达成 —— 404 "User not found" 是幂等信号而非错误。
// 旧行为把它当失败 ⇒ node_accounts 永远清不掉 ⇒ 该行永远「收回失败」、
// 每轮重试、并把假告警刷进日志。
func TestRevokeTreatsUserNotFoundAsSuccess(t *testing.T) {
	raw, err := os.ReadFile("provisioning.go")
	if err != nil {
		t.Fatalf("读不到 provisioning.go: %v", err)
	}
	src := string(raw)

	start := strings.Index(src, "func (s *Server) revokeSubscriptionAccounts(")
	if start < 0 {
		t.Fatal("找不到 revokeSubscriptionAccounts")
	}
	rest := src[start:]
	// 截到下一个顶层函数
	if next := strings.Index(rest[1:], "\nfunc "); next > 0 {
		rest = rest[:next+1]
	}

	if !strings.Contains(rest, "isUserNotFound") {
		t.Fatal("revokeSubscriptionAccounts 必须把「节点说用户不存在」当作成功：" +
			"否则 node_accounts 永远清不掉，该行会一直被判为收回失败并反复重试")
	}
	if !strings.Contains(rest, "isUserNotFound(err)") {
		t.Fatal("应调用 isUserNotFound(err) 做判据（与 orphan.go 共用同一份实现）")
	}
}

func clipForGapTest(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
