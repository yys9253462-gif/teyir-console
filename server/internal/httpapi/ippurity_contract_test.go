package httpapi

// ippurity_contract_test.go — 把「IP 纯净度」的三条契约钉死为源码级检查。
//
// 为什么需要（与 store_contract_test.go 同一思路，不依赖数据库）
// ------------------------------------------------------------
// 这个功能横跨四层，任何一层漏改都不会让 go build 失败，只会在运行时表现成
// 「界面空白 / 接口 500 / 检测结果永远是空的」这类难查的现象：
//
//   1. 路由：server.go 注册路径 → 前端 api.ts 请求的路径必须一致。
//   2. 迁移列：034 建的表列名 → ippurity.go 的 INSERT/SELECT 必须一致。
//      加列/改名同样没有编译期保护（pgx 只在运行时告诉你列不存在）。
//   3. 落库策略：失败不得覆盖成功结果 —— 这是本功能最容易写错、又最难被
//      发现的地方（一次抖动就把所有节点的历史判定清空，看起来像"还没测过"）。
//
// 这里做静态检查，跑 go test 就红，而不是等上线。

import (
	"strings"
	"testing"
)

// 1. 三个端点必须注册，且路径与前端 api.ts 对齐。
func TestIPPurityRoutesRegistered(t *testing.T) {
	src := readFileForTest(t, "server.go")
	want := []string{
		"GET /api/nodes/{id}/ip-purity",
		"POST /api/nodes/{id}/ip-purity/refresh",
		"POST /api/ip-purity/refresh-all",
	}
	for _, route := range want {
		if !strings.Contains(src, `"`+route+`"`) {
			t.Errorf("路由 %q 未注册（前端会 404）", route)
		}
	}
}

// 2. ippurity.go 的 SQL 列必须与迁移 034 的列一一对应。
func TestIPPurityColumnsMatchMigration(t *testing.T) {
	handler := readFileForTest(t, "ippurity.go")
	migration := readFileForTest(t, "../../migrations/034_node_ip_purity.sql")

	// 判定列：INSERT 与 SELECT 两侧都要出现。
	columns := []string{
		"connection_type", "is_dual_isp", "fraud_score",
		"is_proxy", "is_vpn", "is_abuser", "asn", "asn_org", "source", "checked_at", "error",
	}
	for _, col := range columns {
		if !strings.Contains(migration, col) {
			t.Errorf("迁移 034 缺少列 %q", col)
		}
		if !strings.Contains(handler, col) {
			t.Errorf("ippurity.go 未引用列 %q（迁移与代码已漂移）", col)
		}
	}
}

// 3. 落库策略：INSERT ... ON CONFLICT 的 DO UPDATE 必须保留 checked_at，
// 而失败路径（recordPurityError）只允许写 error —— 绝不能把 checked_at 或
// 判定字段清掉。这里查关键语句的存在性，防止有人"顺手"改成全量覆盖。
func TestIPPurityFailureDoesNotClobberResult(t *testing.T) {
	handler := readFileForTest(t, "ippurity.go")

	if !strings.Contains(handler, "recordPurityError") {
		t.Fatal("缺少 recordPurityError：失败路径必须与成功路径分开")
	}
	// 失败路径的 UPDATE 子句只应写 error 与 updated_at。
	body := funcBodyForTest(handler, "func (s *Server) recordPurityError")
	if body == "" {
		t.Fatal("找不到 recordPurityError 定义")
	}
	if strings.Contains(body, "checked_at") {
		t.Error("失败路径不应触碰 checked_at（会把上次成功时间抹掉）")
	}
	if strings.Contains(body, "fraud_score") || strings.Contains(body, "connection_type") {
		t.Error("失败路径不应触碰判定字段（一次抖动会清空历史结果）")
	}
}

// 4. 纯净度是"附加能力"：未配置时必须允许 Resolver 为 nil，
// 且 handler 对此返回明确说明而不是 panic。这条钉住与 geo 一致的容错姿态。
func TestIPPurityToleratesDisabledResolver(t *testing.T) {
	handler := readFileForTest(t, "ippurity.go")
	if !strings.Contains(handler, "s.purity == nil") {
		t.Error("handler 必须处理 purity 为 nil（未配置）的情况，否则会 nil panic")
	}
}

// 5. ⭐ 专用「IP 纯净度」页面依赖的全量快照端点必须注册。
// 这个端点服务于「一次拉所有节点」的表格页 —— 少了它，页面会整体空白，
// 而不是单个字段缺失，属于一上线就看得见、但很容易被改坏的路径。
func TestIPPurityListEndpointRegistered(t *testing.T) {
	src := readFileForTest(t, "server.go")
	if !strings.Contains(src, `"GET /api/ip-purity"`) {
		t.Error("全量快照端点 GET /api/ip-purity 未注册（专用页面会空白）")
	}
	handler := readFileForTest(t, "ippurity.go")
	if !strings.Contains(handler, "func (s *Server) listIPPurity") {
		t.Error("缺少 listIPPurity handler")
	}
	// 全量读取必须走读限流，且**不得**触发第三方查询（只查库）。
	body := funcBodyForTest(handler, "func (s *Server) listIPPurity")
	if !strings.Contains(body, "enforceReadLimit") {
		t.Error("listIPPurity 必须走 readLimiter（全量接口不设限流会被刷）")
	}
	if strings.Contains(body, "checkAndStorePurity") || strings.Contains(body, "s.purity.Lookup") {
		t.Error("listIPPurity 不得联网检测 —— 进页面就该只读缓存，否则刷爆第三方配额")
	}
}

func TestIPPurityBatchEnumerationErrorsAreReported(t *testing.T) {
	handler := readFileForTest(t, "ippurity.go")
	body := funcBodyForTest(handler, "func (s *Server) refreshAllIPPurity")
	for _, want := range []string{
		"if err := rows.Scan(&id); err != nil",
		"if err := rows.Err(); err != nil",
		"s.databaseError(w, r, err)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("批量纯净度刷新必须报告不完整的节点列表读取：缺少 %q", want)
		}
	}
}

// funcBodyForTest 从源码里截出某个顶层函数的函数体（含签名行）。
// 截断点是"下一个行首 func "之前的最后一个非注释块 —— 顶层函数前面常有
// 紧贴的文档注释，那些注释属于下一个函数，不能算进当前函数体。
func funcBodyForTest(src, signature string) string {
	idx := strings.Index(src, signature)
	if idx < 0 {
		return ""
	}
	rest := src[idx+len(signature):]
	// 找下一个顶层函数定义（行首 "\nfunc "）。
	end := len(rest)
	if next := strings.Index(rest, "\nfunc "); next >= 0 {
		end = next
	}
	body := rest[:end]
	// 丢掉尾部属于下一函数的注释块：从末尾往前，去掉连续的注释行。
	lines := strings.Split(body, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || strings.HasPrefix(last, "//") {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}
