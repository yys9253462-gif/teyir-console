package httpapi

// expired_closeout_test.go — 到期结案出口的测试。
//
// 两条思路分开：
//   - 源码级守卫：确认路由、权限、确认头、审计都在（改坏了要红）
//   - 集成测试：真连库跑结案逻辑，确认「孤儿登记 + 摘映射 + 删行 + 清退避」
//     这四件事真的发生了，而且**没到期的不被结案**、**节点还在的不被批量结案**

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodeops/server/internal/secrets"
)

// closeoutDB 建一个带最小表结构的测试库（独立 schema，用完即删）。
func closeoutDB(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("nodeops_closeout_%d", time.Now().UnixNano())
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
  token text NOT NULL DEFAULT 'test-token',
  label text NOT NULL DEFAULT 'test-label',
  enabled boolean DEFAULT true,
  expires_at timestamptz,
  ip_limit int DEFAULT 5,
  auto_delete boolean DEFAULT true,
  order_id text NOT NULL DEFAULT '',
  user_email text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  category_id uuid,
  revenue_monthly numeric,
  revenue_currency text,
  node_ids text[],
  created_at timestamptz DEFAULT now(),
  last_accessed_at timestamptz,
  access_count bigint DEFAULT 0,
  deleted_at timestamptz,
  node_accounts jsonb DEFAULT '{}'::jsonb,
  active_ips jsonb DEFAULT '[]'::jsonb
);
CREATE TABLE hysteria_nodes(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text,
  api_url text,
  api_key bytea,
  deleted_at timestamptz,
  server_id uuid,
  status text,
  status_error text,
  users_count int,
  api_checked_at timestamptz,
  updated_at timestamptz DEFAULT now()
);
CREATE TABLE orphan_node_accounts(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  node_id uuid,
  node_name text,
  node_api_url text,
  user_id text,
  subscription_id uuid,
  api_key_cipher bytea,
  reason text,
  attempts int DEFAULT 0,
  last_attempt_at timestamptz,
  resolved_at timestamptz,
  resolved_note text,
  created_at timestamptz DEFAULT now()
);
CREATE TABLE subscription_revoke_backoff(
  subscription_id uuid,
  node_id uuid,
  attempts int DEFAULT 0,
  next_attempt_at timestamptz,
  last_error text,
  first_failed_at timestamptz,
  PRIMARY KEY(subscription_id,node_id)
);
CREATE UNIQUE INDEX idx_orphan_accounts_unique_pending
  ON orphan_node_accounts(node_id,user_id) WHERE resolved_at IS NULL;`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// 源码级守卫：路由、权限、确认头、审计。
func TestCloseExpiredRoutesAreGuarded(t *testing.T) {
	body := funcBlock(t, "expired_closeout.go", "func (s *Server) closeExpiredSubscriptionHandler(")
	for _, want := range []string{`X-Confirm-Dangerous`, "s.audit(", "s.enforceWriteLimit("} {
		if !strings.Contains(body, want) {
			t.Errorf("closeExpiredSubscriptionHandler 缺少 %q", want)
		}
	}
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"POST /api/subscriptions/{id}/close-expired", s.authenticated(s.requireAdmin(s.closeExpiredSubscriptionHandler))`,
		`"POST /api/subscriptions/close-expired-batch", s.authenticated(s.requireAdmin(s.closeExpiredSubscriptionBatchHandler))`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("路由缺失或未限管理员：%s", want)
		}
	}
}

// 结案的核心语义：已到期 + 节点已删 → 登记孤儿、摘映射、删行、清退避。
func TestCloseExpiredSubscriptionRegistersOrphanAndDeletesRow(t *testing.T) {
	db := closeoutDB(t)
	ctx := context.Background()

	const (
		goneNode   = "00000000-0000-0000-0000-00000000e001"
		liveNode   = "00000000-0000-0000-0000-00000000e002"
		expiredSub = "00000000-0000-0000-0000-00000000e010"
		liveSub    = "00000000-0000-0000-0000-00000000e011"
	)
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	liveKey, _ := box.Seal([]byte("live-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `
INSERT INTO hysteria_nodes(id,name,api_url,api_key,deleted_at) VALUES
  ($1,'已删节点','',NULL,now()),
  ($2,'在役节点','https://example.invalid:8443',$3,NULL)`, goneNode, liveNode, liveKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO subscription_tokens(id,expires_at,enabled,auto_delete,node_accounts) VALUES
  ($1, now()-interval '2 days', false, true,
     jsonb_build_object($2::text, jsonb_build_object('user_id','sub_expired'),
                        $3::text, jsonb_build_object('user_id','sub_live'))),
  ($4, now()+interval '10 days', true, true,
     jsonb_build_object($3::text, jsonb_build_object('user_id','sub_notexpired')))`,
		expiredSub, goneNode, liveNode, liveSub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO subscription_revoke_backoff(subscription_id,node_id,attempts)
VALUES ($1,$2,27)`, expiredSub, goneNode); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, box: box, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orphans, deleted, err := s.closeExpiredSubscription(ctx, expiredSub, "测试结案")
	if err != nil {
		t.Fatalf("结案失败：%v", err)
	}
	if !deleted {
		t.Fatal("已到期订阅应被删除")
	}
	if orphans != 2 {
		t.Errorf("登记孤儿数 = %d，应为 2（两个节点账号各一条）", orphans)
	}

	// 订阅行必须没了。
	var remaining int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid`, expiredSub).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Error("到期订阅行仍存在，结案没生效")
	}
	// 孤儿登记必须留痕，且指回原订阅。
	rows, err := db.Query(ctx, `SELECT node_id::text,user_id,subscription_id::text,reason FROM orphan_node_accounts ORDER BY user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var nodeID, userID, subID, reason string
		if err := rows.Scan(&nodeID, &userID, &subID, &reason); err != nil {
			t.Fatal(err)
		}
		got[userID] = nodeID + "|" + subID + "|" + reason
	}
	if len(got) != 2 {
		t.Fatalf("孤儿登记条数 = %d，应为 2：%+v", len(got), got)
	}
	if !strings.Contains(got["sub_expired"], expiredSub) {
		t.Errorf("孤儿记录未指回原订阅：%s", got["sub_expired"])
	}
	if !strings.Contains(got["sub_expired"], goneNode) {
		t.Errorf("孤儿记录的节点不对：%s", got["sub_expired"])
	}
	// 2026-11-03：在役节点的待补删记录必须带凭据快照，否则「补删」只能报「没有留存凭据」。
	var liveHasKey, goneHasKey bool
	if err := db.QueryRow(ctx, `SELECT
  COALESCE(bool_or(api_key_cipher IS NOT NULL) FILTER (WHERE user_id='sub_live'), false),
  COALESCE(bool_or(api_key_cipher IS NOT NULL) FILTER (WHERE user_id='sub_expired'), false)
FROM orphan_node_accounts`).Scan(&liveHasKey, &goneHasKey); err != nil {
		t.Fatal(err)
	}
	if !liveHasKey {
		t.Error("在役节点上的待补删账号没有留存凭据，补删将无法自动进行")
	}
	if goneHasKey {
		t.Error("已删节点不该凭空出现凭据快照")
	}
	// 退避记录必须清掉，否则这行会永远留在退避表里。
	var backoff int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_revoke_backoff WHERE subscription_id=$1::uuid`, expiredSub).Scan(&backoff); err != nil {
		t.Fatal(err)
	}
	if backoff != 0 {
		t.Errorf("回收退避记录未清理，剩 %d 条", backoff)
	}
	// 未到期的订阅必须原封不动。
	var liveEnabled bool
	var liveDeleted bool
	if err := db.QueryRow(ctx, `SELECT enabled, deleted_at IS NOT NULL FROM subscription_tokens WHERE id=$1::uuid`, liveSub).Scan(&liveEnabled, &liveDeleted); err != nil {
		t.Fatal(err)
	}
	if !liveEnabled || liveDeleted {
		t.Error("未到期订阅被误改")
	}
}

// 未到期的订阅不能被结案。
func TestCloseExpiredRefusesUnexpiredSubscription(t *testing.T) {
	db := closeoutDB(t)
	ctx := context.Background()

	const sub = "00000000-0000-0000-0000-00000000e020"
	if _, err := db.Exec(ctx, `
INSERT INTO subscription_tokens(id,expires_at,enabled,auto_delete,node_accounts) VALUES
  ($1, now()+interval '5 days', true, true,
     jsonb_build_object('00000000-0000-0000-0000-00000000e021'::text, jsonb_build_object('user_id','sub_x')))`, sub); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, _, err := s.closeExpiredSubscription(ctx, sub, ""); err == nil {
		t.Fatal("未到期订阅被结案了 —— 这是不可逆的数据损失")
	}
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE id=$1::uuid`, sub).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("未到期订阅被删了")
	}
}

// 批量结案：节点仍在的**不能**被批量结案（避免把还可能回收的账号批量放弃）。
func TestCloseExpiredBatchSkipsNodesStillAlive(t *testing.T) {
	db := closeoutDB(t)
	ctx := context.Background()

	const (
		goneNode = "00000000-0000-0000-0000-00000000e101"
		liveNode = "00000000-0000-0000-0000-00000000e102"
		goneSub  = "00000000-0000-0000-0000-00000000e110"
		liveSub  = "00000000-0000-0000-0000-00000000e111"
	)
	if _, err := db.Exec(ctx, `
INSERT INTO hysteria_nodes(id,name,api_url,api_key,deleted_at) VALUES
  ($1,'已删节点','',NULL,now()),
  ($2,'在役节点','https://example.invalid:8443','x'::bytea,NULL)`, goneNode, liveNode); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO subscription_tokens(id,expires_at,enabled,auto_delete,node_accounts) VALUES
  ($1, now()-interval '3 days', false, true,
     jsonb_build_object($2::text, jsonb_build_object('user_id','sub_gone'))),
  ($3, now()-interval '3 days', false, true,
     jsonb_build_object($4::text, jsonb_build_object('user_id','sub_live')))`,
		goneSub, goneNode, liveSub, liveNode); err != nil {
		t.Fatal(err)
	}

	s := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// 直接跑批量判据里的那段逻辑：手工调用单条结案验证"节点还在"不会被自动处理。
	accounts, err := s.loadExpiredSubscriptionAccounts(ctx, liveSub)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("应读到 1 个账号，实际 %d", len(accounts))
	}
	_, gone, _, err := s.nodeIsGone(ctx, accounts[0].NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Error("在役节点被判成已下线，批量结案会误伤还可能回收的账号")
	}
	// 已删节点应判成已下线。
	accounts, err = s.loadExpiredSubscriptionAccounts(ctx, goneSub)
	if err != nil {
		t.Fatal(err)
	}
	_, gone, _, err = s.nodeIsGone(ctx, accounts[0].NodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Error("已删节点未判成已下线 —— 该结案的没结案")
	}
}
