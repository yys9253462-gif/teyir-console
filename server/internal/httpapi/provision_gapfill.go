package httpapi

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// provision_gapfill.go — 新节点主动给存量订阅开户（2026-10-28）。
//
// 原来给订阅补开新节点账号只有一个入口：买家客户端拉取订阅时（subscriptionFeed 的 JIT）。
// 不常更新订阅的买家就一直拿不到新节点 —— 实测 10-27 新加的大阪 / 东京③ / 香港，
// 18 个有效订阅里只有 7 个开了户，缺的 11 个都是 10-27 之后没拉过订阅的。
//
// 这里不新写开户逻辑，只负责「把缺户的订阅排进同步队列」：
// subscription_sync_jobs 由 StartSubscriptionSync 每 30 秒取 3 条执行（synchronizeSubscription，
// 持订阅锁、失败按退避重试），与 JIT、续费走的是同一条路径。
//
// 触发时机：新建节点、节点重新上架、以及每小时一次兜底（节点 API 当时不通、
// 或别的路径漏掉的，都在下一小时补上）。

var gapFillInFlight sync.Mutex

// gapFillRerun 记录「进行中时又来了一次触发」。
//
// 2026-11-04：原来 TryLock 失败就直接丢弃这次触发。整点兜底正在用旧的节点清单跑时，
// 新建 / 重新上架节点触发的那次会被吞掉，买家要等到下一个整点才开户。现在记下来，跑完再补跑一轮。
var gapFillRerun atomic.Bool

// gapFillBeforeUnlock 测试钩子：在「最后一次检查 rerun」与「解锁」之间执行。
var gapFillBeforeUnlock = func() {}

// gapFillOnce 便于测试替换真实的一轮补开户。
var gapFillOnce = (*Server).queueProvisioningGapsOnce

// queueProvisioningGaps 把所有「生效中、且在某个上架节点上缺账号」的订阅排进同步队列，返回排队数。
func (s *Server) queueProvisioningGaps(ctx context.Context, reason string) int {
	queued := 0
	for {
		if !gapFillInFlight.TryLock() {
			gapFillRerun.Store(true)
			return queued
		}
		for {
			gapFillRerun.Store(false)
			queued += gapFillOnce(s, ctx, reason)
			if !gapFillRerun.Load() || ctx.Err() != nil {
				break
			}
			reason += "（补跑：进行中时有新的触发）"
		}
		gapFillBeforeUnlock()
		gapFillInFlight.Unlock()
		// 2026-11-04：解锁之后再看一次。在「最后一次检查 rerun」与「解锁」之间到来的触发会因
		// TryLock 失败而只置位 rerun —— 原来这里直接返回，那次触发就要等到下一个整点。
		if !gapFillRerun.Load() || ctx.Err() != nil {
			return queued
		}
		reason += "（补跑：进行中时有新的触发）"
	}
}

func (s *Server) queueProvisioningGapsOnce(ctx context.Context, reason string) int {
	build, err := s.subscriptionBuildFor(ctx)
	if err != nil {
		s.logger.Warn("provision gap-fill: build failed", "reason", reason, "error", err)
		return 0
	}
	recs, err := s.listSubscriptions(ctx)
	if err != nil {
		s.logger.Warn("provision gap-fill: list failed", "reason", reason, "error", err)
		return 0
	}
	queued := 0
	for i := range recs {
		rec := &recs[i]
		if !s.needsAccountProvisioning(ctx, build.Nodes, rec) {
			continue
		}
		if err := s.queueSubscriptionSync(ctx, rec.ID); err != nil {
			s.logger.Warn("provision gap-fill: queue failed", "subscription", shortID(rec.ID), "error", err)
			continue
		}
		queued++
	}
	// 0 也记一条：否则「跑了但没东西可补」和「根本没跑」从日志上分不出来。
	s.logger.Info("provision gap-fill done", "reason", reason, "subscriptions", len(recs), "nodes", len(build.Nodes), "queued", queued)
	return queued
}

// ProvisioningGapReport 只读诊断：每个订阅按 gap-fill 的判据缺哪些节点（--gapfill-dry-run 用）。
func (s *Server) ProvisioningGapReport(ctx context.Context) map[string]any {
	build, err := s.subscriptionBuildFor(ctx)
	if err != nil {
		return map[string]any{"error": "build: " + err.Error()}
	}
	recs, err := s.listSubscriptions(ctx)
	if err != nil {
		return map[string]any{"error": "list: " + err.Error()}
	}
	nodes := []string{}
	for _, n := range build.Nodes {
		nodes = append(nodes, n.Name+"|"+shortID(n.ID)+"|in_store="+map[bool]string{true: "y", false: "n"}[n.InStore])
	}
	subs := []map[string]any{}
	for i := range recs {
		rec := &recs[i]
		missing := []string{}
		if scope, err := s.categoryScopeFor(ctx, rec); err != nil {
			missing = append(missing, "（范围解析失败："+err.Error()+"）")
		} else {
			for _, n := range missingAccountNodes(build.Nodes, rec, scope) {
				missing = append(missing, n.Name)
			}
		}
		subs = append(subs, map[string]any{"id": shortID(rec.ID), "enabled": rec.Enabled, "expired": rec.expired(time.Now()),
			"accounts": len(rec.nodeAccounts), "missing": missing, "needs": s.needsAccountProvisioning(ctx, build.Nodes, rec)})
	}
	return map[string]any{"nodes": nodes, "subscriptions": subs}
}

// queueProvisioningGapsAsync 给请求处理函数用：不阻塞响应，用独立的 context（请求结束后照常跑完）。
func (s *Server) queueProvisioningGapsAsync(reason string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.queueProvisioningGaps(ctx, reason)
	}()
}
