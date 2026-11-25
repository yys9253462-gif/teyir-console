package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type subscriptionLockKey struct{}

var subscriptionLockSlots = make(chan struct{}, 16)

// subscriptionLockScope 返回 advisory 锁的作用域名。
//
// 取连接池的 search_path：pgxpool 的每个连接都带同一组 RuntimeParams，
// 所以这就是本进程实际读写的那个 schema。取不到时退化为 "public" ——
// 生产本来就是 public，行为不变；隔离环境里由配置显式给出，避免跨 schema 假冲突。
func subscriptionLockScope(cfg *pgxpool.Config) string {
	if cfg == nil {
		return "public"
	}
	if sp := cfg.ConnConfig.RuntimeParams["search_path"]; sp != "" {
		return sp
	}
	return "public"
}

// 订阅锁的三类失败（2026-11-06）。原来全部回 409，且把 pgx 连接错误原文（含库名、用户名、地址）
// 回给商城与前端：数据库连不上被说成「该订阅正在同步或回收」，运维会去查并发而不是数据库。
var (
	// errSubscriptionBusy：确实有别的操作持有这条订阅的锁 → 409。
	errSubscriptionBusy = errors.New("该订阅正在同步或回收，请稍后重试")
	// errSubscriptionLockSlots：本进程同时持有的订阅锁已满（限流）→ 503。
	errSubscriptionLockSlots = errors.New("订阅操作繁忙，请稍后重试")
)

// subscriptionLockDBError 包住取锁时的数据库错误：调用方据此回 500，原文只进日志。
type subscriptionLockDBError struct{ err error }

// Error 不带原文：它会被拼进回给运维 / 商城的失败原因（回收的 failed 明细、待办的 last_error），
// 而 pgx 连接错误含库名、用户名与地址。原文用 Unwrap 取，只进日志。
func (e subscriptionLockDBError) Error() string {
	return "订阅锁暂时不可用（数据库连接失败），请稍后重试"
}
func (e subscriptionLockDBError) Unwrap() error { return e.err }

// writeSubscriptionLockError 按 lockSubscription 的失败类型回状态码。
func (s *Server) writeSubscriptionLockError(w http.ResponseWriter, r *http.Request, err error) {
	var dbErr subscriptionLockDBError
	switch {
	case errors.Is(err, errSubscriptionLockSlots):
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	case errors.As(err, &dbErr):
		s.databaseError(w, r, dbErr.err)
	default:
		writeJSON(w, http.StatusConflict, map[string]string{"error": errSubscriptionBusy.Error()})
	}
}

// 独立连接持有跨进程advisory锁，不占业务连接池，避免锁等待耗尽连接池。
func (s *Server) lockSubscription(ctx context.Context, id string) (context.Context, func(), error) {
	if held, _ := ctx.Value(subscriptionLockKey{}).(string); held == id {
		return ctx, func() {}, nil
	}
	select {
	case subscriptionLockSlots <- struct{}{}:
	default:
		return ctx, nil, errSubscriptionLockSlots
	}
	var slotOnce sync.Once
	cleanupSlot := func() { slotOnce.Do(func() { <-subscriptionLockSlots }) }
	if s.db == nil {
		cleanupSlot()
		return ctx, nil, subscriptionLockDBError{errors.New("数据库未就绪")}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, s.db.Config().ConnString())
	if err != nil {
		cleanupSlot()
		if s.logger != nil {
			s.logger.Warn("subscription lock: connect failed", "subscription", shortID(id), "error", err)
		}
		return ctx, nil, subscriptionLockDBError{err}
	}
	var locked bool
	// ⚠️ 2026-10-30：锁键必须带上 **schema 作用域**，不能只用订阅 id。
	//
	// advisory 锁是数据库级的：同一个订阅 id 在两个 schema 里会互相阻塞。
	// 生产只有一个 schema，行为不变；但任何「同一库多 schema」的场景
	// （隔离回归、影子库比对）都会凭空冒出"该订阅正在同步或回收"的假冲突 ——
	// 实测在隔离 schema 回归里就撞过，看起来像并发 bug，实际是锁粒度错了。
	//
	// 作用域取自连接池的 search_path，而不是在新连接上查 current_schema()：
	// 这里用的是 pgx.Connect(ConnString())，**新连接不继承池的 RuntimeParams**，
	// 查出来永远是 public —— 那等于没加作用域，假冲突照旧。
	// 也不能写死 'public'：非 public 部署会彻底失去互斥，比假冲突危险得多。
	err = conn.QueryRow(cctx, `SELECT pg_try_advisory_lock(hashtextextended($1||':'||$2,9183))`, subscriptionLockScope(s.db.Config()), id).Scan(&locked)
	if err != nil || !locked {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = conn.Close(closeCtx)
		closeCancel()
		cleanupSlot()
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("subscription lock: query failed", "subscription", shortID(id), "error", err)
			}
			return ctx, nil, subscriptionLockDBError{err}
		}
		return ctx, nil, errSubscriptionBusy
	}
	var releaseOnce sync.Once
	return context.WithValue(ctx, subscriptionLockKey{}, id), func() {
		releaseOnce.Do(func() {
			closeCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_ = conn.Close(closeCtx)
			cleanupSlot()
		})
	}, nil
}

func (s *Server) subscriptionMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			next(w, r)
			return
		}
		claims, err := s.authenticate(r)
		if err != nil {
			var unavailable errSessionCheckUnavailable
			if errors.As(err, &unavailable) {
				s.writeAuthError(w, err)
				return
			}
			writeJSON(w, 401, map[string]string{"error": "登录状态无效"})
			return
		}
		// 订阅变更均为管理员动作：在占用跨进程订阅锁之前先拒绝非管理员，防止低权角色抢锁阻塞管理员。
		if claims.Role != "admin" {
			s.auditDenied(r.WithContext(context.WithValue(r.Context(), claimsKey, claims)), "forbidden_admin_only")
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要管理员角色"})
			return
		}
		ctx, release, err := s.lockSubscription(r.Context(), id)
		if err != nil {
			s.writeSubscriptionLockError(w, r, err)
			return
		}
		defer release()
		next(w, r.WithContext(ctx))
	}
}
func (s *Server) lockSubscriptionRequest(w http.ResponseWriter, r *http.Request, rec *subscriptionRecord) (*http.Request, func(), bool) {
	ctx, release, err := s.lockSubscription(r.Context(), rec.ID)
	if err != nil {
		s.writeSubscriptionLockError(w, r, err)
		return r, nil, false
	}
	fresh, err := s.loadSubscriptionByID(ctx, rec.ID)
	if err != nil {
		release()
		// 2026-11-06：查询与加锁之间这条订阅被删掉（并发撤单、清理）是「不存在」，不是服务端错误。
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在或刚被删除"})
			return r, nil, false
		}
		s.databaseError(w, r, err)
		return r, nil, false
	}
	*rec = *fresh
	return r.WithContext(ctx), release, true
}
func (s *Server) queueSubscriptionSync(ctx context.Context, id string) error {
	// ⚠️ 2026-10-30：只在「没有已排定的下一次尝试」时才重置为立即执行。
	//
	// 原来无条件 `status='pending', next_attempt=now()`，后果有两个：
	//   1. 一个**正在执行**的任务（领取时把 next_attempt 推到 45 秒后当租约）
	//      会被改回 next_attempt=now()，于是 30 秒一轮的 worker 会再领一次同一个
	//      订阅。advisory 锁会让第二次失败，但每轮都白开一次数据库连接、刷一条
	//      warn 日志。
	//   2. 失败退避（最长 15 分钟）被整点 gap-fill 无条件清零 —— 故障节点上
	//      的开户会被每小时强行重试一遍。
	// 现在的语义：已经排了未来一次尝试（无论是租约还是退避）就不动它；
	// 其余的（synced / cancelled / 已到期的 pending）才重置为立即 pending。
	// 判据必须同时看 status：只有「pending/degraded 且已排定未来一次尝试」
	// 才是真正在执行或退避中。status='synced' 的行也可能带着一个未来的
	// next_attempt（finish 统一按退避公式写），那种行必须能被重新排队 ——
	// 只按 next_attempt 判断会让"新节点上架后立即补开户"变成空操作，
	// 白白等到退避到期。
	_, err := s.db.Exec(ctx, `INSERT INTO subscription_sync_jobs(subscription_id,status,next_attempt)
VALUES($1::uuid,'pending',now())
ON CONFLICT(subscription_id) DO UPDATE SET
  status = CASE WHEN subscription_sync_jobs.status IN ('pending','degraded')
                 AND subscription_sync_jobs.next_attempt > now()
                THEN subscription_sync_jobs.status ELSE 'pending' END,
  error  = CASE WHEN subscription_sync_jobs.status IN ('pending','degraded')
                 AND subscription_sync_jobs.next_attempt > now()
                THEN subscription_sync_jobs.error ELSE '' END,
  next_attempt = CASE WHEN subscription_sync_jobs.status IN ('pending','degraded')
                       AND subscription_sync_jobs.next_attempt > now()
                      THEN subscription_sync_jobs.next_attempt ELSE now() END,
  updated_at = now()`, id)
	return err
}
func (s *Server) finishSubscriptionSync(id string, err error) {
	s.finishSubscriptionSyncStatus(id, err, "")
}

// syncDegraded：只差「故障节点自动下线」已判下线的节点没开成户，其余健康节点都已交付。
// 这些故障节点本来就不会出现在买家订阅里（filterHealthyNodes），让整单卡在 pending
// 等它们，等于让买家为一台不卖给他的机器等货。degraded 对商城算已交付，
// 但仍留在重试队列里：节点恢复后自动补开户，订阅里随之多出这个节点。
const syncDegraded = "degraded"

func (s *Server) finishSubscriptionSyncStatus(id string, err error, forced string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, message := "synced", ""
	if err != nil {
		status = "pending"
		message = truncateRunes(err.Error(), 1500)
	}
	if forced != "" {
		status = forced
	}
	// 2026-11-03：attempts 是「连续失败次数」，成功时清零。原来成功也 +1 且没有任何地方清零，
	// 同步成功过 30 次以上的订阅（续费、改标签、每小时补开户）第一次真失败就直接退避 15 分钟。
	if _, e := s.db.Exec(ctx, `UPDATE subscription_sync_jobs SET status=$2,error=$3,
attempts=CASE WHEN $4 THEN 0 ELSE attempts+1 END,
next_attempt=now()+make_interval(secs=>CASE WHEN $4 THEN 30 ELSE LEAST(900,30*(attempts+1)) END),updated_at=now()
WHERE subscription_id=$1::uuid`, id, status, message, err == nil); e != nil {
		s.logger.Error("delivery sync result persistence failed", "error", e)
	}
}

// onlyUnhealthyGaps 判断这次同步的缺口是否全部落在已自动下线的故障节点上，
// 且至少有一个健康节点已持有本订阅账号（买家确实拿得到可用节点）。
// 自动下线关闭、或全部节点都故障（filterHealthyNodes 的安全阀会原样下发）时返回 false。
func (s *Server) onlyUnhealthyGaps(ctx context.Context, accounts map[string]subscriptionNodeAccount, failures []provisionResult) bool {
	if len(failures) == 0 || !s.autoHideEnabled(ctx) {
		return false
	}
	bad := s.unhealthyNodes(ctx)
	failed := make(map[string]bool, len(failures))
	for _, f := range failures {
		if _, hidden := bad[f.NodeID]; !hidden {
			return false
		}
		failed[f.NodeID] = true
	}
	for id, acc := range accounts {
		if _, hidden := bad[id]; !hidden && !failed[id] && acc.UserID != "" {
			return true
		}
	}
	return false
}

// deterministicSyncFailure 判断这次节点失败是不是「等也不会自愈」的确定性拒绝。
// 只收录含义明确、不会因为节点恢复而消失的几类；拿不准的一律按可恢复处理（宁可多等一轮，
// 也不要把真的「暂时连不上」当成已交付）。
func deterministicSyncFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"unauthorized", "forbidden",
		"endpoint not found", "api endpoint not found",
		"api 密钥解密失败", "凭据解密失败",
		"节点门户版本过低", "不支持整机流量", "需升级 portal",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	// HTTP 状态码要按**独立数字**匹配：裸 strings.Contains("401") 会把
	// 「dial tcp 1.2.3.4:4013: connection refused」这种端口号含 401 的可恢复错误
	// 也判成确定性拒绝 —— 那会把「暂时连不上」当成已交付，买家少一台节点还没人知道。
	return syncHTTPStatusPattern.MatchString(msg)
}

// syncHTTPStatusPattern 匹配带有明确 HTTP/状态前缀的 401 / 403，避免误命中端口 :403:、延迟 403 ms 或路径数字。
var syncHTTPStatusPattern = regexp.MustCompile(`(?i)(?:^|[\s:(\[])(?:http\s*|status[=:\s]*|返回\s*)(401|403)(?:$|[\s),:;\]])`)

// canDegradeSync 判断本轮失败是否可安全降级交付：
// 每台失败节点要么是自动下线的故障节点，要么属于不会自愈的确定性拒绝，
// 且至少一台不在故障名单中的范围内节点已持有有效账号。
func (s *Server) canDegradeSync(ctx context.Context, scope map[string]subscriptionNodeAccount, failures []provisionResult) bool {
	if len(failures) == 0 {
		return false
	}
	var bad map[string]hiddenNode
	if s.autoHideEnabled(ctx) {
		bad = s.unhealthyNodes(ctx)
	}
	failed := make(map[string]bool, len(failures))
	for _, f := range failures {
		_, hidden := bad[f.NodeID]
		if !hidden && !deterministicSyncFailure(f.Err) {
			return false
		}
		failed[f.NodeID] = true
	}
	for id, acc := range scope {
		if _, hidden := bad[id]; !hidden && !failed[id] && acc.UserID != "" {
			return true
		}
	}
	return false
}

// allDeterministicSyncFailures：本轮所有失败都是确定性拒绝（空切片返回 false）。
func allDeterministicSyncFailures(failures []provisionResult) bool {
	if len(failures) == 0 {
		return false
	}
	for _, f := range failures {
		if !deterministicSyncFailure(f.Err) {
			return false
		}
	}
	return true
}

// hasDeliveredAccount：范围内至少有一个账号本轮没有失败（买家确实拿得到可用节点）。
func hasDeliveredAccount(scope map[string]subscriptionNodeAccount, failures []provisionResult) bool {
	failed := make(map[string]bool, len(failures))
	for _, f := range failures {
		failed[f.NodeID] = true
	}
	for id, acc := range scope {
		if !failed[id] && acc.UserID != "" {
			return true
		}
	}
	return false
}

func (s *Server) synchronizeSubscription(ctx context.Context, rec *subscriptionRecord) error {
	return s.synchronizeClaimedSubscription(ctx, rec, nil)
}

// syncClaim 记录 worker 领取任务时写下的租约。
//
// 领取与执行是两件事：领取只把 next_attempt 推到未来，执行要拿订阅锁、
// 逐个节点发请求。这中间（以及执行期间）任务可能已经被前台请求完成、
// 被停用取消、或被另一个实例重新领取。带着 claim 执行时必须先核对它，
// 否则会把已经交付成功的订阅重新置回 pending，商城随即报"尚未完成"。
type syncClaim struct {
	leasedUntil time.Time
}

// claimedSyncStillValid 判断领取是否仍然有效：状态仍待处理，且租约仍是本次领取写入的值。
//
// 用 next_attempt 的精确值作为领取凭据，不需要额外加列：
// 每次领取都会写入一个不同的 now()+租约；被其他实例重新领取时值必然改变。
func (s *Server) claimedSyncStillValid(ctx context.Context, id string, leasedUntil time.Time) (bool, error) {
	var status string
	var next time.Time
	err := s.db.QueryRow(ctx, `SELECT status, next_attempt FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, id).Scan(&status, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "pending" && status != syncDegraded {
		return false, nil
	}
	return next.Equal(leasedUntil), nil
}

func (s *Server) synchronizeClaimedSubscription(ctx context.Context, rec *subscriptionRecord, claim *syncClaim) error {
	ctx, release, err := s.lockSubscription(ctx, rec.ID)
	if err != nil {
		if claim != nil {
			s.releaseSyncClaim(rec.ID, claim.leasedUntil, err)
		}
		return err
	}
	defer release()
	fresh, err := s.loadSubscriptionByID(ctx, rec.ID)
	if err != nil {
		// 2026-11-06：锁内读库失败同样归还租约（30 秒后重试）。原来直接返回，任务要等满 5 分钟租约
		// 才会被再次领取 —— 新订单遇到一次数据库抖动，商城就多报 5 分钟「交付尚未完成」。
		if claim != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.releaseSyncClaim(rec.ID, claim.leasedUntil, err)
		}
		return err
	}
	*rec = *fresh
	// 锁内复核领取：订阅级 advisory 锁只保证"同一时刻只有一个执行者"，
	// 它不能证明这次执行仍然该做。
	if claim != nil {
		valid, verifyErr := s.claimedSyncStillValid(ctx, rec.ID, claim.leasedUntil)
		if verifyErr != nil {
			s.releaseSyncClaim(rec.ID, claim.leasedUntil, verifyErr)
			return verifyErr
		}
		if !valid {
			s.logger.Info("delivery claim superseded, skip", "subscription", shortID(rec.ID))
			return nil
		}
	}
	if !rec.Enabled || rec.expired(time.Now()) {
		_, err = s.db.Exec(ctx, `UPDATE subscription_sync_jobs SET status='cancelled',error='订阅停用或已到期',updated_at=now() WHERE subscription_id=$1::uuid`, rec.ID)
		return err
	}
	if err = s.queueSubscriptionSync(ctx, rec.ID); err != nil {
		if claim != nil {
			s.releaseSyncClaim(rec.ID, claim.leasedUntil, err)
		}
		return err
	}
	if claim == nil {
		// 前台直接同步同样为任务行续上租约，防止每 30 秒的后台重试队列同时领走同一订阅撞锁并消耗每轮配额。
		leaseCtx, stopLease := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if _, leaseErr := s.db.Exec(leaseCtx, `
UPDATE subscription_sync_jobs
   SET next_attempt = now() + $2::interval, updated_at = now()
 WHERE subscription_id = $1::uuid AND status IN ('pending', 'degraded')`,
			rec.ID, fmt.Sprintf("%d seconds", int(subscriptionSyncClaimLease.Seconds()))); leaseErr != nil && s.logger != nil {
			s.logger.Warn("delivery: 前台同步续租失败", "subscription", shortID(rec.ID), "error", leaseErr)
		}
		stopLease()
	}
	runCtx, cancel := context.WithTimeout(ctx, subscriptionSyncRunBudget())
	defer cancel()
	accounts, failures, _, err := s.reconcileSubscriptionAccounts(runCtx, rec)
	// ⚠️ 2026-10-30：持久化不能沿用 runCtx。
	//
	// runCtx 只给节点往返留了预算；部分节点成功、其余节点把预算耗光之后，
	// runCtx 恰好到期，persist 立即失败 —— 本轮已经确认成功的映射全部丢失，
	// 节点上却已经换了新密码。下一轮通常能重试恢复，但每轮都超时的订阅
	// 会永远停在"节点已改、库里没记"，回收时按旧映射遍历，漏掉新账号。
	// persistReconciledAccounts 现在用独立有界 context（见该函数注释），
	// 这里仍持有订阅锁，所以落库期间没人能并发改同一订阅。
	//
	// 守卫顺序（别换）：判据只看「有成功结果」，
	// 然后**无条件**落库 —— 哪怕 reconcile 返回了顶层错误。
	// 判据是「这轮至少确认了一个节点」：`accounts` 是 reconcile 对
	// 「旧映射 + 本轮成功节点」的合并，拿到它就意味着部分成功已经发生，
	// 不持久化就等于把成功的结果扔掉。判错方向（写成 err==nil 才落库）
	// 会把「部分成功+一个失败」这类最常见的场景漏掉。
	if len(accounts) > 0 {
		if e := s.persistReconciledAccounts(ctx, rec.ID, accounts); e != nil {
			err = e
		} else {
			rec.nodeAccounts = accounts
		}
	}
	if err == nil && len(failures) > 0 {
		names := []string{}
		for _, f := range failures {
			names = append(names, f.NodeName)
			// 任务表里只记节点名；具体原因原来哪里都不记，日本-东京卡了一整天查不出来（2026-10-28）。
			if f.Err != nil {
				s.logger.Warn("delivery node sync failed", "subscription", shortID(rec.ID), "node", f.NodeName, "error", f.Err.Error())
			}
		}
		err = fmt.Errorf("节点同步尚未完成：%s", strings.Join(names, "、"))
		// 2026-11-06：确定性拒绝（凭据被拒 / 端点不存在 / 门户版本过低 / 密钥解不开）等多久都不会好。
		// 原来只有「失败节点都在自动下线名单里」才算 degraded：一台「活着但拒绝我们」的节点（API key 轮换后
		// 401、门户缺端点）既不在 unhealthyNodes 里，又让每条订阅每轮都是 pending —— 商城 provision_node_order
		// 永远等不到 synced/degraded，而买家其实已经拿到其余节点。这里按「其余范围内账号都成功」降级交付，
		// 失败节点仍留在消息与日志里，后台继续按退避重试。
		// 读范围与降级判定使用独立有界上下文，避免调用方请求超时打断判据把已交付单错打回 pending。
		metaCtx, metaCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		scope := s.inScopeAccounts(metaCtx, rec, accounts)
		// 统一走 canDegradeSync：既覆盖故障下线节点又覆盖确定性拒绝，同时强制要求至少一台非故障节点已交付。
		degrade := s.canDegradeSync(metaCtx, scope, failures)
		metaCancel()
		if degrade {
			s.clearAllRevokeBackoff(ctx, rec.ID)
			s.finishSubscriptionSyncStatus(rec.ID, err, syncDegraded)
			s.logger.Info("delivery degraded: unhealthy nodes skipped, retry continues", "subscription", shortID(rec.ID), "nodes", strings.Join(names, "、"))
			return nil
		}
	}
	// 2026-11-03：判「买家拿到了节点」只能数**当前范围内**的账号。accounts 保留了全部旧映射
	// （回收要用），其中可能有已移出分类/已下架的节点；原来按 len(accounts) 判，范围变空后
	// 任务仍标 synced，商城报已交付，而 /sub 一个节点都不下发。
	if err == nil {
		metaCtx, metaCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		scopeCount := len(s.inScopeAccounts(metaCtx, rec, accounts))
		metaCancel()
		if scopeCount == 0 {
			err = errors.New("没有已交付节点账号，等待节点恢复或配置后重试")
		} else {
			s.clearAllRevokeBackoff(ctx, rec.ID)
		}
	}
	s.finishSubscriptionSync(rec.ID, err)
	return err
}

// inScopeAccounts 只保留落在订阅当前开户范围内、且凭据完整的账号。
// 读范围失败时返回空 —— 无法确认就不能当作「已交付」。
func (s *Server) inScopeAccounts(ctx context.Context, rec *subscriptionRecord, accounts map[string]subscriptionNodeAccount) map[string]subscriptionNodeAccount {
	cands, err := s.listProvisionableNodes(ctx)
	if err == nil {
		cands, err = s.filterProvisionCandidates(ctx, rec, cands)
	}
	if err != nil {
		s.logger.Warn("delivery: 读取开户范围失败", "subscription", shortID(rec.ID), "error", err)
		return nil
	}
	out := make(map[string]subscriptionNodeAccount, len(cands))
	for _, c := range cands {
		if acc, ok := accounts[c.id]; ok && acc.UserID != "" && acc.Password != "" {
			out[c.id] = acc
		}
	}
	return out
}
func (s *Server) StartSubscriptionSync(ctx context.Context) {
	go func() {
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		for {
			func() {
				defer func() {
					if r := recover(); r != nil && s.logger != nil {
						s.logger.Error("delivery: 后台同步轮询发生异常", "panic", r, "stack", string(debug.Stack()))
					}
				}()
				s.retrySubscriptionSync(ctx)
			}()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()
}

// 同步任务的预算与租约。
//
// 一条同步要访问的节点数由运营决定，没有硬上限，所以**不能用"够用就行"的常数**
// 当整轮超时：20 个节点、并发 4、单节点约 13 秒，完整一轮就要 65 秒；
// 预算小于它 ⇒ 每轮都在最后几个节点上被切断，永远没有增量进展。
// 这正是本项目踩过的坑（整轮超时写死 90 秒，靶点扩到 18 个后每轮必然超时）。
//
// 这里改成：单个节点 15 秒独立超时 + 宽松的执行预算。
// 整轮耗时 ≈ ceil(N/4)×15s：72 台节点是 18 波、270 秒，仍在预算内。
// 租约必须覆盖执行预算再加写库余量，否则执行期间又会被别的实例领走。
const (
	subscriptionSyncNodeBudget  = provisionTimeout
	subscriptionSyncClaimLease  = 5 * time.Minute
	subscriptionSyncLeaseMargin = 20 * time.Second
	subscriptionSyncMaxPerRound = 3
)

// subscriptionSyncRunBudget 是本轮执行的预算：租约减去写库余量。
func subscriptionSyncRunBudget() time.Duration {
	return subscriptionSyncClaimLease - subscriptionSyncLeaseMargin
}

// syncBudgetCoversFleet 判断预算是否覆盖给定节点数的完整一轮。
// 用来钉住"预算必须跟着并发度与单节点超时走"，而不是写死一个数。
func syncBudgetCoversFleet(nodes int) bool {
	if nodes <= 0 {
		return true
	}
	waves := (nodes + provisioningConcurrency - 1) / provisioningConcurrency
	return time.Duration(waves)*subscriptionSyncNodeBudget <= subscriptionSyncRunBudget()
}

func (s *Server) retrySubscriptionSync(ctx context.Context) {
	// ⚠️ 2026-10-30：领取与执行必须分开，而且**逐条**领取。
	//
	// 原来一次领 3 条、统一写 45 秒租约，然后顺序执行（每条最长 50 秒）：
	// 第一条还没跑完，第二条的租约就已经过期 —— 别的实例会重新领到它。
	// advisory 锁挡住了"同时开户"，但挡不住**串行重复同步**：
	// 前台已经把这单交付完成（status=synced），这条旧领取仍然会照着
	// 保存下来的 id 再跑一遍，把 synced 重新置回 pending，商城随即
	// 对外报"交付尚未完成"——明明货已经交付过了。
	//
	// 现在的做法：
	//   * 每轮只领 1 条，租约 5 分钟（覆盖完整执行 + 写库余量）；
	//   * 领取后带着租约值进锁，锁内复核状态与租约，旧领取直接跳过；
	//   * 单轮最多执行 3 条，超出部分留给下一轮（30 秒后），避免一个慢订阅
	//     把这一轮的全部时间吃掉。
	banked := 0
	for banked < subscriptionSyncMaxPerRound {
		if err := ctx.Err(); err != nil {
			return
		}
		claimed, lease, err := s.claimNextSubscriptionSync(ctx)
		if err != nil {
			s.logger.Warn("delivery retry claim failed", "error", err)
			return
		}
		if claimed == "" {
			break
		}
		banked++
		rec, err := s.loadSubscriptionByID(ctx, claimed)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 订阅已被删除：直接取消同步任务，彻底终结孤儿任务的 30 秒死循环。
				cancelCtx, cancelDone := context.WithTimeout(context.Background(), 5*time.Second)
				if _, cerr := s.db.Exec(cancelCtx, `UPDATE subscription_sync_jobs SET status='cancelled',error='订阅已不存在',updated_at=now() WHERE subscription_id=$1::uuid`, claimed); cerr != nil {
					s.logger.Warn("delivery: 取消孤儿同步任务失败", "subscription", shortID(claimed), "error", cerr)
				}
				cancelDone()
				continue
			}
			s.releaseSyncClaim(claimed, lease, err)
			continue
		}
		if err = s.synchronizeClaimedSubscription(ctx, rec, &syncClaim{leasedUntil: lease}); err != nil {
			s.logger.Warn("delivery retry pending", "subscription", shortID(claimed), "error", err)
		}
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM store_signature_nonces WHERE expires_at<now()`); err != nil && ctx.Err() == nil {
		s.logger.Warn("store: 清理过期签名防重放记录失败", "error", err)
	}
}

// releaseSyncClaim 在领取后没能真正开始执行时归还租约（30 秒后重试）。
//
// 2026-11-03：领取会把 next_attempt 推到 5 分钟后当租约。原来拿订阅锁失败
// （前台正在同步、或 16 个锁槽占满）或读订阅失败时直接返回，什么都不写回，
// 于是任务白等满 5 分钟。只在租约仍是本次写入的值时归还，避免覆盖别人的结果。
func (s *Server) releaseSyncClaim(id string, leasedUntil time.Time, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `UPDATE subscription_sync_jobs SET next_attempt=now()+interval '30 seconds',
error=$3,updated_at=now() WHERE subscription_id=$1::uuid AND next_attempt=$2 AND status IN ('pending','degraded')`,
		id, leasedUntil, truncateRunes("本轮未能开始："+cause.Error(), 1500)); err != nil {
		s.logger.Warn("delivery: 归还领取租约失败", "subscription", shortID(id), "error", err)
	}
}

// claimNextSubscriptionSync 原子领取一条待同步任务，返回订阅 id 与本次租约值。
func (s *Server) claimNextSubscriptionSync(ctx context.Context) (string, time.Time, error) {
	var id string
	var lease time.Time
	err := s.db.QueryRow(ctx, `
UPDATE subscription_sync_jobs j
   SET next_attempt = now() + make_interval(secs => $1),
       updated_at = now()
  FROM (
      SELECT subscription_id FROM subscription_sync_jobs
       WHERE status IN ('pending','degraded') AND next_attempt <= now()
       ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END, updated_at
       LIMIT 1
       FOR UPDATE SKIP LOCKED
  ) picked
 WHERE j.subscription_id = picked.subscription_id
 RETURNING j.subscription_id::text, j.next_attempt`, subscriptionSyncClaimLease.Seconds()).Scan(&id, &lease)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return id, lease, err
}
func (s *Server) subscriptionDeliveryState(ctx context.Context, id string) (string, string, error) {
	var status, message string
	err := s.db.QueryRow(ctx, `SELECT status,error FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, id).Scan(&status, &message)
	if errors.Is(err, pgx.ErrNoRows) {
		return "legacy", "", nil
	}
	return status, message, err
}

// 节点确认成功之后读回权威绝对期限，不把HTTP200当作已经实际应用。
func verifyAccountExpiry(raw []byte, want int64) error {
	var out struct {
		User struct {
			ExpiresAt int64 `json:"expires_at"`
		} `json:"user"`
	}
	if json.Unmarshal(raw, &out) != nil || out.User.ExpiresAt != want {
		return errors.New("节点期限读回与权威目标不一致")
	}
	return nil
}
