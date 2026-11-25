package httpapi

// pending_node_actions.go — 节点侧执行没完成的停用/回收，后台自动补做（迁移 047）。
//
// 背景（2026-10-23）：
//   - 商城退款（storeRevokeOrder）先把订阅 enabled=FALSE，再去节点删账号；
//   - 后台手动停用（updateSubscriptionHandler）把节点账号置为 disabled。
// 节点当时连不上时，两条路径都只回一句「失败」。之后的到期/清理查询只认
// enabled=TRUE 且已到期、或 auto_delete 已到期的行，都匹配不到「已停用但没到期」
// 的订阅，于是没人再去补：节点账号要等它自己到期才失效，永久订阅永远不会失效。
//
// 做法：失败时写一条待办，sweeper 每轮按退避取出到期的待办重做，直到每个节点都确认。
// 订阅重新生效（续费、重新启用）时待办自动取消——绝不能把续费买家的账号删掉或停掉。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	pendingActionRevoke  = "revoke"
	pendingActionDisable = "disable"
	pendingActionBatch   = 20
)

// queuePendingNodeAction 记录一条待补做的节点动作（2 分钟后首次重试）；同一订阅已有待办时合并。
// revoke 优先级高于 disable：已排了 revoke 的订阅不会被降级成 disable。
// 返回值（2026-11-03）：登记失败时调用方不能再说「已登记、后台会重试」——
// 这条订阅已停用、未到期，到期 / 清理查询都不会再碰它，没有这条待办就再也没有人补做。
// 用独立 ctx 写：客户端断开不该让这条唯一的补做线索丢失。
func (s *Server) queuePendingNodeAction(ctx context.Context, subID, action string, thenDelete bool, reason string) error {
	return s.queuePendingNodeActionFor(ctx, subID, action, thenDelete, false, reason)
}

// queuePendingNodeActionFor 同 queuePendingNodeAction；registerRevoked 只有商城撤单（storeRevokeOrder）传 true：
// 后台补完删除后把订单号登记进 store_revoked_orders（迁移 056 / 058）。
func (s *Server) queuePendingNodeActionFor(ctx context.Context, subID, action string, thenDelete, registerRevoked bool, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// ⚠️ 2026-10-31：必须按**字符**截断，不能按字节。
	// 这些 reason 里几乎一定含中文（"节点门户报错: ..."），按字节切 300
	// 很可能落在某个三字节汉字的中间 —— 产生非法 UTF-8，Postgres 直接拒绝写入
	// （invalid byte sequence for encoding "UTF8"）。
	// 后果不是"少一条日志"：整条待办 INSERT 失败 ⇒ 这条订阅的补做动作
	// 再也不会被重试 ⇒ 退款/停用后账号永久留在节点上继续可用。
	reason = truncateRunes(reason, 300)
	_, err := s.db.Exec(ctx, `INSERT INTO subscription_pending_node_actions
		(subscription_id, action, then_delete, next_attempt_at, last_error, register_revoked_order)
		VALUES ($1::uuid, $2, $3, now() + interval '2 minutes', $4, $5)
		ON CONFLICT (subscription_id) DO UPDATE SET
			action = CASE WHEN subscription_pending_node_actions.action = 'revoke' THEN 'revoke' ELSE EXCLUDED.action END,
			then_delete = subscription_pending_node_actions.then_delete OR EXCLUDED.then_delete,
			register_revoked_order = subscription_pending_node_actions.register_revoked_order OR EXCLUDED.register_revoked_order,
			next_attempt_at = LEAST(subscription_pending_node_actions.next_attempt_at, EXCLUDED.next_attempt_at),
			last_error = EXCLUDED.last_error`, subID, action, thenDelete, reason, registerRevoked)
	if err != nil {
		s.logger.Error("pending node action: could not queue; enforcement will not be retried",
			"subscription", shortID(subID), "action", action, "error", err)
		return err
	}
	s.logger.Warn("pending node action queued", "subscription", shortID(subID), "action", action, "reason", reason)
	return nil
}

// cancelPendingNodeAction 订阅重新生效时调用。使用脱离请求取消的有界上下文，防止浏览器断开残留旧停用待办。
func (s *Server) cancelPendingNodeAction(ctx context.Context, subID string) {
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.Exec(bctx, `DELETE FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, subID); err != nil {
		s.logger.Warn("pending node action: cancel failed", "subscription", shortID(subID), "error", err)
	}
}

// drainPendingNodeActions 由 sweeper 每轮调用。
func (s *Server) drainPendingNodeActions(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT subscription_id::text, action, then_delete, register_revoked_order
		FROM subscription_pending_node_actions WHERE next_attempt_at <= now()
		ORDER BY next_attempt_at LIMIT $1`, pendingActionBatch)
	if err != nil {
		s.logger.Warn("pending node action: select failed", "error", err)
		return
	}
	type item struct {
		id, action    string
		del, register bool
	}
	var due []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.action, &it.del, &it.register); err != nil {
			rows.Close()
			s.logger.Warn("pending node action: scan failed", "error", err)
			return
		}
		due = append(due, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("pending node action: iteration failed", "error", err)
		return
	}
	rows.Close()
	for _, it := range due {
		if ctx.Err() != nil {
			return
		}
		s.runPendingNodeAction(ctx, it.id, it.action, it.del, it.register)
	}
}

func (s *Server) runPendingNodeAction(ctx context.Context, subID, action string, thenDelete, registerRevoked bool) {
	// 2026-11-04：整个动作持有订阅锁，并在锁内重读。原来只在开头读一次就判断「仍未生效」，
	// 之后回收 / 停用 / 软删都在锁外：商城在这期间续费（启用、删待办、重新开户）后，
	// 这里仍会把刚续费的订阅软删、或把刚恢复的账号停掉，而待办已被续费删除，没有任何补救。
	lockedCtx, release, lockErr := s.lockSubscription(ctx, subID)
	if lockErr != nil {
		s.bumpPendingNodeAction(ctx, subID, "订阅正在被其它操作处理："+lockErr.Error())
		return
	}
	defer release()
	ctx = lockedCtx
	rec, err := s.loadSubscriptionByID(ctx, subID)
	if err != nil {
		// 行已被删除：映射也随之不在，没有可做的了（删除路径本身会先确认回收）。
		if errors.Is(err, pgx.ErrNoRows) {
			s.cancelPendingNodeAction(ctx, subID)
			return
		}
		s.bumpPendingNodeAction(ctx, subID, "订阅读取失败："+err.Error())
		return
	}
	// 续费或重新启用：动作作废，绝不能停掉/删掉正在使用的账号。
	if rec.Enabled && !rec.expired(time.Now()) {
		s.cancelPendingNodeAction(ctx, subID)
		s.logger.Info("pending node action cancelled: subscription active again", "subscription", shortID(subID), "action", action)
		return
	}
	if len(rec.nodeAccounts) == 0 && action == pendingActionRevoke {
		s.finishPendingNodeAction(ctx, rec, thenDelete, registerRevoked)
		return
	}
	switch action {
	case pendingActionRevoke:
		outcome := s.revokeSubscriptionAccounts(ctx, rec)
		if !outcome.OK() {
			reasons := make([]string, 0, len(outcome.Failed))
			for _, f := range outcome.Failed {
				reasons = append(reasons, f.Reason)
			}
			s.bumpPendingNodeAction(ctx, subID, strings.Join(reasons, "；"))
			return
		}
		s.finishPendingNodeAction(ctx, rec, thenDelete, registerRevoked)
	case pendingActionDisable:
		outcome := s.applySubscriptionNodeStatus(ctx, rec, false)
		// 2026-11-12：失败的只剩已删除节点的残留映射时不再重试（与两个登记入口同一判据 onlyGone）。
		// 原来这里只看 allOK：那台机器已经不在了，永远切不成功，待办就每小时拿一次订阅锁、
		// 向全部在役节点重发一遍 set_status，直到订阅被删除或重新启用。映射本身保留，回收时仍会移交待补删。
		if !outcome.allOK() && !outcome.onlyGone() {
			parts := make([]string, 0, len(outcome.Failed))
			for name, reason := range outcome.Failed {
				parts = append(parts, name+"："+reason)
			}
			s.bumpPendingNodeAction(ctx, subID, strings.Join(parts, "；"))
			return
		}
		s.cancelPendingNodeAction(ctx, subID)
		if outcome.onlyGone() {
			s.logger.Warn("pending node action closed: only deleted nodes remain in the mapping; not retrying",
				"subscription", shortID(subID), "deleted_nodes", outcome.Gone)
			return
		}
		s.logger.Info("pending node action done: accounts disabled on every node", "subscription", shortID(subID))
	default:
		s.cancelPendingNodeAction(ctx, subID)
	}
}

func (s *Server) finishPendingNodeAction(ctx context.Context, rec *subscriptionRecord, thenDelete, registerRevoked bool) {
	if thenDelete {
		// 兜底条件：只软删仍未生效的行（续费后的行绝不能被这里删掉）。
		tag, err := s.db.Exec(ctx, `UPDATE subscription_tokens SET deleted_at=now() WHERE id=$1::uuid AND deleted_at IS NULL
		   AND NOT (enabled AND (expires_at IS NULL OR expires_at > now()))`, rec.ID)
		if err != nil {
			s.bumpPendingNodeAction(ctx, rec.ID, "节点已回收，但标记删除失败："+err.Error())
			return
		}
		// 2026-11-06：商城撤单（回收后删除）在后台补完时同样登记撤销的订单号，
		// 否则同一 order_id 的重试下单会再开一份订阅（见迁移 056）。只登记商城撤单来源的待办（迁移 058）：
		// 订阅中心手动删除不是退款，买家之后续费重新下单必须照常可用。
		if registerRevoked && tag.RowsAffected() == 1 && strings.TrimSpace(rec.OrderID) != "" {
			if _, err := s.db.Exec(ctx, `INSERT INTO store_revoked_orders(order_id, subscription_id) VALUES($1, $2::uuid) ON CONFLICT (order_id) DO NOTHING`, rec.OrderID, rec.ID); err != nil {
				s.logger.Error("pending node action: 登记已撤销订单号失败，同一订单号重新下单会再开通", "subscription", shortID(rec.ID), "order_id", rec.OrderID, "error", err)
			}
		}
	}
	s.cancelPendingNodeAction(ctx, rec.ID)
	s.logger.Info("pending node action done: accounts revoked on every node", "subscription", shortID(rec.ID), "deleted", thenDelete)
}

// bumpPendingNodeAction 记一次失败并推迟下次尝试：2 分钟起翻倍，封顶 1 小时。
func (s *Server) bumpPendingNodeAction(ctx context.Context, subID, reason string) {
	// 同 queuePendingNodeAction：按字符截断，避免非法 UTF-8 让 UPDATE 失败
	// （失败意味着退避不生效，每 2 分钟重试一次，且 last_error 永远是旧的）。
	reason = truncateRunes(reason, 300)
	var attempts int
	err := s.db.QueryRow(ctx, `UPDATE subscription_pending_node_actions SET
			attempts = attempts + 1,
			next_attempt_at = now() + LEAST(interval '1 hour', interval '2 minutes' * power(2, LEAST(attempts, 5))),
			last_error = $2
		WHERE subscription_id=$1::uuid RETURNING attempts`, subID, reason).Scan(&attempts)
	if err != nil {
		s.logger.Warn("pending node action: bump failed", "subscription", shortID(subID), "error", err)
		return
	}
	if attempts == 1 || attempts%12 == 0 {
		s.logger.Warn("pending node action still incomplete; will retry",
			"subscription", shortID(subID), "attempts", attempts, "error", reason)
	}
}

// revokeExpiredInBackground 拉订阅/打开交付页时发现已过期，就把回收放到后台做，请求立刻返回。
// 同一订阅同时只跑一个；sweeper 每 2 分钟也会兜底，所以这里失败不需要再记待办。
var expiredRevokeInFlight sync.Map

func (s *Server) revokeExpiredInBackground(rec *subscriptionRecord) {
	if rec == nil {
		return
	}
	if _, busy := expiredRevokeInFlight.LoadOrStore(rec.ID, struct{}{}); busy {
		return
	}
	snapshot := *rec
	go func() {
		defer expiredRevokeInFlight.Delete(snapshot.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 2*provisionTimeout)
		defer cancel()
		outcome := s.revokeSubscriptionAccounts(ctx, &snapshot)
		if !outcome.OK() && (outcome.Revoked > 0 || !allBackedOff(outcome)) {
			s.logger.Warn("subscription: 过期即时收回不完整，留给 sweeper 重试",
				"subscription", shortID(snapshot.ID), "revoked", outcome.Revoked, "failed", len(outcome.Failed))
		} else if outcome.OK() && outcome.Revoked > 0 {
			s.logger.Info("subscription: 过期即时收回完成", "subscription", shortID(snapshot.ID), "revoked", outcome.Revoked)
		}
	}()
}
