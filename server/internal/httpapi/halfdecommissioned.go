package httpapi

// halfdecommissioned.go —— 「半下线」状态的巡检与自动清理（2026-10-24 加）。
//
// 什么是半下线
// ------------
// 一台节点被删（软删 hysteria_nodes.deleted_at）之后，控制台里它的**记录**没了，
// 但**订阅里的买家账号映射**（subscription_tokens.node_accounts）可能还指着它。
// 此时的状态是「半下线」：
//
//	节点侧：已删除，账号可能还留在机器上能连（机器没退租的话）
//	控制台侧：以为已清理干净，实际映射悬着
//	订阅侧：到期 sweeper 收回时永远失败（loadPortalTarget 对已删节点返回 notFound）
//
// 后果（2026-10-23 美国-阿什本实测）
// --------------------------------
// 一台已软删的节点下挂着 15 条活跃订阅（其中 14 条是商城付费订单）的买家账号。
// orphan_node_accounts 里该节点的记录是 **0 条** —— 说明撤销流程从未被触发。
// 而 manage.go 里 node_subscription_lingering 的检测 SQL 带
// `AND n.deleted_at IS NULL`，**恰好把这类情况排除在外** ——
// 于是「映射指向已删节点」这件事，在整个系统里没有任何一处能发现。
//
// 兜底为什么不够
// --------------
// provisioning.go 的 handOffDeletedNodeAccount 是兜底，但它只在
// **订阅到期 sweeper** 里被调用（最早到期 10-31），意味着这类残留要挂近一个月。
// 而到期 sweeper 的兜底还要求订阅**已到期**才走 —— 未到期的活跃订阅就一直挂着。
//
// 这里的做法
// ----------
// 主动巡检：找出「订阅映射指向已软删节点」的所有组合，
// 自动登记为 orphan_node_accounts（留痕，可在「节点回收」里查到），
// 并摘除订阅映射（让到期 sweeper 的收回流程能正常完成）。
//
// ⚠️ 为什么不自动物理注销买家账号
// ------------------------------
// 节点已删、凭据已随删除抹除（api_url/api_key 被置 NULL），
// **没有地址也没有 key，无法连接节点去删账号**。这不是"漏做"，是物理上做不了。
// 所以登记为 orphan（结案留痕）而不是假装成功 ——
// 这正是 deleteNode 的 decommissioned=1 分支所表达的同一件事。

import (
	"context"
	"fmt"
)

// halfDecommissionedReason 是登记孤儿时写的原因。
// halfDecommissionedResolvedNote 半下线孤儿自动结案时写入的说明。
const halfDecommissionedResolvedNote = "半下线：节点已删除且凭据已抹除，无法注销，自动结案"

const halfDecommissionedReason = "订阅映射指向已软删节点（半下线）：节点已删除且凭据已抹除，无法连接节点注销买家账号，已直接结案"

// halfDecommissionedRef 是一条半下线记录。
type halfDecommissionedRef struct {
	NodeID      string
	NodeName    string
	Subscription string
	UserID      string
}

// detectHalfDecommissioned 找出「订阅映射指向已软删节点」的组合。
//
// 只处理 node_accounts 里**确实挂着买家账号**的节点 ——
// 映射里没有 user_id 的条目（理论上不该有）跳过，因为无从登记孤儿。
//
// 只处理**未过期未删除**的订阅：已过期的由 sweeper 处理，
// 已删除的（deleted_at 非空）留着映射无意义但也无害，不去动它 ——
// 改动越少越好，尤其是在生产库上。
func (s *Server) detectHalfDecommissioned(ctx context.Context) ([]halfDecommissionedRef, error) {
	rows, err := s.db.Query(ctx, `
SELECT hn.id::text, hn.name, st.id::text,
       COALESCE(st.node_accounts->hn.id::text->>'user_id', '')
FROM subscription_tokens st
JOIN hysteria_nodes hn ON st.node_accounts ? hn.id::text
WHERE st.deleted_at IS NULL
  AND hn.deleted_at IS NOT NULL
  AND st.node_accounts->hn.id::text->>'user_id' <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []halfDecommissionedRef
	for rows.Next() {
		var r halfDecommissionedRef
		if err := rows.Scan(&r.NodeID, &r.NodeName, &r.Subscription, &r.UserID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sweepHalfDecommissioned 巡检并清理半下线状态。
//
// 返回被清理的条目数。清理动作：
//  1. 登记为 orphan_node_accounts（留痕，可查、可人工结案）
//  2. 摘除该节点在订阅里的映射
//
// 为什么两步必须配套：recordOrphanAccounts 内部已经会摘除映射，
// 但它只在「登记成功」时才摘（登记失败则保留映射，至少还有一处线索）。
// 这里额外显式检查映射是否真的被摘掉了，摘不掉就记日志 ——
// 否则就是「以为清了其实没清」，正是这套代码一直在防的那类病。
//
// 返回的 error 只表示「整轮巡检失败」（例如数据库查不通），
// 单条清理失败不算失败 —— 下一轮还会再试。
func (s *Server) sweepHalfDecommissioned(ctx context.Context) (int, error) {
	refs, err := s.detectHalfDecommissioned(ctx)
	if err != nil {
		return 0, err
	}
	if len(refs) == 0 {
		return 0, nil
	}

	// 按节点聚合：一个节点可能挂着多条订阅的账号，
	// 登记孤儿时一次插多条更省事，也便于在「节点回收」里按节点看。
	byNode := map[string][]halfDecommissionedRef{}
	var order []string
	for _, r := range refs {
		if _, ok := byNode[r.NodeID]; !ok {
			order = append(order, r.NodeID)
		}
		byNode[r.NodeID] = append(byNode[r.NodeID], r)
	}

	cleaned := 0
	for _, nodeID := range order {
		group := byNode[nodeID]
		nodeName := group[0].NodeName

		var failures []revokeFailure
		for _, r := range group {
			failures = append(failures, revokeFailure{
				UserID: r.UserID,
				SubID:  r.Subscription,
				Reason: halfDecommissionedReason,
			})
		}

		// 凭据快照传 nil：节点已软删，api_url/api_key 已被 deleteNode 抹成 NULL，
		// 取不到也不该取 —— 拿不到凭据就意味着无法物理注销账号，
		// 这正是 orphan 存在的意义（留痕等人工处理或直接结案）。
		s.recordOrphanAccounts(ctx, nodeID, nodeName, "", nil, failures)
		// 凭据已抹除，这些账号不可能再被物理注销 —— 登记即结案（2026-10-30 修）。
		// 原来只在 reason 里写「已直接结案」却没设 resolved_at，它们永远挂在
		// 「未结案孤儿」里，把真正需要人工处理的孤儿淹没（实测积了 13 条）。
		// 2026-11-04：只结案**没有凭据快照**的记录。登记是 upsert：同一节点同一用户已有一条待补删
		// （删除节点时带着 key 快照登记的）时，这里会把它的 reason 改成半下线原因、保留原来的 key ——
		// 原来随即按 reason 结案，一条明明还能「补删」的记录就从待办里消失，买家账号留在节点上。
		if _, err := s.db.Exec(ctx, `UPDATE orphan_node_accounts SET resolved_at=now(), resolved_note=$3
			WHERE node_id=$1::uuid AND reason=$2 AND resolved_at IS NULL AND api_key_cipher IS NULL`,
			nodeID, halfDecommissionedReason, halfDecommissionedResolvedNote); err != nil {
			s.logger.Warn("半下线清理: 孤儿结案失败", "node", nodeName, "error", err)
		}

		// 核对映射是否真的摘掉了 —— recordOrphanAccounts 只在登记成功时摘，
		// 登记失败会保留映射。这种情况下必须让人知道，而不是静默当作已清理。
		for _, r := range group {
			var still bool
			if err := s.db.QueryRow(ctx,
				`SELECT COALESCE(node_accounts,'{}'::jsonb) ? $1 FROM subscription_tokens WHERE id=$2::uuid`,
				r.NodeID, r.Subscription).Scan(&still); err != nil {
				s.logger.Warn("半下线清理: 核对映射失败", "node", nodeName, "error", err)
				continue
			}
			if still {
				s.logger.Warn("半下线清理: 映射未能摘除，保留待下轮重试",
					"node", nodeName, "subscription", shortID(r.Subscription), "user_id", r.UserID)
				continue
			}
			cleaned++
		}
	}

	if cleaned > 0 {
		InvalidateSubscriptionCache()
		s.logger.Warn("半下线巡检: 发现并清理了指向已删节点的订阅映射",
			"nodes", len(order), "entries", cleaned)
		// 只在真的清理了映射时推送：半下线是删除节点后的残留，正常情况下极少出现，
		// 不会刷屏；而只写日志等于没人知道有买家账号被直接结案。
		s.telegramNotifyAsync("🧹 半下线巡检\n" + halfDecommissionedSummary(cleaned, len(order)))
	}
	return cleaned, nil
}

// halfDecommissionedSummary 生成给运维看的摘要，用于告警推送。
func halfDecommissionedSummary(cleaned int, nodes int) string {
	return fmt.Sprintf("发现 %d 个已下线节点仍挂着 %d 条订阅账号映射，已自动登记为孤儿并摘除映射（节点凭据已抹除，无法连接节点注销买家账号）", nodes, cleaned)
}
