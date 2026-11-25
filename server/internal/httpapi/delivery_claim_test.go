package httpapi

// 同步任务的领取与落库契约（2026-10-30）。
//
// 这里钉三件在真机上出过问题、而单看代码很难发现的语义：
//
//  1. **旧领取必须跳过**。前台已经把订阅交付完成（synced），后台那条
//     早就领到的任务不能照着保存的 id 再跑一遍 —— 那会把 synced 置回
//     pending，商城据此对外报"交付尚未完成"，而货其实已经交付过了。
//  2. **已确认的成功必须落库**。节点侧改完密码之后，落库不能因为
//     "请求被取消 / 节点往返把预算耗光"而失败 —— 否则节点上换了新密码、
//     库里仍是旧密码，回收按旧映射遍历时会漏掉这个账号。
//  3. **预算必须覆盖整轮**。20 个节点、并发 4、单节点 15 秒需要 75 秒；
//     预算写死 50 秒就每轮都在最后几个节点上被切断，永远没有增量进展。

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 旧领取遇到"已被前台完成"必须跳过，且不得改写任务状态。
func TestClaimSupersededByForegroundCompletionIsSkipped(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	ctx := context.Background()

	id, lease, err := s.claimNextSubscriptionSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != fixSub || lease.IsZero() {
		t.Fatalf("领取结果异常 id=%q lease=%v", id, lease)
	}

	// 模拟前台请求在后台执行之前完成了这一单。
	if _, err = db.Exec(ctx, `UPDATE subscription_sync_jobs SET status='synced',error='' WHERE subscription_id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	// 必须给空账号：否则即使旧领取被正确跳过，synchronizeClaimedSubscription
	// 也会在最后因为 `len(accounts) == 0` 返回错误，把"正确跳过"误判成失败。
	rec.nodeAccounts = map[string]subscriptionNodeAccount{fixNode: {UserID: "sub_a", Password: "pw-a"}}
	if err = s.synchronizeClaimedSubscription(ctx, rec, &syncClaim{leasedUntil: lease}); err != nil {
		t.Fatalf("旧领取被跳过时不该报错：%v", err)
	}
	var status string
	if err = db.QueryRow(ctx, `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "synced" {
		t.Fatalf("旧领取把已交付的订阅改回了 %q —— 商城会开始报「交付尚未完成」", status)
	}
}

// 被另一实例重新领取（租约值变化）也必须跳过。
func TestClaimSupersededByAnotherInstanceIsSkipped(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	ctx := context.Background()

	_, stale, err := s.claimNextSubscriptionSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 另一个实例（或下一轮）重新领取：租约被写成新的值。
	if _, err = db.Exec(ctx, `UPDATE subscription_sync_jobs SET next_attempt=now()+interval '4 minutes' WHERE subscription_id=$1::uuid`, fixSub); err != nil {
		t.Fatal(err)
	}
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	rec.nodeAccounts = map[string]subscriptionNodeAccount{fixNode: {UserID: "sub_a", Password: "pw-a"}}
	if err = s.synchronizeClaimedSubscription(ctx, rec, &syncClaim{leasedUntil: stale}); err != nil {
		t.Fatalf("被重新领取的旧任务应静默跳过：%v", err)
	}
	var status string
	if err = db.QueryRow(ctx, `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("跳过旧领取不该改动任务状态，实际 %q", status)
	}
}

// 领取是逐条的：一轮只领一条，慢订阅不会一次性占满名额。
func TestClaimTakesOneJobPerCall(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	// 再加一条订阅，确保确实有两条可领。
	if _, err := db.Exec(context.Background(), `INSERT INTO subscription_tokens(id,token,order_id,expires_at)
VALUES('00000000-0000-0000-0000-000000000104','fixture-token-second','fixture-order-2',now()+interval '30 days')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	ctx := context.Background()
	first, _, err := s.claimNextSubscriptionSync(ctx)
	if err != nil || first == "" {
		t.Fatalf("第一次领取失败：%q %v", first, err)
	}
	second, _, err := s.claimNextSubscriptionSync(ctx)
	if err != nil || second == "" {
		t.Fatalf("第二次领取失败：%q %v", second, err)
	}
	if first == second {
		t.Fatal("两次领取拿到同一条任务 —— 租约没生效")
	}
	if third, _, err := s.claimNextSubscriptionSync(ctx); err != nil || third != "" {
		t.Fatalf("两条都已领完时不应再领到任务：%q %v", third, err)
	}
}

// 节点侧已改完之后，落库不能因为调用方 context 已取消而失败。
func TestPersistSurvivesCancelledContext(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	accounts := map[string]subscriptionNodeAccount{
		fixNode: {UserID: "sub_fixture", Password: "deterministic-fixture-password"},
	}
	if err := s.persistReconciledAccounts(cancelled, fixSub, accounts); err != nil {
		t.Fatalf("已确认的成功因上游取消而丢失：%v", err)
	}
	var stored map[string]subscriptionNodeAccount
	if err := db.QueryRow(context.Background(), `SELECT node_accounts FROM subscription_tokens WHERE id=$1::uuid`, fixSub).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored[fixNode].Password != "deterministic-fixture-password" {
		t.Fatalf("映射未落库：%+v", stored)
	}
}

// 执行预算必须覆盖整轮机队；租约必须留出写库余量。
func TestSyncBudgetCoversFleetAndKeepsLeaseMargin(t *testing.T) {
	// 72 台节点正好是 5 分钟租约内能覆盖的上限（18 波 × 15 秒 = 270 秒）。
	// 超过它就说明该换并发度或加租约，而不是让预算悄悄覆盖不住。
	for _, fleet := range []int{1, 4, 12, 20, 40, 72} {
		if !syncBudgetCoversFleet(fleet) {
			t.Errorf("%d 个节点时执行预算覆盖不了整轮 —— 每轮会被切断，永远没有增量进展", fleet)
		}
	}
	if subscriptionSyncNodeBudget != provisionTimeout {
		t.Errorf("单节点预算 %v 与 provisionTimeout %v 脱节，改一处就会让预算失真",
			subscriptionSyncNodeBudget, provisionTimeout)
	}
	if subscriptionSyncRunBudget() <= 0 || subscriptionSyncRunBudget() >= subscriptionSyncClaimLease {
		t.Errorf("执行预算 %v 必须小于租约 %v 且为正", subscriptionSyncRunBudget(), subscriptionSyncClaimLease)
	}
}

// advisory 锁必须带 schema 作用域（2026-10-30）。
//
// 锁只用订阅 id 作键时是**数据库级**的：同一库两个 schema 里相同 id 会互相
// 阻塞，表现为"该订阅正在同步或回收"的假冲突 —— 隔离回归里实测撞到过。
// 作用域必须取自连接池配置：新连接不继承池的 RuntimeParams，
// 在那上面查 current_schema() 永远得到 public，等于没加作用域。
func TestSubscriptionLockScopeUsesPoolSearchPath(t *testing.T) {
	t.Parallel()
	if got := subscriptionLockScope(nil); got != "public" {
		t.Fatalf("无配置时应退化为 public，实际 %q", got)
	}
	cfg, err := pgxpool.ParseConfig("postgres://fixture@127.0.0.1:1/fixture?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "nodeops_fixture_schema"
	if got := subscriptionLockScope(cfg); got != "nodeops_fixture_schema" {
		t.Fatalf("锁作用域必须取连接池的 search_path，实际 %q —— "+
			"取不到会让不同 schema 互相阻塞", got)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = ""
	if got := subscriptionLockScope(cfg); got != "public" {
		t.Fatalf("search_path 为空时应退化为 public，实际 %q", got)
	}
}

// 对账必须与落库共处同一把订阅锁，否则停用回收会与落库交错。
func TestReconcilePersistsUnderSubscriptionLock(t *testing.T) {
	t.Parallel()
	for _, fn := range []string{
		"func (s *Server) reconcileSubscriptionHandler(",
		"func (s *Server) reconcileAllSubscriptionsHandler(",
	} {
		body := funcBlock(t, "provisioning.go", fn)
		if !strings.Contains(body, "lockSubscription(") {
			t.Errorf("%s 没有在外层持锁 —— 对账与落库之间的空窗会让回收删除的账号被合并回映射", fn)
		}
	}
}
// 2026-11-03：领取后拿不到订阅锁（这里用占满 16 个锁槽模拟）时，必须归还租约，
// 不能让任务白等满 5 分钟的领取租约。
func TestClaimReleasedWhenLockUnavailable(t *testing.T) {
	db := fixDB(t)
	createFixSub(t, db)
	s := fixServer(db)
	ctx := context.Background()
	id, lease, err := s.claimNextSubscriptionSync(ctx)
	if err != nil || id != fixSub {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(subscriptionLockSlots); i++ {
		subscriptionLockSlots <- struct{}{}
	}
	err = s.synchronizeClaimedSubscription(ctx, rec, &syncClaim{leasedUntil: lease})
	for i := 0; i < cap(subscriptionLockSlots); i++ {
		<-subscriptionLockSlots
	}
	if err == nil {
		t.Fatal("lock unavailable must be reported")
	}
	var wait float64
	if err = db.QueryRow(ctx, `SELECT extract(epoch from next_attempt - now()) FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait > 60 {
		t.Fatalf("lease not released after lock failure: next attempt in %.0fs", wait)
	}
}

// 2026-11-06：锁内读库 / 复核领取失败时也要归还租约，不能让交付白等满 5 分钟。
func TestClaimedSyncReleasesLeaseOnInLockErrors(t *testing.T) {
	body := funcBlock(t, "delivery_sync.go", "func (s *Server) synchronizeClaimedSubscription(")
	if strings.Count(body, "s.releaseSyncClaim(rec.ID, claim.leasedUntil") < 3 {
		t.Fatal("lock failure, in-lock load failure and claim verification failure must all release the lease")
	}
}
