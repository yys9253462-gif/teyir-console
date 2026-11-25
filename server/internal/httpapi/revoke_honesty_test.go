package httpapi

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 这组测试针对 2026-10-02 验收时抓到的真实缺陷：
//
//	deleteNode 先执行 `UPDATE hysteria_nodes SET deleted_at=now(), api_url=NULL, api_key=NULL`，
//	之后才调用 revokeNodeFromAllSubscriptions；而 loadPortalTarget 的 SQL 带
//	`AND deleted_at IS NULL`，所以回收时必然拿到 errPortalNodeNotFound，
//	/users/delete 一个都发不出去。
//
//	叠加第二层静默失败：revokeNodeFromAllSubscriptions 在 portalCall 报错时只记一条
//	Warn 日志，然后照样把 node_accounts 里的映射删掉、照样记「已成功回收」。
//
//	合起来的后果正是全局铁律①⑮描述的那种坏法：**账面已回收，账号还在节点上能连。**
//
// 这里用「读源码断言」的方式钉住顺序与语义。理由：Server.db 是具体的 *pgxpool.Pool，
// 没有可注入的接口，为这次修复引入 sqlmock/mockery 属于超出问题范围的重构。
// 真正端到端的验证在真机完成（2026-10-02，见 docs/acceptance-in-store-20261002.md）。
// 读源码的断言虽然朴素，但它对「顺序倒置」这类回归是有效的 —— 只要有人把两行换回去，
// 测试立刻红。

// sourceFile 在包根定位一个源文件。测试以包的 cwd（server/internal/httpapi）为基准。
func sourceFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(".", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("找不到源文件 %s：%v", name, err)
	}
	return path
}

// funcBlock 截出从 `func <sig>` 开始的那一段函数体，按 `func ` 出现的位置配对花括号。
func funcBlock(t *testing.T, file, sig string) string {
	t.Helper()
	raw, err := os.ReadFile(sourceFile(t, file))
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", file, err)
	}
	src := string(raw)
	start := strings.Index(src, sig)
	if start < 0 {
		t.Fatalf("在 %s 里找不到 %s", file, sig)
	}
	// 从 sig 起找到第一个左花括号，再配对。
	open := strings.Index(src[start:], "{")
	if open < 0 {
		t.Fatalf("%s 在 %s 起始处没有函数体", sig, file)
	}
	depth, i := 0, start+open
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("%s 的函数体没有闭合", sig)
	return ""
}

// ---------------------------------------------------------------------------
// 1. 顺序：凭据快照必须在删除之前取
// ---------------------------------------------------------------------------

func TestDeleteNodeTakesCredentialSnapshotBeforeSoftDelete(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "manage.go", "func (s *Server) deleteNode(")

	snapshotAt := strings.Index(body, "s.loadPortalTarget(r.Context(), nodeID)")
	softDeleteAt := strings.Index(body, "UPDATE hysteria_nodes SET deleted_at=now()")
	revokeAt := strings.Index(body, "s.revokeNodeFromAllSubscriptions(")

	if snapshotAt < 0 {
		t.Fatal("deleteNode 必须先用 loadPortalTarget 取凭据快照")
	}
	if softDeleteAt < 0 {
		t.Fatal("deleteNode 里的软删除语句不见了，测试需要跟着更新")
	}
	if revokeAt < 0 {
		t.Fatal("deleteNode 不再调用回收函数，买家账号会被留在节点上")
	}
	if !(snapshotAt < softDeleteAt) {
		t.Error("凭据快照必须早于软删除：deleted_at 置上后 loadPortalTarget 带 " +
			"`AND deleted_at IS NULL`，必然返回 errPortalNodeNotFound，" +
			"回收会静默变成「账面成功、账号仍在节点上」")
	}
	if !(revokeAt < softDeleteAt) {
		t.Error("必须先完整读取订阅并尝试回收，再软删除节点；清单读取失败时需保留凭据供重试")
	}
}

// TestDeleteNodeClearsStatus 守住 2026-10-24 审计 P1-4 附的修复：
// 软删除时**必须**把 status 置成 offline。
//
// 为什么值得单独一条：这个缺陷不留日志、不报错，只是让已删节点继续显示
// "online"。实测库里 7 台已删节点仍是 online，而采集器按
// `deleted_at IS NULL` 取目标，永远不会再写它们 —— 这个错误的 status
// 会永久冻结，不会自愈。除了直接改库，没有别的手段能发现它。
func TestDeleteNodeClearsStatus(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "manage.go", "func (s *Server) deleteNode(")
	at := strings.Index(body, "UPDATE hysteria_nodes SET deleted_at=now()")
	if at < 0 {
		t.Fatal("找不到软删除语句")
	}
	// 只看这一条语句本身（到闭合反引号为止），避免把后面的语句算进来。
	end := strings.Index(body[at:], "`")
	if end < 0 {
		t.Fatal("软删除语句未闭合")
	}
	stmt := body[at : at+end]
	if !strings.Contains(stmt, "status='offline'") {
		t.Fatal("软删除必须同时把 status 置为 offline，否则已删节点会永久显示在线")
	}
	// 凭据抹除不能顺手删掉 —— 那是同一条语句里既有且必要的行为。
	for _, want := range []string{"api_url=NULL", "api_key=NULL"} {
		if !strings.Contains(stmt, want) {
			t.Fatalf("软删除仍需抹除凭据（缺 %s）：那把 key 能读出节点口令", want)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 语义：回收失败不得删除映射
// ---------------------------------------------------------------------------

func TestRevokeKeepsMappingWhenNodeCallFails(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "store.go", "func (s *Server) revokeNodeFromSubscriptions(")

	// 真实删除失败必须 continue（保留映射）；精确的 User not found 则视为幂等成功。
	if !strings.Contains(body, "!isUserNotFound(err)") {
		t.Error("节点明确返回 User not found 时，回收应幂等清除映射；其它错误仍需保留映射重试")
	}
	if !strings.Contains(body, "failed = append(failed, revokeFailure") {
		t.Error("节点删除失败必须记录为失败，不能静默跳过或清除映射")
	}

	// 映射的删除必须发生在调用成功之后。
	callAt := strings.Index(body, `"/users/delete"`)
	deleteAt := strings.Index(body, "node_accounts=node_accounts - $1")
	if callAt < 0 || deleteAt < 0 {
		t.Fatal("回收函数里应同时存在 /users/delete 调用与映射删除")
	}
	if !(callAt < deleteAt) {
		t.Error("必须先成功调用节点侧删除，再删本地映射")
	}

	// 写回失败也必须算失败，不能悄悄吞掉。
	if !strings.Contains(body, "写回映射失败") {
		t.Error("写回 node_accounts 失败必须记入 outcome.Failed，不能被 `_ =` 吞掉")
	}
}

// ---------------------------------------------------------------------------
// 3. 诚实：返回值不得无条件宣称成功
// ---------------------------------------------------------------------------

func TestRevokeOutcomeIsHonest(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "store.go", "func (s *Server) revokeNodeFromSubscriptions(")

	// 「已成功」那条日志只能在没有失败时打印。
	successLog := regexp.MustCompile(`(?s)if len\(failed\) == 0 \{.*?已成功从买家订阅中移出该节点并注销账号`)
	if !successLog.MatchString(body) {
		t.Error("「已成功回收」的日志必须受 `if len(failed) == 0` 保护；" +
			"无条件打印等同于谎报成功（全局铁律①）")
	}

	// 旧代码用 len(toRevoke) 报成功数，那包含失败的账号，是错的。
	if regexp.MustCompile(`"revoked_count", len\(toRevoke\)`).MatchString(body) {
		t.Error("revoked_count 必须统计真正成功的条数，不能用待处理总数 len(toRevoke)")
	}
}

func TestRevokeOutcomeOKRequiresNoFailures(t *testing.T) {
	t.Parallel()

	clean := revokeOutcome{Revoked: 2}
	if !clean.OK() {
		t.Error("没有失败、没有错误时必须判定为成功")
	}

	withFailure := revokeOutcome{Revoked: 1, Failed: []revokeFailure{{UserID: "sub_x", Reason: "限流"}}}
	if withFailure.OK() {
		t.Error("存在失败账号时绝不能判定为成功")
	}

	withDBErr := revokeOutcome{Err: errors.New("查询失败")}
	if withDBErr.OK() {
		t.Error("数据库出错时绝不能判定为成功")
	}

	if got := withFailure.FailedSummary(); got == "" {
		t.Error("失败时必须给出可展示给操作者的文案")
	} else if !strings.Contains(got, "sub_x") {
		t.Errorf("失败文案应带上出问题的 user_id，实际 %q", got)
	}
	if got := clean.FailedSummary(); got != "" {
		t.Errorf("成功时不应有失败文案，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 4. 覆盖面：下架与删除两条路都必须能把失败暴露给操作者
// ---------------------------------------------------------------------------

func TestBothRevokePathsSurfaceFailures(t *testing.T) {
	t.Parallel()

	del := funcBlock(t, "manage.go", "func (s *Server) deleteNode(")
	if !strings.Contains(del, "http.StatusMultiStatus") {
		t.Error("删除节点时若有账号未注销成功，必须用非 200 状态码告知操作者")
	}
	if !strings.Contains(del, "len(outcome.Failed) > 0") {
		t.Error("deleteNode 必须检查 outcome.Failed")
	}

	upd := funcBlock(t, "admin.go", "func (s *Server) updateNode(")
	if !strings.Contains(upd, "http.StatusMultiStatus") {
		t.Error("节点移出商城时若有账号未注销成功，必须用非 200 状态码告知操作者")
	}
	if !strings.Contains(upd, "len(outcome.Failed) > 0") {
		t.Error("updateNode 必须在 closingStore 分支检查 outcome.Failed")
	}
}

// ---------------------------------------------------------------------------
// 5. 回归：签名变更后所有调用方都要跟上
// ---------------------------------------------------------------------------

func TestAllRevokeCallSitesPassThirdArgument(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"manage.go", "admin.go"} {
		body, err := os.ReadFile(sourceFile(t, file))
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", file, err)
		}
		// 旧签名是两参数调用；改成三参数后若漏改，这里会编译不过。
		// 这条断言的价值在于：一旦有人把签名改回去，它会给出可读的失败信息。
		bad := regexp.MustCompile(`revokeNodeFromAllSubscriptions\(([^()]*)\)`)
		for _, m := range bad.FindAllStringSubmatch(string(body), -1) {
			// 声明行带 `ctx context.Context, nodeID string` 属正常。
			if strings.Contains(m[0], "ctx context.Context") {
				continue
			}
			if strings.Count(m[0], ",") < 2 {
				t.Errorf("%s 里的调用 %s 只传了不足三个参数，回调签名已变为 "+
					"(ctx, nodeID, override)", file, m[0])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 6. 超时预算：回收不能无限期挂住管理端
// ---------------------------------------------------------------------------

func TestRevokeUsesBoundedTimeout(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "store.go", "func (s *Server) revokeNodeFromSubscriptions(")
	if !strings.Contains(body, "context.WithTimeout") {
		t.Fatal("节点侧删除必须带超时，否则一个不响应的节点会拖住整个管理请求")
	}
	if !strings.Contains(body, "10*time.Second") && !regexp.MustCompile(`\d+\s*\*\s*time\.Second`).MatchString(body) {
		t.Error("请给节点侧删除一个明确的有界超时")
	}
	_ = time.Second // 保持 time 导入在断言演进后仍被引用
}
