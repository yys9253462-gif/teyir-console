package httpapi

// monitoring_schedule.go — 后台周期执行：三网测速 + 网络延迟探测。
//
// 为什么放在 httpapi 包而不是 collector
// -------------------------------------
// collector 的职责是"采集主机指标"（CPU/内存/流量），它每 30 秒跑一次、
// 用一条 SSH 会话做完所有事。而测速是**重操作**（12 个靶点 × 3 个包，
// 最坏 72 秒），绝不能用同样的频率跑 —— 那会让节点持续处于被 ping 的
// 状态，也会把 api 容器的内存压满（mem_limit 256m）。
//
// 所以单独一个低频调度器，默认 15 分钟一轮，且与 ENABLE_COLLECTOR 解耦：
// 用户可能想关掉指标采集（比如节点太多、采集太重），但仍想看三网质量。

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"nodeops/server/internal/speedtest"
)

// StartMonitoringScheduler 启动后台监控循环。
//
// interval 是测速周期；<=0 时用默认 15 分钟。传 0 且明确要禁用时，
// 调用方不该调这个方法（而不是靠传 0 来"禁用"，那样语义含糊）。
func (s *Server) StartMonitoringScheduler(ctx context.Context, interval time.Duration) {
	if s == nil || s.prober == nil {
		s.logger.Info("monitoring scheduler disabled: no prober configured")
		return
	}
	if interval <= 0 {
		// 15 分钟的理由：三网线路质量是**慢变量**（除非运营商调整路由，
		// 否则一天内基本不变）。跑得更勤只会增加节点负担而不带来新信息。
		// 想立刻看结果用「立即测速」按钮。
		interval = 15 * time.Minute
	}

	go func() {
		runSafe := func() {
			defer func() {
				if rec := recover(); rec != nil && s.logger != nil {
					s.logger.Error("monitoring: 后台监控周期任务发生异常恢复", "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			s.runMonitoringRound(ctx)
		}
		// 启动后先等 90 秒再跑第一轮。
		//
		// 为什么不在启动时立刻跑：面板重启往往发生在部署/故障时，
		// 那一刻最重要的事情是让 API 尽快可用，而不是让 6 台节点的
		// SSH 会话同时建立、抢内存和连接数。等 90 秒，让服务先稳定下来。
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}

		runSafe()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runSafe()
			}
		}
	}()
	s.logger.Info("monitoring scheduler started", "interval", interval.String())
}

// runMonitoringRound 跑一轮：先测网络延迟（快），再测三网（慢）。
func (s *Server) runMonitoringRound(ctx context.Context) {
	roundCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// 顺序刻意的：延迟探测很快（每台几百毫秒），先把这部分数据刷新，
	// 即使后面三网测速超时，用户看到的延迟也是新的。
	s.probeNetworkLatency(roundCtx)
	s.runSpeedtestRound(roundCtx)
}

// probeNetworkLatency 测「面板 → 各主机」的纯 TCP 建连耗时。
//
// 与 collector 的 SSH 握手耗时区分开：这里只做 TCP 三次握手，
// 不建 SSH 会话。意义在于 —— 它是"网络可达性与质量"的度量，
// 不受对方 sshd 负载、认证耗时的影响。SSH 端口被封但代理正常的节点，
// 在这里会明确显示"不可达"，而它的服务状态由别的探测负责。
func (s *Server) probeNetworkLatency(ctx context.Context) {
	rows, err := s.db.Query(ctx, `
SELECT id, name, host, ssh_port FROM servers WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		s.logger.Warn("monitoring: 读取主机失败", "error", err)
		return
	}
	type hostRow struct {
		id   string
		name string
		host string
		port int
	}
	var hosts []hostRow
	for rows.Next() {
		var h hostRow
		if err := rows.Scan(&h.id, &h.name, &h.host, &h.port); err != nil {
			rows.Close()
			s.logger.Warn("monitoring: 主机列表读取不完整，跳过本轮延迟探测", "error", err)
			return
		}
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("monitoring: 主机列表读取不完整，跳过本轮延迟探测", "error", err)
		return
	}
	rows.Close()

	for _, h := range hosts {
		if ctx.Err() != nil {
			return
		}
		latency, err := speedtest.DialOnly(ctx, h.host, h.port, 5*time.Second)
		if err != nil {
			// 探测失败时**写入 NULL 而不是 0 或保留旧值**。
			//
			// 保留旧值会让界面显示一个陈旧但看起来合理的数字 ——
			// 用户不知道它其实是 10 分钟前的。写 NULL 则明确表达
			// "这次没测到"，前端显示"未测得"，这是诚实的。
			if _, dbErr := s.db.Exec(ctx, `
UPDATE servers SET net_latency_ms = NULL, net_latency_checked_at = now() WHERE id = $1::uuid`, h.id); dbErr != nil {
				s.logger.Warn("monitoring: 写入延迟失败", "server", h.name, "error", dbErr)
			}
			continue
		}
		if _, dbErr := s.db.Exec(ctx, `
UPDATE servers SET net_latency_ms = $2, net_latency_checked_at = now() WHERE id = $1::uuid`, h.id, latency); dbErr != nil {
			s.logger.Warn("monitoring: 写入延迟失败", "server", h.name, "error", dbErr)
		}
	}
}

// runSpeedtestRound 跑一轮三网测速：有 SSH 凭据的主机走 SSH 探针，
// 没有凭据但绑了节点 API 的节点走 portal 通道（节点自己 ping）。
//
// 2026-11-03：原来定时轮次只查有 SSH 凭据的主机，portal 通道只在手动「立即测速」里有。
// 「节点是节点、主机是主机」之后大多数节点只接 API、不录主机凭据 —— 它们只有手点才有数据，
// node_china_loss 要求 1 小时内至少 2 轮，于是这些节点永远不会触发回国丢包告警，
// 7 天后历史数据也过期清空。现在与手动触发同一套选路：SSH 优先，portal 兜底。
func (s *Server) runSpeedtestRound(ctx context.Context) {
	if s.prober == nil {
		return
	}
	rows, err := s.db.Query(ctx, `
SELECT s.id, s.name, n.id::text, n.name, COALESCE(n.api_url, ''), n.api_key,
       EXISTS (SELECT 1 FROM server_credentials x WHERE x.server_id = s.id)
FROM servers s
JOIN hysteria_nodes n ON n.server_id = s.id AND n.deleted_at IS NULL
WHERE s.deleted_at IS NULL
ORDER BY s.name, n.name`)
	if err != nil {
		s.logger.Warn("monitoring: 读取测速目标失败", "error", err)
		return
	}
	type portalJob struct {
		nodeID, nodeName, serverID, baseURL, apiKey string
	}
	type job struct {
		serverID   string
		serverName string
		nodeIDs    []string
		// fallback 是这台主机上绑了节点 API 的节点：SSH 测速失败（凭据失效、指纹未登记、sshd 不可达）时
		// 改由节点自己测（2026-11-06）。原来只在「没有凭据」时走 portal，「凭据坏了」不兜底 ——
		// 这些节点永远没有三网数据，回国丢包告警也就永远不会触发。
		fallback []portalJob
	}
	var jobs []job
	var portalJobs []portalJob
	serverIdx := map[string]int{}
	for rows.Next() {
		var serverID, serverName, nodeID, nodeName, baseURL string
		var ciphertext []byte
		var hasCred bool
		if err := rows.Scan(&serverID, &serverName, &nodeID, &nodeName, &baseURL, &ciphertext, &hasCred); err != nil {
			rows.Close()
			scheduleFailed(ctx)
			s.logger.Warn("monitoring: 测速目标列表读取不完整，跳过本轮测速", "error", err)
			return
		}
		var pj *portalJob
		if baseURL != "" && len(ciphertext) > 0 && s.box != nil {
			if plain, err := s.box.Open(ciphertext); err == nil {
				apiKey := string(plain)
				clear(plain)
				pj = &portalJob{nodeID: nodeID, nodeName: nodeName, serverID: serverID, baseURL: baseURL, apiKey: apiKey}
			} else if !hasCred {
				s.logger.Warn("monitoring: 节点 API 密钥解密失败，跳过该节点测速", "node", nodeName)
			}
		}
		if hasCred {
			i, ok := serverIdx[serverID]
			if !ok {
				i = len(jobs)
				serverIdx[serverID] = i
				jobs = append(jobs, job{serverID: serverID, serverName: serverName})
			}
			jobs[i].nodeIDs = append(jobs[i].nodeIDs, nodeID)
			if pj != nil {
				jobs[i].fallback = append(jobs[i].fallback, *pj)
			}
			continue
		}
		if pj != nil {
			portalJobs = append(portalJobs, *pj)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		scheduleFailed(ctx)
		s.logger.Warn("monitoring: 测速目标列表读取不完整，跳过本轮测速", "error", err)
		return
	}
	rows.Close()
	total := len(jobs) + len(portalJobs)
	if total == 0 {
		return
	}

	// 并发度与手动触发一致（4），避免两处行为不同。
	sem := make(chan struct{}, maxProbeConcurrency)
	done := make(chan struct{}, total)
	for _, j := range jobs {
		go func(j job) {
			defer func() { done <- struct{}{} }()
			defer func() {
				if rec := recover(); rec != nil && s.logger != nil {
					s.logger.Error("monitoring: 主机定时测速子协程异常恢复", "server", j.serverName, "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if _, err := s.prober.RunOnServer(ctx, j.serverID, j.nodeIDs); err != nil {
				s.logger.Warn("monitoring: 测速失败", "server", j.serverName, "error", err, "portal_fallback", len(j.fallback))
				for _, f := range j.fallback {
					if ctx.Err() != nil {
						return
					}
					if err := s.prober.RunOnPortalAndStore(ctx, f.nodeID, f.serverID, f.baseURL, f.apiKey); err != nil {
						s.logger.Warn("monitoring: SSH 测速失败后改走节点 portal 也失败", "node", f.nodeName, "error", err)
					}
				}
			}
		}(j)
	}
	for _, j := range portalJobs {
		go func(j portalJob) {
			defer func() { done <- struct{}{} }()
			defer func() {
				if rec := recover(); rec != nil && s.logger != nil {
					s.logger.Error("monitoring: 门户定时测速子协程异常恢复", "node", j.nodeName, "panic", rec, "stack", string(debug.Stack()))
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := s.prober.RunOnPortalAndStore(ctx, j.nodeID, j.serverID, j.baseURL, j.apiKey); err != nil {
				s.logger.Warn("monitoring: 节点 portal 测速失败", "node", j.nodeName, "error", err)
			}
		}(j)
	}
	for i := 0; i < total; i++ {
		<-done
	}
}

// 保留对 logger 的显式依赖，避免 s.logger 为 nil 时 panic。
var _ = slog.Default
