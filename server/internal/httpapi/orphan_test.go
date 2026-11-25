package httpapi

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"nodeops/server/internal/secrets"
)

// 这组测试钉住 2026-10-02 补的「孤儿节点账号」机制。
//
// 它修的是一个**死局**（见迁移 023 的注释）：
//
//	deleteNode 回收失败时，实现已经做对两件事 —— 保留 node_accounts 映射、
//	返回 207 明确报错。但随后把 hysteria_nodes.api_url/api_key 抹成 NULL
//	（安全上是对的）。三者合起来：映射留着，**却没有钥匙**，
//	于是「重试」物理上不可能，那句"需手动到节点清理"也缺了最关键的信息
//	（该连哪个地址、用哪把 key、删哪个 uid）。
//
// 实测证据：清理订阅残留时发现已软删的「悉尼-AWS」上还挂着 sub_00000001，
// 而该节点的 api_url/api_key 早已是 NULL，无法自动补删。
//
// 断言方式沿用本文件既有的「读源码」风格，理由见 revoke_honesty_test.go 开头。

// ---------------------------------------------------------------------------
// 1. 登记必须发生，且必须发生在"能拿到凭据"的时机
// ---------------------------------------------------------------------------

func TestDeleteNodeRecordsOrphansWithCredentialSnapshot(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "manage.go", "func (s *Server) deleteNode(")

	recordAt := strings.Index(body, "s.recordOrphanAccounts(")
	if recordAt < 0 {
		t.Fatal("deleteNode 回收失败时必须调用 recordOrphanAccounts —— " +
			"否则凭据随删除被抹掉后，那批账号永远补删不了")
	}
	// ⚠️ 这里必须断言「调用被 len(outcome.Failed) > 0 这个**守卫**包住」，
	// 而不能只断言"文件里出现过这行字"。
	// 第一版就是后者 —— 注入验证时把守卫改成 `if false {` 测试照样绿，
	// 因为它只检查字符串存在（见 AGENTS.md 铁律⑫：新写的测试必须先验证它会红）。
	// 改成：守卫必须**紧邻**在调用之前，且中间不夹别的语句。
	guard := "if len(outcome.Failed) > 0 {"
	guardAt := strings.LastIndex(body[:recordAt], guard)
	if guardAt < 0 {
		t.Error("recordOrphanAccounts 必须被 `if len(outcome.Failed) > 0` 守卫包住；" +
			"无条件调用会往表里灌无效记录，而写死 false 则等于没登记")
	} else if strings.TrimSpace(body[guardAt+len(guard):recordAt]) != "" {
		t.Error("守卫与 recordOrphanAccounts 调用之间夹了其它语句，结构可能已被改动")
	}

	// 节点名/地址的读取必须早于软删除，否则 api_url 已经是 NULL。
	nameReadAt := strings.Index(body, "SELECT name, COALESCE(api_url,'') FROM hysteria_nodes")
	softDeleteAt := strings.Index(body, "UPDATE hysteria_nodes SET deleted_at=now()")
	if nameReadAt < 0 {
		t.Fatal("deleteNode 必须先读出节点名与地址 —— 否则孤儿登记表里只有一串 uuid，运维不知道是哪台机器")
	}
	if softDeleteAt < 0 {
		t.Fatal("软删除语句不见了，测试需要跟着更新")
	}
	if !(nameReadAt < softDeleteAt) {
		t.Error("节点名/地址的读取必须早于软删除：抹除之后 api_url 就是 NULL 了")
	}
	// 那一句必须真的被执行 —— 夹在 `if false {}` 里就等于没读。
	// ⚠️ 判定区间要**往前**取一点：`QueryRow(` 在 SQL 字符串**之前**，
	// 只从 SQL 串开始截会永远找不到它（第一版就是这么写错的）。
	probeFrom := nameReadAt
	if qr := strings.LastIndex(body[:nameReadAt], "QueryRow("); qr >= 0 {
		probeFrom = qr
	}
	if !strings.Contains(body[probeFrom:softDeleteAt], "QueryRow(") {
		t.Error("节点名/地址的查询没有真正执行（QueryRow 不见了，可能被塞进了不会走到的分支）")
	}
}

// ---------------------------------------------------------------------------
// 2. 凭据必须加密后入库，绝不落明文
// ---------------------------------------------------------------------------

func TestOrphanCredentialIsEncryptedNotPlaintext(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "manage.go", "func (s *Server) recordOrphanAccounts(")

	if !strings.Contains(body, "s.box.Seal(") {
		t.Error("凭据快照必须经 secrets.Box 加密后入库（与 hysteria_nodes.api_key 同一套）")
	}
	// 把明文 key 直接写进 SQL 参数是这条防线最容易破的地方。
	if strings.Contains(body, "snapshot.APIKey,") && !strings.Contains(body, "string(plain)") {
		// recordOrphanAccounts 里不应该出现把 APIKey 直接当参数传的形状
		if strings.Contains(body, "nodeID, nodeName, nodeAPIURL, f.UserID, subID, snapshot.APIKey") {
			t.Error("凭据明文被直接写进 SQL —— 必须只写加密后的密文")
		}
	}
	// 反向确认：写库用的是 cipher 这个变量。
	if !strings.Contains(body, "cipher") {
		t.Error("未见到 cipher 变量 —— 看不出来写的是密文")
	}
}

// ---------------------------------------------------------------------------
// 3. 列表接口不得回显密钥
// ---------------------------------------------------------------------------

func TestListOrphansNeverSelectsCredential(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "orphan.go", "func (s *Server) listOrphans(")

	// 允许 `(api_key_cipher IS NOT NULL)` 这种"只问在不在"的用法，
	// 但**不允许**把 api_key_cipher 本身 SELECT 出来。
	if !strings.Contains(body, "(api_key_cipher IS NOT NULL)") {
		t.Error("列表应当只取「有没有留存凭据」这个布尔，供前端区分可自动补删与只能人工上机")
	}
	for _, bad := range []string{
		"SELECT api_key_cipher",
		"api_key_cipher, ",
		", api_key_cipher",
		"COALESCE(api_key_cipher, ''::bytea)",
	} {
		if strings.Contains(body, bad) {
			t.Errorf("列表接口里出现了取出凭据明/密文的写法 %q —— 它没有理由离开数据库", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. 补删的语义：区分"删掉了"与"本来就没有"
// ---------------------------------------------------------------------------

// 补删与「回收循环」的语义**故意不同**，这条测试把差别钉住：
//   - 回收循环要遍历全部节点，因为不知道账号在哪一台，
//     "这台没有"必须继续往后走（铁律⑮）。
//   - 补删针对的是**确定的那一台节点上的那一个 uid**，
//     所以"用户不存在"与"删掉了"对结果是等价的：账号确实没了。
//     如果补删也照搬回收循环的"继续往后走"，就永远不会结束。
func TestRetryOrphanTreatsUserNotFoundAsResolved(t *testing.T) {
	t.Parallel()

	body := funcBlock(t, "orphan.go", "func (s *Server) retryOrphan(")

	if !strings.Contains(body, "isUserNotFound(callErr)") {
		t.Fatal("补删必须把「节点返回用户不存在」判为已解决 —— 否则这条记录会永远卡在待补删列表里")
	}
	// 判为已解决时必须**写回状态**，不能只是返回 200。
	if !strings.Contains(body, "markResolved(") {
		t.Error("判为已解决时必须调用 markResolved 落库，否则下次刷新它还在列表里")
	}
	// 失败路径同样要留痕（attempts + reason），供运维判断是否反复失败。
	if !strings.Contains(body, "markAttempt(") {
		t.Error("失败路径要记 attempts 与 reason，否则运维看不出这条是不是反复失败")
	}
}

// 「用户不存在」的判据必须是精确的 user not found，不能是宽泛的 not found ——
// 后者会把 "API endpoint not found"（老节点没这个端点）也误判成"已清理"，
// 于是账号明明还在节点上、却被标成已解决。
func TestIsUserNotFoundIsPrecise(t *testing.T) {
	t.Parallel()

	cases := []struct {
		msg  string
		want bool
	}{
		{"节点 API 报错: User not found", true},
		{"节点 API 报错: user not found", true},
		{"节点 API 报错: USER NOT FOUND", true},
		// 这些**不能**被判成"已清理"：
		{"节点 API 报错: API endpoint not found", false},
		{"连接节点 API 失败: dial tcp: i/o timeout", false},
		{"api_key 不被接受 (401)", false},
	}
	for _, c := range cases {
		got := isUserNotFound(errString(c.msg))
		if got != c.want {
			t.Errorf("isUserNotFound(%q) = %v，期望 %v", c.msg, got, c.want)
		}
	}
	if isUserNotFound(nil) {
		t.Error("isUserNotFound(nil) 必须为 false")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// 2026-11-06：同一账号再次登记时若没有凭据快照，留存的地址不能被覆盖（地址与密钥要成对）。
func TestRecordOrphanKeepsAddressWithItsKey(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../../migrations/023_orphan_node_accounts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s := fixServer(db)
	s.box = box
	failed := []revokeFailure{{UserID: "sub_x", Reason: "unreachable"}}
	s.recordOrphanAccounts(ctx, fixNode, "n", "https://8.8.8.8:8443", &portalTarget{BaseURL: "https://8.8.8.8:8443", APIKey: "key"}, failed)
	s.recordOrphanAccounts(ctx, fixNode, "n", "", nil, failed)
	var url string
	var hasKey bool
	if err := db.QueryRow(ctx, `SELECT node_api_url, api_key_cipher IS NOT NULL FROM orphan_node_accounts WHERE user_id='sub_x'`).Scan(&url, &hasKey); err != nil {
		t.Fatal(err)
	}
	if url != "https://8.8.8.8:8443" || !hasKey {
		t.Fatalf("a re-registration without a snapshot must keep the stored address and key, got url=%q key=%v", url, hasKey)
	}
}
