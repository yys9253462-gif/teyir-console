package httpapi

// 健康闸门纳入主机级告警（healthgate.go，2026-10-24）的回归测试。
//
// 背景：2026-10-23 美国-阿什本节点宕机事件。
// 主机整机失联数小时，而订阅输出**照旧下发这台主机上的节点** ——
// 客户拿到配置却连不上，而面板自己也不知道该隐藏它们。
//
// 根因：queryUnhealthyNodes 原来只查 node_offline / node_blocked，
// 且 JOIN 条件 `n.id = a.node_id` 对主机级告警（node_id 为空）根本匹配不上。
//
// 这一组最关键的是**不许误伤**：自动下线会直接影响客户拿到的订阅内容，
// 把正常节点下线是生产事故级。所以边界断言比正向断言更重要。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestHealthGateIncludesServerLevelAlerts 断言主机级告警被纳入查询。
//
// 反向验证要点：若漏掉 server_offline，主机整机宕机时订阅照旧下发死节点 ——
// 这正是 2026-10-23 的原始问题。
func TestHealthGateIncludesServerLevelAlerts(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "healthgate.go"))

	// ⚠️ 不能只查 "server_offline" 这个字符串出现过 ——
	// 它在 describeAlert 等处也有，注入缺陷后测试照样全绿（2026-10-24 反向验证实测）。
	// 必须精确断言**健康闸门那条 SQL 的 WHERE 子句**里含这两个类型。
	sql := extractHealthGateSQL(t, code)
	for _, typ := range []string{"server_offline", "server_stale"} {
		if !strings.Contains(sql, typ) {
			t.Errorf("健康闸门 SQL 里缺少 %q —— "+
				"主机整机失联时，它上面的节点对客户就是死节点，必须一起隐藏", typ)
		}
	}
}

// extractHealthGateSQL 从 healthgate.go 里取出健康闸门查询的 SQL 文本。
func extractHealthGateSQL(t *testing.T, code string) string {
	t.Helper()
	start := strings.Index(code, "WITH bad AS (")
	if start < 0 {
		t.Fatal("healthgate.go 里找不到健康闸门的查询（WITH bad AS）")
	}
	rest := code[start:]
	end := strings.Index(rest, "autoHideAfter.Seconds()")
	if end < 0 {
		t.Fatal("健康闸门查询未正常结束")
	}
	return rest[:end]
}

// TestHealthGateServerJoinUsesServerID 断言主机级分支通过 server_id 关联节点。
//
// ⚠️ 关键：主机级告警的 node_id 为 NULL（它针对的是"机器"不是"节点"）。
// 若 JOIN 仍写 `n.id = a.node_id`，主机级告警永远匹配不到任何行 ——
// 加了类型也没用。这是最容易"看着加了其实没生效"的写法。
func TestHealthGateServerJoinUsesServerID(t *testing.T) {
	t.Parallel()

	sql := extractHealthGateSQL(t, stripGoComments(readSourceFile(t, "healthgate.go")))

	if !strings.Contains(sql, "n.server_id = a.server_id") {
		t.Error("主机级分支必须用 `n.server_id = a.server_id` 关联节点 —— " +
			"主机级告警的 node_id 为 NULL，用 `n.id = a.node_id` 永远匹配不到")
	}
	if !strings.Contains(sql, "a.node_id IS NULL") {
		t.Error("主机级分支必须带 `a.node_id IS NULL` —— " +
			"否则同时带 node_id 和 server_id 的告警会被两支重复计入")
	}
}

// TestHealthGateKeepsNodeLevelBranch 断言节点级分支没被改坏。
//
// 主机级纳入之后，节点级（端口不通/被墙）必须照旧生效 ——
// 它有更保守的 5 分钟门槛，因为端口抖动很常见。
func TestHealthGateKeepsNodeLevelBranch(t *testing.T) {
	t.Parallel()

	sql := extractHealthGateSQL(t, stripGoComments(readSourceFile(t, "healthgate.go")))

	for _, typ := range []string{"node_offline", "node_blocked"} {
		if !strings.Contains(sql, typ) {
			t.Errorf("健康闸门 SQL 的节点级分支里缺少 %q —— "+
				"主机级纳入之后，节点级判定必须照旧生效", typ)
		}
	}
	if !strings.Contains(sql, "n.id = a.node_id AND n.deleted_at IS NULL") {
		t.Error("健康闸门 SQL 的节点级分支必须保留 `n.id = a.node_id AND n.deleted_at IS NULL` —— " +
			"软删的节点不在订阅里，不需要也不该参与健康判定")
	}
}

// TestHealthGateServerThresholdIsShorter 断言主机级门槛短于节点级。
//
// 整机 SSH 全断不会在 5 分钟内自愈，而端口不通可能是抖动。
// 两者用同一门槛就等于把确定的故障也按不确定的处理。
func TestHealthGateServerThresholdIsShorter(t *testing.T) {
	t.Parallel()

	if autoHideServerAfter >= autoHideAfter {
		t.Errorf("主机级门槛（%v）应短于节点级（%v）—— "+
			"整机失联基本不会自愈，而端口不通可能只是抖动",
			autoHideServerAfter, autoHideAfter)
	}
	if autoHideServerAfter > 5*time.Minute {
		t.Errorf("主机级门槛不应超过 5 分钟，实得 %v", autoHideServerAfter)
	}
}

// TestHealthGateServerThresholdIsUsedInQuery 断言新门槛真的传进了 SQL。
//
// 只定义了常量但没传进去，等于没分级 —— 这是"看起来做了其实没做"。
func TestHealthGateServerThresholdIsUsedInQuery(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "healthgate.go"))
	sql := extractHealthGateSQL(t, code)

	if !strings.Contains(code, "autoHideServerAfter.Seconds()") {
		t.Error("autoHideServerAfter 没有传进 SQL —— " +
			"定义了常量却不用，等于没有分级")
	}
	// 三个占位参数都要传：节点级门槛、恢复观察期、主机级门槛
	if !strings.Contains(code, "autoHideAfter.Seconds(), autoHideRecoverHold.Seconds(), autoHideServerAfter.Seconds()") {
		t.Error("SQL 的三个参数必须按 (节点级门槛, 恢复观察期, 主机级门槛) 的顺序传入")
	}
	// 参数位必须真的分开用：节点级用 $1，主机级用 $3。
	// 如果两支都用 $1（或都用 $3），门槛就是同一个 —— 分级等于没做。
	if !strings.Contains(sql, "$1::float AS grace") {
		t.Error("节点级分支必须使用 $1 作为门槛参数")
	}
	if !strings.Contains(sql, "$3::float AS grace") {
		t.Error("主机级分支必须使用 $3 作为门槛参数 —— " +
			"若与节点级共用 $1，分级形同虚设")
	}
}

// TestHealthGateReasonMentionsServerCause 断言主机级原因文案存在。
//
// 运维在「自动下线」页面看到原因时，必须知道是"整机不可达"而不是
// 含糊的"节点端口不通" —— 两者的处理动作完全不同（换机器 vs 查节点）。
func TestHealthGateReasonMentionsServerCause(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "healthgate.go"))

	for _, want := range []string{"所在主机整机不可达", "所在主机采集停滞"} {
		if !strings.Contains(code, want) {
			t.Errorf("缺少主机级原因文案 %q —— "+
				"运维需要区分「节点端口不通」与「整机不可达」：前者查节点，后者查机器", want)
		}
	}
}

// TestSSHAuthFailureDoesNotHideNodes：SSH 凭据失效（节点 API 仍通）不能触发
// 健康闸门要用的 server_stale —— 否则 3 分钟后这台主机上正常工作的节点
// 全部从客户订阅里消失（2026-11-03）。凭据失效只给 warning，且不进闸门。
func TestSSHAuthFailureDoesNotHideNodes(t *testing.T) {
	t.Parallel()
	code := stripGoComments(readSourceFile(t, "healthgate.go"))
	if strings.Contains(extractHealthGateSQL(t, code), "server_ssh_broken") {
		t.Fatal("server_ssh_broken 不能进健康闸门：节点 API 是通的，下线它们是误伤")
	}
	body := funcBlock(t, "manage.go", "func (s *Server) ReconcileAlerts(")
	// 2026-11-06：采集器 sshDialFailureReason 写入的三种 SSH 失败前缀都要算 ssh_broken（API 回退后主机照样 online）。
	collectorSrc := readFileForTest(t, "../collector/collector.go")
	for _, prefix := range []string{"SSH 凭据失效", "SSH 连接失败", "SSH 主机指纹"} {
		if !strings.Contains(collectorSrc, `"`+prefix) {
			t.Fatalf("采集器不再写「%s」前缀，请同步更新 ssh_broken 判据", prefix)
		}
		if !strings.Contains(body, "LIKE '"+prefix+"%'") {
			t.Fatalf("ReconcileAlerts 的 ssh_broken 必须认出「%s」", prefix)
		}
	}
	broken := strings.Index(body, `alertKey{"server_ssh_broken", row.id, ""}, "warning"`)
	stale := strings.Index(body, `alertKey{"server_stale", row.id, ""}, "critical"`)
	if broken < 0 || stale < 0 {
		t.Fatal("expected server_ssh_broken (warning) and server_stale (critical) conditions")
	}
	if !strings.Contains(body[broken:stale], "} else if") {
		t.Fatal("server_stale 必须在 SSH 凭据失效时被排除（else 分支）")
	}
}

// 2026-11-06：流量超额被停用的节点要被健康闸门拿掉（只接 API 的节点探测看门户，一直是 online）。
func TestHealthGateHidesTrafficSuspendedNodes(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz);
INSERT INTO hysteria_nodes(id,name,server_id,traffic_suspended,traffic_suspended_at) VALUES('`+fixNode+`','suspended','`+fixHost+`',true,now())`); err != nil {
		t.Fatal(err)
	}
	bad, err := fixServer(db).queryUnhealthyNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := bad[fixNode]; !ok || !strings.Contains(h.Reason, "流量超额") {
		t.Fatalf("a traffic-suspended node must be hidden with a reason, got %v", bad)
	}
	if _, err := db.Exec(ctx, `UPDATE hysteria_nodes SET traffic_suspended=false`); err != nil {
		t.Fatal(err)
	}
	if bad, err := fixServer(db).queryUnhealthyNodes(ctx); err != nil || len(bad) != 0 {
		t.Fatalf("a resumed node must come back, got %v %v", bad, err)
	}
}
