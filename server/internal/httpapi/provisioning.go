package httpapi

// provisioning.go gives every subscription its OWN account on every node.
//
// Before this file, a subscription carried no identity on the node side at all:
// `renderClash` stamped the node's shared `auth_password` into every buyer's
// config. Two things went wrong with that, and only the second one is visible
// from the panel:
//
//  1. `ip_limit` was fiction. The quota is enforced by the node against the
//     account that authenticated, so every buyer's devices were counted against
//     one shared counter — one heavy buyer could lock out everyone else, and no
//     buyer's own limit meant anything.
//
//  2. Expiry could not be enforced end-to-end. Reclaiming a subscription
//     deleted the `subscription_tokens` row and the delivery link started
//     answering 404 — correctly — but the node still held an account whose
//     password was in the config the buyer had already imported. That password
//     resolved to `admin_master`, valid until 2036. A buyer who had ever pulled
//     their subscription kept working after expiry, indefinitely.
//
// So provisioning now runs the other way around: the shop's order is the trigger,
// we open a per-buyer account on each registered node, and the credentials we
// hand out are that account's. Reclaiming deletes them, which is the only thing
// that actually disconnects a client that already has the config.
//
// The whole feature is best-effort by design. A node that is unreachable at
// order time must not fail the order — the buyer would be charged for nothing.
// Such a node is simply skipped, the subscription stays valid for the nodes that
// did answer, and the delivery page reports the gap through the same `excluded`
// channel the rest of the system already uses.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// provisioningConcurrency bounds how many nodes we open accounts on at once.
// Orders are rare and node fleets are small, but an unbounded fan-out here would
// let one shop retry storm hold a connection to every node simultaneously.
const provisioningConcurrency = 4

// provisionTimeout caps a single node's account creation. portalCall already
// applies a 10s deadline; this is the outer bound for the whole per-node step
// including the credential seal.
const provisionTimeout = 15 * time.Second

// provisionResult is one node's outcome, kept per-node so a partial success is
// representable. The shop is told what was provisioned and what was not; it is
// never told "ok" for a node that silently failed.
type provisionResult struct {
	NodeID   string
	NodeName string
	Account  subscriptionNodeAccount
	Err      error
}

// 对账复用确定性密码，但已有账号只能安全更新，不能覆盖用量。
func (s *Server) reconcileSubscriptionAccounts(ctx context.Context, rec *subscriptionRecord) (map[string]subscriptionNodeAccount, []provisionResult, int, error) {
	lockedCtx, release, lockErr := s.lockSubscription(ctx, rec.ID)
	if lockErr != nil {
		return nil, nil, 0, lockErr
	}
	defer release()
	ctx = lockedCtx
	current, loadErr := s.loadSubscriptionByID(ctx, rec.ID)
	if loadErr != nil {
		return nil, nil, 0, loadErr
	}
	*rec = *current
	candidates, err := s.listProvisionableNodes(ctx)
	if err == nil {
		candidates, err = s.filterProvisionCandidates(ctx, rec, candidates)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	accounts := make(map[string]subscriptionNodeAccount, len(candidates)+len(rec.nodeAccounts))
	// Preserve all prior mappings, including temporarily unreachable nodes.
	for k, v := range rec.nodeAccounts {
		accounts[k] = v
	}
	if len(candidates) == 0 {
		return accounts, nil, 0, nil
	}
	results := make([]provisionResult, len(candidates))
	sem := make(chan struct{}, provisioningConcurrency)
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func(i int, c subscriptionCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("provisioning: 同步节点账号协程异常", "subscription", shortID(rec.ID), "node", c.name, "panic", r, "stack", string(debug.Stack()))
					results[i] = provisionResult{
						NodeID:   c.id,
						NodeName: c.name,
						Err:      fmt.Errorf("同步节点账号时发生内部异常: %v", r),
					}
				}
			}()
			results[i] = s.reconcileOneNode(ctx, rec, c)
		}(i, c)
	}
	wg.Wait()

	failures := make([]provisionResult, 0)
	for _, r := range results {
		if r.Err != nil {
			failures = append(failures, r)
			continue
		}
		accounts[r.NodeID] = r.Account
	}
	return accounts, failures, len(results) - len(failures), nil
}

// POST /api/subscriptions/{id}/reconcile — 对一条订阅做账号对账重刷。
//
// 用途：库里记的密码与节点上实际生效的不一致时（典型成因是早期版本
// 让节点随机生成密码），用它把两边刷成一致。
// 仅使用安全属性接口；保留status/used_bytes，失败必须重试。
func (s *Server) reconcileSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
		return
	}
	rec, err := s.loadSubscriptionByID(r.Context(), id)
	if err != nil {
		// 2026-11-03：只有「查无此行」才是 404；数据库故障原来也报「订阅不存在」，误导排障。
		if isNoRows(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	// ⚠️ 2026-10-30：对账与落库必须在**同一把订阅锁**内。
	//
	// reconcileSubscriptionAccounts 自己拿锁、自己释放；如果落库放在它返回之后，
	// 中间这段空窗里可能发生：订阅被停用/到期 ⇒ 回收把账号从节点上删了、
	// 映射也摘了 ⇒ 这边的落库又把旧映射**合并回去**。结果库里记着一批
	// 节点上已经不存在的账号，下次回收照着这份假记录遍历，日志与看板
	// 都会显示"还有未回收的账号"。持有外层锁后，reconcileSubscriptionAccounts
	// 内部的同名锁会命中重入判据直接放行，不会自我阻塞。
	lockCtx, release, lockErr := s.lockSubscription(r.Context(), rec.ID)
	if lockErr != nil {
		s.writeSubscriptionLockError(w, r, lockErr)
		return
	}
	defer release()
	accounts, failures, succeeded, err := s.reconcileSubscriptionAccounts(lockCtx, rec)
	if err != nil {
		// 2026-11-06：这里的错误都来自面板自己（读订阅、读节点列表、解析范围），不是节点：
		// 原来一律 502 + pgx 原文，订阅在两次读之间被删也报成「网关错误：no rows in result set」。
		if isNoRows(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if err := s.persistReconciledAccounts(lockCtx, rec.ID, accounts); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "节点已更新但映射持久化失败，请重试", "persisted": false, "ok_nodes": 0, "node_succeeded": succeeded})
		return
	}
	failed := make([]map[string]any, 0, len(failures))
	for _, f := range failures {
		failed = append(failed, map[string]any{"node": f.NodeName, "error": f.Err.Error()})
	}
	s.audit(r.Context(), r, "reconcile_subscription", "subscription_token", rec.ID,
		map[string]any{"ok_nodes": succeeded, "failed_nodes": len(failures)})
	// 有节点没对上时回 207（与 writeStoreOrder 同一约定），调用方不能只看 2xx 就当全部完成。
	status := http.StatusOK
	if len(failures) > 0 {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{
		"ok_nodes":        succeeded,
		"failed_nodes":    len(failures),
		"attempted_nodes": succeeded + len(failures),
		"retained_nodes":  len(accounts) - succeeded,
		"persisted":       true,
		"failed":          failed,
		"message":         "已安全同步属性；失败节点未改动或未确认，保留原有映射，可同值重试",
	})
}

// reconcileAllMaxSubscriptions 是「全量对账」允许处理的订阅上限。
//
// 为什么要这个闸（2026-10-04 加）：
//
//	reconcile-all 是**同步循环**，每个订阅都要对每个节点发一次请求，
//	单次节点调用最长 10 秒超时。订阅一旦上量，最坏情况是
//	  订阅数 × 节点数 × 10s
//	—— 100 个订阅 × 5 节点就已经是 5000 秒。这既会打爆 HTTP 超时
//	（浏览器先报失败、后台还在跑，运维会以为没生效而重复点），
//	也会同时向所有节点压上有意义的写负载。
//
//	现在生产只有 2 个订阅，跑起来是秒级；但这个闸是为**上量之后**准备的 ——
//	等到出问题才加，那时已经有真实买家在等。
//
// 超过上限时**拒绝并给出可执行的办法**，而不是静默截断 ——
// 截断会让运维以为"全都对好了"，实际只处理了一部分（比报错危险得多）。
const reconcileAllMaxSubscriptions = 50

// POST /api/subscriptions/reconcile-all — 对全部订阅做对账重刷。
//
// 一键修存量：升级到"确定性密码"之后，所有老订阅在节点上的密码都还是
// 早期随机值，必须逐个重刷一次才对得上。
func (s *Server) reconcileAllSubscriptionsHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	recs, err := s.listSubscriptions(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 上限闸：见 reconcileAllMaxSubscriptions 的说明。
	if len(recs) > reconcileAllMaxSubscriptions {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf(
				"订阅数为 %d，超过全量对账的上限 %d。全量对账是同步逐个处理的，"+
					"订阅多了会超时且给所有节点压上有意义的写负载。"+
					"请改用「单条对账」按需处理，或分批进行。",
				len(recs), reconcileAllMaxSubscriptions),
			"subscriptions": len(recs),
			"limit":         reconcileAllMaxSubscriptions,
		})
		return
	}
	type result struct {
		Label  string `json:"label"`
		OK     int    `json:"ok_nodes"`
		Failed int    `json:"failed_nodes"`
	}
	results := make([]result, 0, len(recs))
	totalOK, totalFailed := 0, 0
	for i := range recs {
		if r.Context().Err() != nil {
			s.logger.Warn("reconcile-all: 客户端连接已中断，提前停止剩余对账", "processed", len(results), "total", len(recs), "error", r.Context().Err())
			break
		}
		rec := &recs[i]
		// 与单条对账同因：对账与落库必须在同一把订阅锁内，否则停用/回收
		// 可能与落库交错，把已删账号的映射合并回去。锁冲突只影响这一条，
		// 不能让整轮中断 —— 记为失败并说清原因。
		lockCtx, release, lockErr := s.lockSubscription(r.Context(), rec.ID)
		if lockErr != nil {
			s.logger.Warn("reconcile-all: 订阅忙，跳过", "subscription", shortID(rec.ID), "error", lockErr)
			totalFailed++
			results = append(results, result{Label: rec.Label, OK: 0, Failed: 1})
			continue
		}
		accounts, failures, succeeded, err := s.reconcileSubscriptionAccounts(lockCtx, rec)
		var persistErr error
		if err == nil {
			persistErr = s.persistReconciledAccounts(lockCtx, rec.ID, accounts)
		}
		release()
		if err != nil {
			s.logger.Warn("reconcile-all: 订阅处理失败", "subscription", shortID(rec.ID), "error", err)
			totalFailed++
			results = append(results, result{Label: rec.Label, OK: 0, Failed: 1})
			continue
		}
		if persistErr != nil {
			s.logger.Error("reconcile-all: 节点映射持久化失败", "subscription", shortID(rec.ID), "error", persistErr)
			totalFailed += len(failures) + 1
			results = append(results, result{Label: rec.Label, OK: 0, Failed: len(failures) + 1})
			continue
		}
		totalOK += succeeded
		totalFailed += len(failures)
		results = append(results, result{Label: rec.Label, OK: succeeded, Failed: len(failures)})
	}
	auditCtx, stopAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	s.audit(auditCtx, r, "reconcile_all_subscriptions", "subscription_token", "",
		map[string]any{"subscriptions": len(recs), "processed": len(results), "ok_nodes": totalOK, "failed_nodes": totalFailed})
	stopAudit()
	writeJSON(w, http.StatusOK, map[string]any{
		"subscriptions": len(recs),
		"ok_nodes":      totalOK,
		"failed_nodes":  totalFailed,
		"details":       results,
	})
}

// listProvisionableNodes returns the nodes we can actually open an account on —
// the same set the subscription build would serve, so a buyer is never given a
// config for a node we did not provision.
func (s *Server) listProvisionableNodes(ctx context.Context) ([]subscriptionCandidate, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, name, COALESCE(api_url,''), api_key, COALESCE(domain,''), listen_port, updated_at, in_store
FROM hysteria_nodes
WHERE deleted_at IS NULL AND COALESCE(api_url,'') <> '' AND api_key IS NOT NULL
ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscriptionCandidate
	for rows.Next() {
		var c subscriptionCandidate
		if err := rows.Scan(&c.id, &c.name, &c.apiURL, &c.ciphertext, &c.domain, &c.listenPort, &c.updatedAt, &c.inStore); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// provisionReport 是一次开户的**如实**结果，供手工建订阅的响应使用。
//
// Provisioned / Expected 都按这条订阅的**圈定范围**统计，不是全站节点数：
// 分类圈定与按节点圈定的订阅天生只在一部分节点上开户，拿全站数比会把
// 一次完整成功的交付报成部分失败；而 len(nodeAccounts) 又会把历史上
// 留在映射里、现在已经不在范围内的节点算成"已开户"。
type provisionReport struct {
	Provisioned int
	Expected    int
	Gaps        []map[string]string
}

// scopedProvisionReport 按订阅范围统计开户结果。
//
// 为什么需要它：provisionAndPersist 内部只把失败写进日志
// （对商城下单路径来说这是对的 —— 订单已经成立，不能因为
// 一个节点慢就告诉买家下单失败）。但**手工建订阅**这条路径上
// 操作者就在屏幕前，他需要当场知道哪台没开成。
//
// 读库失败返回 error：调用方必须把"无法确认"如实说出去，
// 不能当成"没有缺口"（那正是"看起来成功、客户连不上"的成因）。
func scopedProvisionReport(ctx context.Context, s *Server, rec *subscriptionRecord) (provisionReport, error) {
	cands, err := s.listProvisionableNodes(ctx)
	if err != nil {
		return provisionReport{}, err
	}
	scoped, err := s.filterProvisionCandidates(ctx, rec, cands)
	if err != nil {
		return provisionReport{}, err
	}
	report := provisionReport{Expected: len(scoped), Gaps: make([]map[string]string, 0)}
	for _, c := range scoped {
		acc, ok := rec.nodeAccounts[c.id]
		if ok && acc.UserID != "" && acc.Password != "" {
			report.Provisioned++
			continue
		}
		report.Gaps = append(report.Gaps, map[string]string{
			"node_id":   c.id,
			"node_name": c.name,
			"reason":    "该节点未开户成功，此节点上的客户端会认证失败。可稍后用「账号对账」重试。",
		})
	}
	return report, nil
}

// subscriptionToNodeDays converts the subscription's deadline into the day count
// the node-side create call wants.
//
// Ceiling division, so an 11th-hour purchase still gets its full day rather than
// expiring the moment it is minted. The floor of 1 keeps portal.py's own default
// from being worse than what we asked for.
func subscriptionToNodeDays(rec *subscriptionRecord, now time.Time) int {
	if rec.ExpiresAt != nil {
		remaining := rec.ExpiresAt.Sub(now)
		if remaining <= 0 {
			return 1
		}
		days := int((remaining + 24*time.Hour - 1) / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		return days
	}
	// Perpetual. 3650 is the panel's own cap on duration_days, so the node-side
	// account cannot outlive any subscription the panel would ever mint.
	return 3650
}

// provisionUserID builds the node-side account id.
//
// Must satisfy portal.py's ^[A-Za-z0-9._-]{1,64}$. A uuid (with hyphens) plus
// the `sub_` prefix fits well inside 64, and embedding the subscription uuid is
// what makes the account greppable from this side when support asks "which
// buyer is sub_00000002-…?". The node id is deliberately NOT part of the id:
// an account belongs to a subscription, and one subscription already has exactly
// one account per node, so the node is identified by which portal holds it.
func provisionUserID(rec *subscriptionRecord) string {
	return "sub_" + rec.ID
}

// provisionAccountPassword 确定性推导某个订阅在某节点上的账号密码。
//
// 为什么必须确定性（2026-10-03 修的真 BUG）
// ----------------------------------------
// 节点侧 portal.py 的 /users/create 是「不带 password 就随机生成」：
//
//	pwd = params.get('password') or secrets.token_hex(16)
//
// 而它对已存在的 user_id 是**覆盖**语义。原实现不传 password，于是：
//   - 第一次创建 → 节点生成密码 A，面板记 A
//   - 第二次创建（JIT 重跑 / 并发 / 节点侧账号被清）→ 节点生成密码 B 并覆盖
//   - 面板库里仍是 A → 客户端拿 A 连 → authentication error
//
// 实测证据：用 users/list 的 expires_at 反推创建时刻，哥伦布-AWS 与
// 阿什本-AWS 上的账号与库里记录的 created_at 不符 —— 确实被重建过；
// 其中哥伦布那台的客户端 100% 连不上。
//
// 改成确定性密码后，重复 create 变成**幂等**：覆盖也不会换密码。
// 这与项目铁律④同源 —— 幂等的最佳实现是「确定性值」，不是「先查再建」。
//
// 输入里包含 nodeID 是**刻意的**：一台节点上出现两个不同订阅的账号时，
// 它们必须拿到不同密码（否则一个买家能用另一个买家的账号）。
// 同一个 (订阅, 节点) 永远得到同一个值 —— 这是幂等所需的全部性质。
//
// 用 SHA-256 而不是直接拼接：一是长度固定（portal 侧 user_id 上限 64，
// 但密码没有显式限制，固定 32 位 hex 最稳），二是避免把订阅 uuid 以
// 明文形式写进节点上的密码字段（那会让节点侧的密码泄漏订阅 id）。
func provisionAccountPassword(rec *subscriptionRecord, nodeID string) string {
	sum := sha256.Sum256([]byte("hy2-account-password\x00" + rec.ID + "\x00" + nodeID))
	return hex.EncodeToString(sum[:16])
}

// openNodeKey decrypts one node's stored API key.
func (s *Server) openNodeKey(ciphertext []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", errors.New("节点未绑定 API 密钥")
	}
	if s.box == nil {
		return "", errors.New("服务端未配置凭据加密密钥")
	}
	plain, err := s.box.Open(ciphertext)
	if err != nil {
		return "", errors.New("节点 API 密钥解密失败")
	}
	return string(plain), nil
}

// shortID is the first 8 characters of a uuid, for log and note text.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// ---------------------------------------------------------------------------
// Revocation
// ---------------------------------------------------------------------------

// revokeSubscriptionAccounts deletes this subscription's accounts from every
// node, best-effort, and reports how many were actually removed.
//
// This is the half that makes expiry real. It is called from the reclaim paths
// (sweeper, read-time purge, shop revoke). A node that cannot be reached is
// logged and skipped rather than retried forever — the account's own expiry is
// the backstop, and that backstop is now meaningful because we set it to match
// the subscription's deadline.
//
// A node target that is missing or unreachable is reported as a failure: without
// authoritative confirmation, the caller must retain the node-account mapping.
type subscriptionRevokeFailure struct {
	NodeID string `json:"node_id"`
	UserID string `json:"user_id"`
	Reason string `json:"reason"`
}

type subscriptionRevokeOutcome struct {
	Revoked int
	Failed  []subscriptionRevokeFailure
}

func (o subscriptionRevokeOutcome) OK() bool { return len(o.Failed) == 0 }

func (s *Server) revokeSubscriptionAccounts(ctx context.Context, rec *subscriptionRecord) subscriptionRevokeOutcome {
	if rec == nil {
		return subscriptionRevokeOutcome{}
	}

	wasExpired, wasDisabled := rec.expired(time.Now()), !rec.Enabled
	lockedCtx, release, err := s.lockSubscription(ctx, rec.ID)
	if err != nil {
		return subscriptionRevokeOutcome{Failed: []subscriptionRevokeFailure{{Reason: err.Error()}}}
	}
	defer release()
	ctx = lockedCtx
	current, err := s.loadSubscriptionByID(ctx, rec.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return subscriptionRevokeOutcome{}
		}
		return subscriptionRevokeOutcome{Failed: []subscriptionRevokeFailure{{Reason: "订阅权威状态读取失败"}}}
	}
	*rec = *current
	if (wasExpired && !current.expired(time.Now())) || (wasDisabled && current.Enabled) {
		return subscriptionRevokeOutcome{Failed: []subscriptionRevokeFailure{{Reason: "订阅已续费或重新启用，本轮取消旧状态回收"}}}
	}
	if len(rec.nodeAccounts) == 0 {
		// 拿锁并重读权威记录后确认无节点账号映射，避免锁前按陈旧空快照早退而漏收并发新开通的账号。
		return subscriptionRevokeOutcome{}
	}

	// Snapshot: the map may have come from a struct the caller keeps using.
	type job struct {
		nodeID  string
		account subscriptionNodeAccount
	}
	jobs := make([]job, 0, len(rec.nodeAccounts))
	for nodeID, account := range rec.nodeAccounts {
		jobs = append(jobs, job{nodeID: nodeID, account: account})
	}
	// Deterministic order keeps logs readable when several nodes are revoked.
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].nodeID < jobs[j].nodeID })

	sem := make(chan struct{}, provisioningConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcome := subscriptionRevokeOutcome{}

	// 连不上的节点按退避间隔重试，不再每 2 分钟拨一次死节点。
	// 映射（node_accounts）仍是「还要回收」的唯一依据，退避只决定这一轮要不要拨号，
	// 所以节点恢复后的下一轮就会补上，不会丢回收。
	deferred, backoffErr := s.revokeBackoffDue(ctx, rec.ID)
	if backoffErr != nil {
		for _, j := range jobs {
			outcome.Failed = append(outcome.Failed, subscriptionRevokeFailure{NodeID: j.nodeID, UserID: j.account.UserID, Reason: "读取回收退避状态失败：" + backoffErr.Error()})
		}
		return outcome
	}
	for _, j := range jobs {
		if until, wait := deferred[j.nodeID]; wait {
			// 2026-11-12：持 mu 追加。同一个循环里前面的迭代已经起了回收 goroutine，它们在 mu 保护下写同一个切片；
			// 这里原来不持锁，是一处写-写数据竞争（会丢一条失败明细）。
			mu.Lock()
			outcome.Failed = append(outcome.Failed, subscriptionRevokeFailure{
				NodeID: j.nodeID, UserID: j.account.UserID,
				Reason: "节点上次回收失败，退避中，" + until.Format("01-02 15:04") + " 后重试",
			})
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("provisioning: 回收节点账号协程异常", "subscription", shortID(rec.ID), "node_id", j.nodeID, "user_id", j.account.UserID, "panic", r, "stack", string(debug.Stack()))
					mu.Lock()
					outcome.Failed = append(outcome.Failed, subscriptionRevokeFailure{
						NodeID: j.nodeID, UserID: j.account.UserID, Reason: fmt.Sprintf("回收账号时发生内部异常: %v", r),
					})
					mu.Unlock()
				}
			}()

			callCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
			defer cancel()

			target, err := s.loadPortalTarget(callCtx, j.nodeID)
			if errors.Is(err, errPortalNodeNotFound) {
				handoffCtx, stopHandoff := context.WithTimeout(context.WithoutCancel(ctx), persistReconciledAccountTimeout)
				handedOff := s.handOffDeletedNodeAccount(handoffCtx, rec.ID, j.nodeID, j.account.UserID)
				if handedOff {
					s.clearRevokeBackoff(handoffCtx, rec.ID, j.nodeID)
				}
				stopHandoff()
				if handedOff {
					// 节点已删：重试在物理上不可能，责任已移交 orphan_node_accounts，
					// 映射也已摘除 —— 对本订阅而言视为收回完成，不再卡住到期流程。
					mu.Lock()
					outcome.Revoked++
					mu.Unlock()
					return
				}
			}
			if err != nil {
				mu.Lock()
				outcome.Failed = append(outcome.Failed, subscriptionRevokeFailure{
					NodeID: j.nodeID, UserID: j.account.UserID, Reason: err.Error(),
				})
				mu.Unlock()
				// 若因外层调用方主动取消/超时导致的失败，不将健康节点拉入退避表，避免后续巡检误跳过。
				if ctx.Err() == nil {
					persistCtx, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), persistReconciledAccountTimeout)
					s.recordRevokeBackoff(persistCtx, rec.ID, j.nodeID, err.Error())
					stopPersist()
				}
				s.logger.Warn("provisioning: revoke target unavailable",
					"node_id", j.nodeID, "user_id", j.account.UserID, "error", err)
				return
			}
			_, err = portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodPost,
				"/users/delete", map[string]any{"user_id": j.account.UserID})
			// ⚠️ 2026-10-04 修：节点说「用户不存在」应当算**成功**，不是失败。
			//
			// 为什么（这是补缺口 B 时被真实测出来的）：
			//   收回的**目标**是「该账号在节点上不存在」。
			//   如果它本来就不存在（账号被别的路径删过、或当初开户就没成功），
			//   目标已经达成 —— 此时 portal 返回的 404 "User not found"
			//   **不是错误，是幂等信号**。
			//
			// 旧行为把它当失败 ⇒ 不清 node_accounts ⇒ 这一行永远「收回失败」：
			//   * sweeper 每轮都重试、永远不标记，日志刷 "revoke incomplete"；
			//   * 更糟的是，看板会一直显示这条订阅「有未回收的账号」，
			//     而实际上节点侧早已干净 —— 属于**假告警压住真告警**。
			//
			// 判据复用 orphan.go 的 isUserNotFound（同一份语义只该有一处实现，
			// 两处各写一份的话，改了一处就会不一致）。
			if err != nil && isUserNotFound(err) {
				err = nil
			}
			// 节点侧删号完成后，使用脱离 15 秒网络超时的独立短写库上下文摘除映射并维护退避记录，
			// 同时不把数据库 I/O 放进 mu 锁临界区。
			persistCtx, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), persistReconciledAccountTimeout)
			defer stopPersist()
			if err == nil {
				// Persist each confirmed deletion immediately. If another node fails,
				// retries then target only the accounts not yet confirmed removed.
				_, err = s.db.Exec(persistCtx, `UPDATE subscription_tokens
					SET node_accounts=COALESCE(node_accounts,'{}'::jsonb)-$1::text
					WHERE id=$2::uuid`, j.nodeID, rec.ID)
			}
			if err != nil {
				mu.Lock()
				outcome.Failed = append(outcome.Failed, subscriptionRevokeFailure{
					NodeID: j.nodeID, UserID: j.account.UserID, Reason: err.Error(),
				})
				mu.Unlock()
				s.logger.Warn("provisioning: revoke or mapping persistence failed",
					"node", target.NodeName, "user_id", j.account.UserID, "error", err)
				if ctx.Err() == nil {
					s.recordRevokeBackoff(persistCtx, rec.ID, j.nodeID, err.Error())
				}
				return
			}
			mu.Lock()
			outcome.Revoked++
			mu.Unlock()
			s.clearRevokeBackoff(persistCtx, rec.ID, j.nodeID)
		}(j)
	}
	wg.Wait()

	if outcome.Revoked > 0 {
		s.logger.Info("provisioning: revoked node accounts",
			"subscription", shortID(rec.ID), "revoked", outcome.Revoked, "total", len(jobs))
	}
	return outcome
}

// handOffDeletedNodeAccount 处理「订阅映射指向一个已删除节点」的残留。
//
// 正常情况下 deleteNode 登记孤儿账号时就会摘掉映射（见 recordOrphanAccounts）；
// 这里兜住登记那一刻失败而保留下来的映射。不兜的后果：loadPortalTarget 对
// 已删节点永远返回 errPortalNodeNotFound，到期 sweeper 的收回永远不完整，
// 订阅永远不会被标记 enabled=FALSE（2026-10-14 达卡节点实测）。
//
// 返回 true 表示已登记为孤儿且映射已摘除；登记失败时返回 false，保留映射待下轮重试。
//
// （函数体在下方 revokeBackoff* 三个函数之后。）

// revokeBackoff* 控制对连不上节点的回收重试频率（迁移 046）。
// 间隔 2→4→8… 分钟，封顶 1 小时（上限写在 recordRevokeBackoff 的 SQL 里）；
// 表查询失败时本轮不拨号：不能确认退避状态，就不能假设「现在可重试」。
func (s *Server) revokeBackoffDue(ctx context.Context, subID string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	rows, err := s.db.Query(ctx, `SELECT node_id::text, next_attempt_at FROM subscription_revoke_backoff
		WHERE subscription_id=$1::uuid AND next_attempt_at > now()`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		var until time.Time
		if err := rows.Scan(&nodeID, &until); err != nil {
			return nil, err
		}
		out[nodeID] = until
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) recordRevokeBackoff(ctx context.Context, subID, nodeID, reason string) {
	// 按字符截断：按字节切中文会产生非法 UTF-8，Postgres 拒绝整条写入，
	// 于是回收退避记录建不起来（每次回收都全量重试）。
	reason = truncateRunes(reason, 300)
	var attempts int
	err := s.db.QueryRow(ctx, `INSERT INTO subscription_revoke_backoff
		(subscription_id, node_id, attempts, next_attempt_at, last_error)
		VALUES ($1::uuid, $2::uuid, 1, now() + interval '2 minutes', $3)
		ON CONFLICT (subscription_id, node_id) DO UPDATE SET
			attempts = subscription_revoke_backoff.attempts + 1,
			next_attempt_at = now() + LEAST(interval '1 hour',
				interval '2 minutes' * power(2, LEAST(subscription_revoke_backoff.attempts, 5))),
			last_error = EXCLUDED.last_error
		RETURNING attempts`, subID, nodeID, reason).Scan(&attempts)
	if err != nil {
		s.logger.Warn("provisioning: revoke backoff not recorded", "subscription", shortID(subID), "error", err)
		return
	}
	// 只在第一次失败和每 12 次（约半天）时记一条，避免每轮刷屏。
	if attempts == 1 || attempts%12 == 0 {
		s.logger.Warn("provisioning: node unreachable for revoke; backing off",
			"subscription", shortID(subID), "node_id", nodeID, "attempts", attempts, "error", reason)
	}
}

func (s *Server) clearRevokeBackoff(ctx context.Context, subID, nodeID string) {
	if _, err := s.db.Exec(ctx, `DELETE FROM subscription_revoke_backoff
		WHERE subscription_id=$1::uuid AND node_id=$2::uuid`, subID, nodeID); err != nil {
		s.logger.Warn("provisioning: revoke backoff not cleared", "subscription", shortID(subID), "error", err)
	}
}

// clearAllRevokeBackoff 在订阅重新同步为有效态后清掉过往回收退避记录，
// 避免续费或重新启用后短期内撤单/停用被陈旧的 1 小时退避跳过。
func (s *Server) clearAllRevokeBackoff(ctx context.Context, subID string) {
	if s.db == nil {
		return
	}
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.Exec(cleanCtx, `DELETE FROM subscription_revoke_backoff WHERE subscription_id=$1::uuid`, subID); err != nil {
		s.logger.Warn("provisioning: clear subscription revoke backoff failed", "subscription", shortID(subID), "error", err)
	}
}

func (s *Server) handOffDeletedNodeAccount(ctx context.Context, subID, nodeID, userID string) bool {
	var nodeName, nodeAPIURL string
	var deleted bool
	err := s.db.QueryRow(ctx, `SELECT name, COALESCE(api_url,''), deleted_at IS NOT NULL
		FROM hysteria_nodes WHERE id=$1::uuid`, nodeID).Scan(&nodeName, &nodeAPIURL, &deleted)
	if err != nil || !deleted {
		// 查不到行（硬删）或节点其实还在：不擅自处理，走原有失败路径。
		return false
	}
	saved := s.recordOrphanAccounts(ctx, nodeID, nodeName, nodeAPIURL, nil, []revokeFailure{{
		UserID: userID, SubID: subID, Reason: "订阅到期收回时节点已删除，无法连接节点注销",
	}})
	if saved == 0 {
		// 登记没成功就别摘映射：保留线索，下轮还能重试。
		return false
	}
	var still bool
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(node_accounts,'{}'::jsonb) ? $1
		FROM subscription_tokens WHERE id=$2::uuid`, nodeID, subID).Scan(&still); err != nil || still {
		return false
	}
	s.logger.Warn("provisioning: 已删节点上的账号已移交孤儿登记",
		"subscription", shortID(subID), "node", nodeName, "user_id", userID)
	return true
}

// persistReconciledAccountTimeout 是落库的独立预算。
//
// 它必须**独立于节点往返的预算**：节点交互消耗的是"等远端"的时间，
// 落库是本机几毫秒的事，两者共用一个 ctx 会让最需要保留的结果最先丢失。
const persistReconciledAccountTimeout = 10 * time.Second

func (s *Server) persistReconciledAccounts(ctx context.Context, id string, accounts map[string]subscriptionNodeAccount) error {
	encoded, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	// context.WithoutCancel：到这一步，节点侧已经改完了。此时请求被取消、
	// 客户端断开、或 runCtx 到期，都不能成为"已确认的成功不落库"的理由 ——
	// 否则节点换了密码、库里还记着旧密码，回收时按旧映射遍历就漏掉这个账号。
	// 但也不能无限期拖住调用方，所以给独立的有界超时。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistReconciledAccountTimeout)
	defer cancel()
	tag, err := s.db.Exec(writeCtx, `UPDATE subscription_tokens SET node_accounts=COALESCE(node_accounts,'{}'::jsonb)||$1::jsonb WHERE id=$2::uuid AND deleted_at IS NULL`, encoded, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("订阅在对账期间被删除")
	}
	return nil
}

func (s *Server) filterProvisionCandidates(ctx context.Context, rec *subscriptionRecord, all []subscriptionCandidate) ([]subscriptionCandidate, error) {
	// 2026-11-03：「是否上架」按订阅类型在这里判，不再写死在候选查询里。
	// 原来 listProvisionableNodes 只取 in_store=TRUE，于是手工订阅勾选的「未上架」
	// 节点（界面明说自用可以用）永远不会开户，开户报告还把它们算作 0 个预期、显示「完整」。
	// 与 applySubscriptionAccounts 同一条规则：只有买家订阅（有 order_id）要求已上架。
	if rec.OrderID != "" {
		listed := make([]subscriptionCandidate, 0, len(all))
		for _, c := range all {
			if c.inStore {
				listed = append(listed, c)
			}
		}
		all = listed
	}
	// 直接圈定节点时只在这些节点上开户（与下发范围 categoryScopeFor 保持一致）。
	if len(rec.NodeIDs) > 0 {
		allowed := make(map[string]bool, len(rec.NodeIDs))
		for _, id := range rec.NodeIDs {
			allowed[id] = true
		}
		out := []subscriptionCandidate{}
		for _, c := range all {
			if allowed[c.id] {
				out = append(out, c)
			}
		}
		return out, nil
	}
	if rec.CategoryID == nil {
		return all, nil
	}
	rows, err := s.db.Query(ctx, `SELECT node_id::text FROM node_category_members WHERE category_id=$1::uuid`, *rec.CategoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		allowed[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []subscriptionCandidate{}
	for _, c := range all {
		if allowed[c.id] {
			out = append(out, c)
		}
	}
	return out, nil
}
