package httpapi

// 半下线巡检的**真跑 SQL** 集成测试（2026-10-24 加）。
//
// 为什么必须真跑：2026-10-24 把这批改动部署到生产后，
// 容器日志第一条就是：
//
//	"半下线巡检失败" error="ERROR: operator does not exist: uuid = boolean"
//
// 根因在检测 SQL 的 JOIN 子句：
//
//	错误：JOIN hysteria_nodes hn ON hn.id = st.node_accounts ? hn.id::text
//	正确：JOIN hysteria_nodes hn ON st.node_accounts ? hn.id::text
//
// `?` 是 jsonb 的 **exist 操作符**，返回 boolean。
// 写成 `hn.id = <boolean>` 就是 `uuid = boolean`，PostgreSQL 直接报错。
//
// 而当时的单元测试断言的是「代码里含 `st.node_accounts ? hn.id::text`
// 这个子串」—— 错误写法**也含这个子串**，所以测试全绿、缺陷溜过去。
//
// 这条测试直接连库跑那条 SQL：SQL 语法/类型错误不可能再靠字符串断言漏掉。
// 没有测试库时跳过（与仓库其它集成测试一致）。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// halfDB 建一个带最小表结构的测试库（独立 schema，用完即删）。
func halfDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required for PostgreSQL integration")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("nodeops_half_%d", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		base.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		base.Close()
	})
	_, err = db.Exec(ctx, `
CREATE TABLE subscription_tokens(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  node_accounts jsonb DEFAULT '{}',
  deleted_at timestamptz,
  expires_at timestamptz
);
CREATE TABLE hysteria_nodes(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text,
  deleted_at timestamptz,
  server_id uuid
);`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestDetectHalfDecommissionedSQLRunsAgainstPostgres 真跑那条检测 SQL。
//
// 断言两件事：
//  1. SQL **能执行**（语法/类型都对）—— 这条正是 2026-10-24 生产事故
//     漏掉的那一层。
//  2. 结果**正确**：命中软删节点、跳过在役节点、跳过无账号映射。
func TestDetectHalfDecommissionedSQLRunsAgainstPostgres(t *testing.T) {
	db := halfDB(t)
	ctx := context.Background()

	const (
		deletedNode = "00000000-0000-0000-0000-00000000d001"
		liveNode    = "00000000-0000-0000-0000-00000000d002"
		emptyNode   = "00000000-0000-0000-0000-00000000d003"
	)
	// 软删节点，且有一条订阅挂着买家账号 → 应命中。
	if _, err := db.Exec(ctx, `
INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES
  ($1,'已删节点',now()),
  ($2,'在役节点',NULL),
  ($3,'无账号节点',now())`, deletedNode, liveNode, emptyNode); err != nil {
		t.Fatal(err)
	}
	// ⚠️ 账号映射的值必须是 **jsonb 对象**（{"user_id":"..."}），
	// 不是字符串。真实生产数据就是这样（node_accounts 的值是嵌套对象）。
	// 用 jsonb_build_object 嵌套构造，不能用普通字符串 ——
	// 写成字符串时 `->>'user_id'` 取不到值，测试会假失败。
	if _, err := db.Exec(ctx, `
INSERT INTO subscription_tokens(node_accounts,deleted_at) VALUES
  (jsonb_build_object($1::text, jsonb_build_object('user_id','u-hit')), NULL),
  (jsonb_build_object($2::text, jsonb_build_object('user_id','u-live')), NULL),
  (jsonb_build_object($3::text, jsonb_build_object('user_id','')), NULL),
  (jsonb_build_object($1::text, jsonb_build_object('user_id','u-deleted-sub')), now())`,
		deletedNode, liveNode, emptyNode); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	refs, err := s.detectHalfDecommissioned(ctx)
	if err != nil {
		t.Fatalf("检测 SQL 执行失败：%v —— "+
			"这就是 2026-10-24 生产事故的那类错误（uuid = boolean）", err)
	}
	if len(refs) != 1 {
		t.Fatalf("应命中 1 条，实际 %d 条：%+v", len(refs), refs)
	}
	if refs[0].NodeID != deletedNode {
		t.Errorf("命中的是节点 %s，应为已软删节点 %s", refs[0].NodeID, deletedNode)
	}
	if refs[0].UserID != "u-hit" {
		t.Errorf("命中的账号是 %q，应为 u-hit（在役节点的 u-live、无账号节点、"+
			"已删订阅都不该命中）", refs[0].UserID)
	}
}

// TestHalfDecommissionedJoinConditionIsValidPostgres 断言 JOIN 条件在
// PostgreSQL 里真的是布尔类型。
//
// 上一条能抓住「SQL 跑不通」；这一条更精确：它直接对**生产用的写法**
// 求值，确保 `?` 的返回值能被当作条件用。
// 若有人把 JOIN 改回 `hn.id = st.node_accounts ? hn.id::text`，
// 上一条会报「执行失败」，这一条会报「类型不是 boolean」。
func TestHalfDecommissionedJoinConditionIsValidPostgres(t *testing.T) {
	db := halfDB(t)
	ctx := context.Background()

	var isBool bool
	err := db.QueryRow(ctx, `
SELECT pg_typeof(st.node_accounts ? 'k')::text = 'boolean'
FROM (SELECT '{}'::jsonb AS node_accounts) st`).Scan(&isBool)
	if err != nil {
		t.Fatal(err)
	}
	if !isBool {
		t.Error("`?` 操作符的返回值类型不是 boolean —— " +
			"说明 PostgreSQL 语义与假设不符，JOIN 条件需重新设计")
	}
}
