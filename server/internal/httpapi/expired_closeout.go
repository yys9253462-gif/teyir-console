package httpapi

// expired_closeout.go — 到期订阅的「结案」出口。
//
// 背景（2026-10-24 排查旧版 shop.example.com 商城的到期订阅时实测到）：
//
// 生产上有 1 条订阅到期 2 天后仍躺在表里：
//   - expires_at 已过、enabled=false、auto_delete=true
//   - 但 deleted_at 仍为空 → 记录没有真正删除
//   - 它指向的节点（韩国-首尔）API 一直 i/o timeout
//   - subscription_revoke_backoff 的 attempts 已经涨到 27 次
//
// 为什么以前删不掉：purgeSubscriptionOutcome 的顺序是「先收回节点账号 → 再 DELETE 行」。
// 收回失败（节点不可达）时它**保留行**，等下一轮重试。这在节点只是短暂抖动时是对的，
// 但节点永久下线时就是死循环：
//
//   节点永远连不上 → 收回永远失败 → 行永远不删 → 每轮都重拨一次死节点
//   → 记录永远挂在"到期未删除"名单里，运维看到的待办永远清不掉。
//
// 而商城侧同时还有另一批情况：面板已退役（面板行 active=0）或面板记录已删，
// 账号在物理上不可能再回收，但那批记录的 node_deliveries 行留在库里。
//
// 本文件提供**两个**出口，都要求管理员显式操作 + 危险确认头，且都写审计：
//
//  1. POST /api/subscriptions/{id}/close-expired
//     对单条到期订阅结案：把仍挂在节点上的账号登记进 orphan_node_accounts（留痕），
//     然后删掉订阅行。**语义是"放弃物理回收"**，所以必须由人确认，
//     不能让后台 sweeper 自动做 —— 节点恢复后账号仍可能存在，
//     登记进 orphan 表正是为了让这件事留在可查、可补删的地方。
//
//  2. POST /api/subscriptions/close-expired-batch
//     批量结案「已到期 + 节点已删除/已退役 + 反复重试仍失败」的订阅。
//     判据刻意保守：只有当订阅过期、账号所在节点**已被删除**（deleted_at 非空）
//     或**已退役**（走 orphan 登记）时才算数，绝不因为"连不上"就放弃回收。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// closeExpiredTimeout 单次结案操作的总预算。
// 结案要读订阅、读节点、登记孤儿、删行，几秒足够；
// 慢的只有节点侧调用，而结案路径**不做**节点侧调用（那正是要放弃的部分）。
const closeExpiredTimeout = 20 * time.Second

// nodeAccountRow 是结案时要摘除的「订阅 → 节点账号」映射。
type nodeAccountRow struct {
	NodeID string
	UserID string
}

// loadExpiredSubscriptionAccounts 取出订阅上仍挂着的节点账号映射。
//
// ⚠️ 取 user_id 的写法必须与 halfdecommissioned.go 一致：
// node_accounts 的结构是 {"<节点uuid>": {"user_id": "...", "password": "...", ...}}，
// 所以要的是 value->>'user_id'。
//
// 两个坑：
//   - 用 jsonb_each_text 拿到的是**文本**，value 是 text 类型，
//     再写 ->>'user_id' 会报 "operator does not exist: text ->> unknown"。
//     必须用 jsonb_each（value 保持 jsonb 类型）。
//   - 即便修正了类型，写成整个对象也会出错 —— 那样登记到 orphan 表里的
//     user_id 会是一段 JSON 文本，后续补删时拿着它去节点上删账号必然 404，
//     而且表里看着"有记录"，属于最难发现的一类坏数据。
func (s *Server) loadExpiredSubscriptionAccounts(ctx context.Context, id string) ([]nodeAccountRow, error) {
	rows, err := s.db.Query(ctx, `SELECT mapped.node_id::text, mapped.user_id
FROM subscription_tokens
CROSS JOIN LATERAL (
  SELECT key AS node_id,
         COALESCE(value->>'user_id','') AS user_id
  FROM jsonb_each(COALESCE(subscription_tokens.node_accounts,'{}'::jsonb))
) AS mapped
WHERE subscription_tokens.id=$1::uuid AND subscription_tokens.deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]nodeAccountRow, 0, 4)
	for rows.Next() {
		var row nodeAccountRow
		if err := rows.Scan(&row.NodeID, &row.UserID); err != nil {
			return nil, err
		}
		if row.UserID == "" {
			continue
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// nodeIsGone 判断节点是否已经不可能再回收账号。
//
// 判据只有两种，都要求节点**被明确标记**过，而不是"连不上"：
//   - deleted_at 非空：节点已被删除，凭据已抹，物理上不可能再调它
//   - 节点被标记为退役：api_url/api_key 已清空（同样不可达且无从凭据）
//
// "连不上"本身**不**算结案理由：临时网络抖动不该让账号被放弃。
func (s *Server) nodeIsGone(ctx context.Context, nodeID string) (name string, gone bool, apiURL string, err error) {
	var deleted bool
	var bound bool
	err = s.db.QueryRow(ctx, `SELECT name, deleted_at IS NOT NULL,
       (COALESCE(api_url,'') <> '' AND api_key IS NOT NULL), COALESCE(api_url,'')
FROM hysteria_nodes WHERE id=$1::uuid`, nodeID).Scan(&name, &deleted, &bound, &apiURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", true, "", nil
	}
	if err != nil {
		return "", false, "", err
	}
	return name, deleted || !bound, apiURL, nil
}

// closeExpiredSubscription 对一条到期订阅执行结案。
//
// 步骤：
//  1. 订阅必须已到期，且还没被删（已删的直接返回，不重复处理）
//  2. 把仍挂着的每个节点账号登记进 orphan_node_accounts（留痕 + 凭据快照）
//     —— 这一步**摘除订阅上的映射**，否则下面删行时账号就彻底没人知道了
//  3. 删除订阅行，并清掉对应的回收退避记录（否则退避表里会一直留着这行）
//
// 返回值：登记的孤儿账号数、删除的行数。
var errCloseoutNotFound = errors.New("订阅不存在或已删除")

// errCloseoutInternal 包住结案过程中的数据库错误（2026-11-04）。原来这些错误以 409 + 原文回给调用方：
// 数据库故障被说成「冲突」，而且 pgx 的错误原文（表名、列、SQLSTATE）直接出现在响应里。
// 现在回 500、原文只进日志；业务上的拒绝（未到期、登记失败而中止、状态已变）仍是 409。
var errCloseoutInternal = errors.New("结案时读写数据库失败")

type closeoutRequireAllGoneKey struct{}

// withRequireAllNodesGone 标记本次结案必须在订阅锁内复核全部节点账号均处于已下线状态，
// 供批量结案入口防并发竞态（避免外层刚检查完就新增/恢复了在役节点账号）。
func withRequireAllNodesGone(ctx context.Context) context.Context {
	return context.WithValue(ctx, closeoutRequireAllGoneKey{}, true)
}

func (s *Server) closeExpiredSubscription(ctx context.Context, id, reason string) (orphans int, deleted bool, err error) {
	// 与续费、编辑、同步、回收共用同一把订阅锁，并在锁内重读：
	// 否则「先读到已到期 → 期间用户续费 → 再删行」会把刚续费的订阅删掉。
	ctx, release, err := s.lockSubscription(ctx, id)
	if err != nil {
		// 2026-11-06：取锁时的数据库错误走 500（原文只进日志）。原来一律包成「订阅正在处理中」回 409，
		// 而且 %w 把 pgx 连接错误原文（库名、用户名、地址）带进了响应。
		var dbErr subscriptionLockDBError
		if errors.As(err, &dbErr) {
			return 0, false, fmt.Errorf("%w：取订阅锁：%v", errCloseoutInternal, dbErr.Unwrap())
		}
		return 0, false, errors.New("订阅正在处理中，请稍后重试")
	}
	defer release()
	rec, err := s.loadSubscriptionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, errCloseoutNotFound
		}
		return 0, false, fmt.Errorf("%w：读取订阅：%v", errCloseoutInternal, err)
	}
	if !rec.expired(time.Now()) {
		return 0, false, fmt.Errorf("订阅尚未到期，不能结案")
	}
	accounts, err := s.loadExpiredSubscriptionAccounts(ctx, id)
	if err != nil {
		return 0, false, fmt.Errorf("%w：读取节点账号映射：%v", errCloseoutInternal, err)
	}
	type resolvedAccountNode struct {
		account nodeAccountRow
		name    string
		gone    bool
		apiURL  string
	}
	requireAllGone, _ := ctx.Value(closeoutRequireAllGoneKey{}).(bool)
	resolved := make([]resolvedAccountNode, 0, len(accounts))
	for _, account := range accounts {
		name, gone, apiURL, err := s.nodeIsGone(ctx, account.NodeID)
		if err != nil {
			return orphans, false, fmt.Errorf("%w：读取节点 %s 状态：%v", errCloseoutInternal, shortID(account.NodeID), err)
		}
		if requireAllGone && !gone {
			return orphans, false, fmt.Errorf("节点 %s 仍在役，不符合批量结案条件，已跳过", name)
		}
		resolved = append(resolved, resolvedAccountNode{
			account: account,
			name:    name,
			gone:    gone,
			apiURL:  apiURL,
		})
	}
	// 先登记孤儿：登记失败就**中止**，不删行。
	// 顺序反了会丢掉"这个账号曾经存在、在这台机器上"的线索 ——
	// 那正是 orphan 表存在的意义。
	for _, item := range resolved {
		account, name, gone, apiURL := item.account, item.name, item.gone, item.apiURL
		note := reason
		if note == "" {
			note = "订阅到期，运营确认放弃物理回收"
		}
		if gone {
			note = note + "（节点已下线：" + name + "）"
		}
		// 2026-11-03：节点仍在役（已绑定 API）时带上凭据快照。原来一律传 nil，
		// 登记的待补删记录没有密钥 ⇒「补删」直接报「没有留存凭据」，只能手工到机器上删，
		// 而那台节点其实可达、库里也有密钥。已下线的节点没有密钥可取，照旧不带。
		var snapshot *portalTarget
		if !gone {
			// 直接读库里的密钥，不走 loadPortalTarget：那里会因「节点安装/迁移中」拒绝，
			// 而这里只是给待补删记录留一份凭据，不对节点发任何请求。
			if target, terr := s.portalCredentialSnapshot(ctx, account.NodeID); terr == nil {
				snapshot = target
			} else {
				s.logger.Warn("close expired: 读取节点凭据快照失败，待补删记录将没有凭据",
					"node", name, "error", terr)
			}
		}
		saved := s.recordOrphanAccounts(ctx, account.NodeID, name, apiURL, snapshot, []revokeFailure{{
			UserID: account.UserID, SubID: id, Reason: note,
		}})
		orphans += saved
		if saved == 0 {
			// 登记失败就**中止**：不能删行。
			// 删了行，这个账号就再也没人知道了 —— 那正是 orphan 表要防的事。
			return orphans, false, fmt.Errorf("节点账号 %s 登记待补删失败，已中止结案（订阅记录保留）", account.UserID)
		}
	}
	// 登记完成 → 在单事务内原子摘映射、删行并清理退避与待办记录，防中途失败留下无映射僵尸行。
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return orphans, false, fmt.Errorf("%w：开启结案事务：%v", errCloseoutInternal, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE subscription_tokens
SET node_accounts='{}'::jsonb, active_ips='[]'::jsonb, enabled=FALSE
WHERE id=$1::uuid AND deleted_at IS NULL`, id); err != nil {
		return orphans, false, fmt.Errorf("%w：摘除节点账号映射：%v", errCloseoutInternal, err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM subscription_tokens
WHERE id=$1::uuid AND expires_at IS NOT NULL AND expires_at <= now()`, id)
	if err != nil {
		return orphans, false, fmt.Errorf("%w：删除订阅记录：%v", errCloseoutInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return orphans, false, fmt.Errorf("订阅已不再处于到期状态，未删除（账号映射已登记待补删）")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM subscription_revoke_backoff WHERE subscription_id=$1::uuid`, id); err != nil {
		return orphans, false, fmt.Errorf("%w：清理回收退避记录：%v", errCloseoutInternal, err)
	}
	var hasPendingTable bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('subscription_pending_node_actions') IS NOT NULL`).Scan(&hasPendingTable); err != nil {
		return orphans, false, fmt.Errorf("%w：检查待办表：%v", errCloseoutInternal, err)
	}
	if hasPendingTable {
		if _, err := tx.Exec(ctx, `DELETE FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, id); err != nil {
			return orphans, false, fmt.Errorf("%w：清理待办动作记录：%v", errCloseoutInternal, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return orphans, false, fmt.Errorf("%w：提交结案事务：%v", errCloseoutInternal, err)
	}
	return orphans, true, nil
}

// POST /api/subscriptions/{id}/close-expired
//
// 对单条到期订阅结案。语义是**放弃物理回收**，所以要求：
//   - 管理员
//   - X-Confirm-Dangerous: yes（与删除节点、删除订阅同一套约定）
//   - 订阅必须已到期
func (s *Server) closeExpiredSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "到期结案会放弃对该订阅节点账号的物理回收，需要 X-Confirm-Dangerous: yes",
		})
		return
	}
	id := r.PathValue("id")
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	ctx, cancel := context.WithTimeout(r.Context(), closeExpiredTimeout)
	defer cancel()
	if !uuidPattern.MatchString(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errCloseoutNotFound.Error()})
		return
	}
	orphans, deleted, err := s.closeExpiredSubscription(ctx, id, reason)
	if err != nil {
		// 2026-11-03：原来一律 409 + 原文，「订阅不存在」也是 409、数据库错误原样外泄。
		if errors.Is(err, errCloseoutNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, errCloseoutInternal) {
			s.logger.Error("close expired: database error", "subscription", shortID(id), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errCloseoutInternal.Error() + "，请查看服务端日志"})
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), r, "close_expired_subscription", "subscription_token", id,
		map[string]any{"orphans": orphans, "deleted": deleted, "reason": reason})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "closed", "orphans_registered": orphans, "deleted": deleted,
		"note": "订阅记录已删除；节点侧账号已登记到 orphan_node_accounts，可在「节点回收」中查询与补删。",
	})
}

// POST /api/subscriptions/close-expired-batch
//
// 批量结案：**只**处理「已到期 + 节点已被删除或已退役」的订阅。
//
// 为什么不自动扫「连不上」的：节点可能只是网络抖动，把仍可回收的账号批量放弃
// 是不可逆的损失。批量入口因此刻意排除这种情况 ——
// 连不上的走单条 close-expired，由人逐条判断。
func (s *Server) closeExpiredSubscriptionBatchHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "批量到期结案需要 X-Confirm-Dangerous: yes",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), closeExpiredTimeout*10)
	defer cancel()

	// 候选：已到期、未删除、且仍挂着节点账号映射的订阅。
	rows, err := s.db.Query(ctx, `
SELECT subscription_tokens.id::text
FROM subscription_tokens
WHERE subscription_tokens.deleted_at IS NULL
  AND subscription_tokens.expires_at IS NOT NULL
  AND subscription_tokens.expires_at <= now()
  AND COALESCE(subscription_tokens.node_accounts,'{}'::jsonb) <> '{}'::jsonb
ORDER BY subscription_tokens.expires_at
LIMIT 200`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	candidates := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	rows.Close()

	closed := make([]string, 0, len(candidates))
	skipped := make([]string, 0, len(candidates))
	failedReasons := map[string]string{}
	// 节点下线状态在本轮批量预筛中按节点 ID 缓存，避免对同一批候选重复查库；
	// 进入单条结案后仍会在订阅锁内权威复核一次，防止并发恢复/新增在役节点。
	nodeGoneCache := make(map[string]bool)
	for _, id := range candidates {
		accounts, err := s.loadExpiredSubscriptionAccounts(ctx, id)
		if err != nil {
			s.logger.Warn("批量到期结案：读取账号映射失败", "subscription", shortID(id), "error", err)
			skipped = append(skipped, id)
			continue
		}
		if len(accounts) == 0 {
			skipped = append(skipped, id)
			continue
		}
		// 全部账号所在节点都必须"已下线"才批量结案。
		allGone := true
		for _, account := range accounts {
			gone, cached := nodeGoneCache[account.NodeID]
			if !cached {
				_, gone, _, err := s.nodeIsGone(ctx, account.NodeID)
				if err != nil {
					s.logger.Warn("批量到期结案：读取节点状态失败", "subscription", shortID(id), "node", shortID(account.NodeID), "error", err)
					allGone = false
					break
				}
				nodeGoneCache[account.NodeID] = gone
				if !gone {
					allGone = false
					break
				}
				continue
			}
			if !gone {
				allGone = false
				break
			}
		}
		if !allGone {
			skipped = append(skipped, id)
			continue
		}
		if _, _, err := s.closeExpiredSubscription(withRequireAllNodesGone(ctx), id, "批量结案：已到期且节点已下线"); err != nil {
			// 2026-11-03：原来连原因都不记，「登记孤儿失败而中止」与「不符合条件」在结果里分不开。
			s.logger.Warn("批量到期结案：结案失败", "subscription", shortID(id), "error", err)
			skipped = append(skipped, id)
			msg := err.Error()
			if errors.Is(err, errCloseoutInternal) {
				msg = errCloseoutInternal.Error() + "，请查看服务端日志"
			}
			failedReasons[id] = truncateRunes(msg, 300)
			continue
		}
		closed = append(closed, id)
	}
	if len(closed) > 0 {
		s.audit(r.Context(), r, "close_expired_subscriptions_batch", "subscription_token", "",
			map[string]any{"closed": len(closed), "skipped": len(skipped)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"closed": len(closed), "closed_ids": closed,
		"skipped": len(skipped), "skipped_ids": skipped,
		"failed": failedReasons,
		"note": "只结案了节点已删除或已退役的订阅；节点只是连不上的留在 skipped，需逐条判断后走单条 close-expired。",
	})
}

// portalCredentialSnapshot 读出节点当前的 portal 地址与解密后的密钥，只用于留存快照。
func (s *Server) portalCredentialSnapshot(ctx context.Context, nodeID string) (*portalTarget, error) {
	if s.box == nil {
		return nil, errors.New("服务端未配置凭据加密密钥")
	}
	var name, baseURL string
	var ciphertext []byte
	if err := s.db.QueryRow(ctx, `SELECT name, COALESCE(api_url,''), api_key FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&name, &baseURL, &ciphertext); err != nil {
		return nil, err
	}
	if baseURL == "" || len(ciphertext) == 0 {
		return nil, errPortalNotBound
	}
	plain, err := s.box.Open(ciphertext)
	if err != nil {
		return nil, errors.New("节点 API 密钥解密失败")
	}
	return &portalTarget{NodeID: nodeID, NodeName: name, BaseURL: baseURL, APIKey: string(plain)}, nil
}
