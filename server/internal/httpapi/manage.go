package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"nodeops/server/internal/auth"

	"github.com/jackc/pgx/v5"
)

type alertKey struct {
	kind     string
	serverID string
	nodeID   string
}

type alertCondition struct {
	key      alertKey
	severity string
}

// alerts serves the alert centre. It is deliberately read-only: deriving and
// persisting alert rows used to happen inside this GET handler, which meant every
// 15-second poll from every open browser wrote to the alerts table and a plain
// read could change state. Reconciliation now runs in the collector loop
// (ReconcileAlerts); this handler only reads what that pass has persisted, and
// falls back to computing live conditions when the collector is disabled so the
// UI still shows something truthful.
func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	ctx := r.Context()

	includeResolved := r.URL.Query().Get("include") == "resolved"
	list, err := s.db.Query(ctx, `
SELECT a.id, a.type, a.severity, a.status, a.first_seen_at, a.last_seen_at,
       COALESCE(a.resolved_at, 'epoch'::timestamptz) AS resolved_at,
       s.name, s.host, COALESCE(n.name, ''), COALESCE(n.listen_port, 0),
       COALESCE(n.id::text, '')
FROM alerts a
JOIN servers s ON s.id = a.server_id
LEFT JOIN hysteria_nodes n ON n.id = a.node_id
WHERE (a.status <> 'resolved' OR $1) AND s.deleted_at IS NULL
ORDER BY CASE a.status WHEN 'open' THEN 0 WHEN 'acknowledged' THEN 1 ELSE 2 END,
         CASE a.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, a.last_seen_at DESC
LIMIT 200`, includeResolved)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer list.Close()

	result := make([]map[string]any, 0)
	for list.Next() {
		var id, kind, severity, status, serverName, host, nodeName, nodeID string
		var listenPort int
		var firstSeen, lastSeen, resolvedAt time.Time
		if err := list.Scan(&id, &kind, &severity, &status, &firstSeen, &lastSeen, &resolvedAt, &serverName, &host, &nodeName, &listenPort, &nodeID); err != nil {
			s.databaseError(w, r, err)
			return
		}
		var resolvedAny any
		if status == "resolved" {
			resolvedAny = resolvedAt
		}
		result = append(result, map[string]any{
			"id": id, "type": kind, "severity": severity, "status": status,
			"server_name": serverName, "host": host, "node_name": nodeName, "node_port": listenPort,
			// 2026-11-08：告警页「确认机器已下线」按钮要按节点删除。
			"node_id":       nodeID,
			"message":       describeAlert(kind, serverName, host, nodeName, listenPort),
			"first_seen_at": firstSeen, "last_seen_at": lastSeen, "resolved_at": resolvedAny,
		})
	}
	if err := list.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// alertEvents returns one alert's timeline, newest first. The panel shows this
// under an expanded alert so an operator can tell "it fired once and I fixed it"
// apart from "it has been flapping for two hours", which look identical when all
// you have is first_seen_at and last_seen_at.
func (s *Server) alertEvents(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	alertID := r.PathValue("id")
	rows, err := s.db.Query(r.Context(), `
SELECT e.id, e.event_type, e.details, e.created_at
FROM alert_events e
WHERE e.alert_id = $1::uuid
ORDER BY e.created_at DESC
LIMIT 100`, alertID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	result := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var eventType string
		var details []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &eventType, &details, &createdAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		var parsed map[string]any
		if err := json.Unmarshal(details, &parsed); err != nil || parsed == nil {
			parsed = map[string]any{}
		}
		result = append(result, map[string]any{
			"id": id, "event_type": eventType, "details": parsed, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ReconcileAlerts derives the current set of alert conditions from live server and
// node state and syncs the alerts table: new conditions are opened, still-active
// ones refreshed, and conditions that no longer hold are resolved. It is exported
// so the collector loop can run it on a schedule instead of on every HTTP GET.
//
// It returns an error only for failures that make the whole pass meaningless; rows
// that fail individually are logged and skipped so one bad row cannot stall alerts.
func (s *Server) ReconcileAlerts(ctx context.Context) error {
	type serverRow struct {
		id, name, host, status string
		hasCredential          bool
		sshBroken              bool
		createdAt              time.Time
		lastMetric             *time.Time
	}
	var serverRows []serverRow
	rows, err := s.db.Query(ctx, `
SELECT s.id, s.name, s.host, s.status,
       EXISTS (SELECT 1 FROM server_credentials sc WHERE sc.server_id = s.id) AS has_credential,
       -- 2026-11-06：SSH 连接失败 / 指纹不一致同样算「SSH 不可用」（采集器改走 API 探测后主机照样 online）。
       -- 原来只认「SSH 凭据失效」：sshd 挂了、面板 IP 被 fail2ban 封时判成 server_stale（critical），
       -- 健康闸门随即把这台主机上好好的节点全部从客户订阅里拿掉。
       (COALESCE(s.status_error, '') LIKE 'SSH 凭据失效%'
        OR COALESCE(s.status_error, '') LIKE 'SSH 连接失败%'
        OR COALESCE(s.status_error, '') LIKE 'SSH 主机指纹%') AS ssh_broken,
       s.created_at,
       (SELECT MAX(sampled_at) FROM server_metrics m WHERE m.server_id = s.id) AS last_metric
FROM servers s WHERE s.deleted_at IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var row serverRow
		if err := rows.Scan(&row.id, &row.name, &row.host, &row.status, &row.hasCredential, &row.sshBroken, &row.createdAt, &row.lastMetric); err != nil {
			rows.Close()
			return err
		}
		serverRows = append(serverRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	type nodeRow struct {
		id, serverID, name, status string
		trafficSuspended           bool
	}
	var nodeRows []nodeRow
	rows, err = s.db.Query(ctx, `
SELECT n.id, n.server_id, n.name, n.status, n.traffic_suspended FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id WHERE s.deleted_at IS NULL AND n.deleted_at IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var row nodeRow
		if err := rows.Scan(&row.id, &row.serverID, &row.name, &row.status, &row.trafficSuspended); err != nil {
			rows.Close()
			return err
		}
		nodeRows = append(nodeRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	conditions := make([]alertCondition, 0, len(serverRows)+len(nodeRows))
	staleAfter := time.Duration(max(s.collectorSeconds*3, 90)) * time.Second
	for _, row := range serverRows {
		switch row.status {
		case "offline":
			conditions = append(conditions, alertCondition{alertKey{"server_offline", row.id, ""}, "critical"})
		case "warning":
			conditions = append(conditions, alertCondition{alertKey{"server_warning", row.id, ""}, "warning"})
		}
		// `unknown` is not merely "we have not looked yet". A host reaches
		// `unknown` in exactly two ways: it was just created and no cycle has
		// finished, or every cycle since has failed. The first is transient and
		// must stay quiet; the second is the single most common way a
		// non-technical operator gets stuck — they added a VPS, the dashboard
		// says "未知", and nothing tells them what to do next. Distinguish the
		// two by whether a credential exists and whether we already have a
		// sample: with a credential but no sample, collection is failing and the
		// operator must be told.
		//
		// Threshold is one full extra cycle beyond the first attempt so a normal
		// deployment (credential uploaded, first sample lands within one tick)
		// never flashes an alert.
		// 新登记主机在首个采集宽限期（staleAfter）内尚未落盘首条采样属于正常过渡，不误报 server_unknown / server_stale。
		pastInitialGrace := time.Since(row.createdAt) > staleAfter
		if row.status == "unknown" && row.hasCredential && ((row.lastMetric == nil && pastInitialGrace) || (row.lastMetric != nil && time.Since(*row.lastMetric) > staleAfter)) {
			conditions = append(conditions, alertCondition{alertKey{"server_unknown", row.id, ""}, "warning"})
		}
		// A host that is reachable but has produced no sample for several cycles
		// means the collector itself is broken, which is otherwise silent.
		//
		// 2026-11-03：SSH 认证失败但节点 API 通时，采集器把主机写成 online 并在
		// status_error 记「SSH 凭据失效」——指标当然不会再来。原来这里照样判
		// server_stale（critical），而健康闸门把 server_stale 当整机故障，
		// 3 分钟后把这台主机上**能正常用的**节点全部从客户订阅里拿掉。
		// 凭据失效要的动作是「重新录入 SSH 凭据」，不是下线节点：单列一条 warning。
		stale := (row.lastMetric == nil && pastInitialGrace) || (row.lastMetric != nil && time.Since(*row.lastMetric) > staleAfter)
		if row.hasCredential && row.status != "offline" && stale && row.sshBroken {
			conditions = append(conditions, alertCondition{alertKey{"server_ssh_broken", row.id, ""}, "warning"})
		} else if row.hasCredential && row.status != "offline" && row.status != "unknown" && stale {
			conditions = append(conditions, alertCondition{alertKey{"server_stale", row.id, ""}, "critical"})
		}
		// 无 SSH 凭据、却显示 online、且从来没有过一条指标（2026-10-24 审计 P2-1）。
		//
		// 这是一条**完全静默**的失效路径，两条判定都漏掉了它：
		//   - server_unknown / server_stale 都带 `row.hasCredential` 守卫，
		//     没凭据就一律不判 —— 因为对"只接 API 的节点"这是正常形态；
		//   - 而 servers.status 会被 probeNodesViaAPI 按"旗下有任一节点 API 通"
		//     写成 online，于是界面显示「在线」。
		//
		// 两者叠加的后果：一台机器**采集能力为零**，界面却说它在线，告警一声不吭。
		// 实测库里 6 台（东京、哥伦布②③、马尼拉、斯德哥尔摩、首尔）全是
		// `有凭据=0` + `server_metrics 0 行` + `status=online`。
		// 运维看到的「6 台在线」里，这几台其实完全没有资源数据。
		//
		// 为什么判据是「从未有过指标」而不是「指标陈旧」：没有凭据意味着
		// 永远不会有指标，用陈旧判据会在第一次采样后就再也不会触发 ——
		// 而现在这个状态是**创建那天就有**的，不是运行中退化的。
		// 之所以不是 critical：机器本身可能是好的（节点 API 通说明它在跑），
		// 缺的是面板对它的可见性。给 warning，敦促补 SSH 凭据或确认这是刻意为之。
		if !row.hasCredential && row.status == "online" && row.lastMetric == nil {
			conditions = append(conditions, alertCondition{alertKey{"server_blind", row.id, ""}, "warning"})
		}
	}
	for _, row := range nodeRows {
		// 流量超额停服是我们主动做的，但仍要进告警中心并推送 ——
		// 节点一停，这台机器上的所有用户都连不上。见 nodetraffic.go。
		if row.trafficSuspended {
			conditions = append(conditions, alertCondition{alertKey{"node_traffic_exceeded", row.serverID, row.id}, "critical"})
		}
		switch row.status {
		case "offline":
			conditions = append(conditions, alertCondition{alertKey{"node_offline", row.serverID, row.id}, "critical"})
		case "warning":
			// 2026-10-02 起这个状态真的会产生（连续探测失败达阈值，见
			// collector.go 的 unstableAfterFailures）。此前它是一条**死规则** ——
			// 前端有「不稳定」文案与指标卡、这里也等着，但从来没有任何代码
			// 路径写 status='warning'，于是永远不会触发。
			conditions = append(conditions, alertCondition{alertKey{"node_unstable", row.serverID, row.id}, "warning"})
		case "unknown":
			// 节点 unknown 的成因与主机不同：**主机层的 server_unknown /
			// server_stale 覆盖不到它**（那两条依赖 server_metrics 样本，
			// 而密钥解不开时主机照样在采样本 —— 机器好得很，只是这台节点的
			// api_key 用不了）。缺了这条，节点侧那类故障会被完全静默：
			// 页面上显示"未知"，告警中心一片安静。
			conditions = append(conditions, alertCondition{alertKey{"node_unknown", row.serverID, row.id}, "warning"})
		}
	}

	// 商业层风控告警：检查已到期（超过 5 分钟缓冲）但在节点上仍滞留未注销成功的订阅。
	// 典型成因：节点离线、网络抖动、或注销接口重试未通。若不告警，买家到期后在节点侧可能仍可连入。
	lingeringCutoff := time.Now().Add(-5 * time.Minute)
	lingerRows, err := s.db.Query(ctx, `
SELECT DISTINCT n.server_id::text, n.id::text
FROM subscription_tokens st
JOIN LATERAL jsonb_object_keys(st.node_accounts) AS node_key(id) ON jsonb_typeof(st.node_accounts) = 'object'
JOIN hysteria_nodes n ON n.id::text = node_key.id
WHERE st.deleted_at IS NULL
  AND st.expires_at IS NOT NULL
  AND st.expires_at <= $1
  AND n.deleted_at IS NULL`, lingeringCutoff)
	if err != nil {
		return err
	}
	for lingerRows.Next() {
		var sID, nID string
		if err := lingerRows.Scan(&sID, &nID); err != nil {
			lingerRows.Close()
			return err
		}
		if sID != "" && nID != "" {
			conditions = append(conditions, alertCondition{alertKey{"node_subscription_lingering", sID, nID}, "warning"})
		}
	}
	if err := lingerRows.Err(); err != nil {
		lingerRows.Close()
		return err
	}
	lingerRows.Close()

	// 滞留「收不回来」的升级判据（2026-10-24 审计 P1-1）。
	//
	// 上面那条 lingering 只说明「这个账号还挂在节点上」，它在退避重试期间
	// 会一直亮着、级别是 warning，很容易被运维当成「系统自己在重试，等它」。
	// 但退避是指数级的（2→4→8…封顶 1 小时），一台长期离线的机器上，
	// attempts 会涨到几十次 —— 实测韩国-首尔 attempts=26、已挂 11 天。
	// 此时「系统会自己重试好」是**错的预期**：节点永远回不来，
	// 这个账号永远不会被回收，除非有人去做决定（退租机器 / 删节点 / 手工认账）。
	//
	// 所以按 attempts 分档再补一条 critical：达到 lingRetryExhausted 次
	// 说明自动路径已经放弃性重试了足够久，必须由人介入。
	// 复用 node_subscription_lingering 这个类型会覆盖上面的 warning ——
	// 那样运维就分不清「等等就好」和「必须动手」，宁可多一条独立类型。
	const lingRetryExhausted = 12 // 约半天（2+4+8+…累计，含封顶段）
	exhaustedRows, xerr := s.db.Query(ctx, `
SELECT DISTINCT n.server_id::text, n.id::text, max(b.attempts)
FROM subscription_revoke_backoff b
JOIN hysteria_nodes n ON n.id = b.node_id
JOIN subscription_tokens st ON st.id = b.subscription_id
WHERE n.deleted_at IS NULL
  AND st.deleted_at IS NULL
  AND b.attempts >= $1
GROUP BY 1, 2`, lingRetryExhausted)
	if xerr != nil {
		return xerr
	}
	for exhaustedRows.Next() {
		var sID, nID string
		var attempts int
		if err := exhaustedRows.Scan(&sID, &nID, &attempts); err != nil {
			exhaustedRows.Close()
			return err
		}
		if sID != "" && nID != "" {
			conditions = append(conditions, alertCondition{alertKey{"node_revoke_exhausted", sID, nID}, "critical"})
		}
	}
	if err := exhaustedRows.Err(); err != nil {
		exhaustedRows.Close()
		return err
	}
	exhaustedRows.Close()

	expiring, err := s.expiringServerConditions(ctx)
	if err != nil {
		return err
	}
	conditions = append(conditions, expiring...)
	blocked, err := s.blockedNodeConditions(ctx)
	if err != nil {
		return err // 探测状态读失败不能当作「被墙已恢复」
	}
	conditions = append(conditions, blocked...)
	chinaLoss, err := s.chinaLossConditions(ctx)
	if err != nil {
		return err // 读不到测速数据不能当作「丢包已恢复」
	}
	conditions = append(conditions, chinaLoss...)

	active := make(map[alertKey]bool, len(conditions))
	for _, condition := range conditions {
		active[condition.key] = true
		var existingID string
		err := s.db.QueryRow(ctx, `
SELECT id FROM alerts
WHERE type=$1 AND status <> 'resolved' AND server_id=$2::uuid AND COALESCE(node_id::text, '') = $3`,
			condition.key.kind, condition.key.serverID, condition.key.nodeID).Scan(&existingID)
		if errors.Is(err, pgx.ErrNoRows) {
			// 复用 10 分钟内刚 resolve 的同一行，而不是新开一条
			// （2026-10-24 审计 P0-2，见 alertMergeWindow）。
			//
			// 为什么必须复用：一次 90 秒以上的抖动会越过 alertResolveGrace
			// 被 resolve，几十秒后条件回来又是一条全新的 alert 行。
			// 库里于是堆着一串「同一台机器、同一类型、间隔 1~2 分钟」的告警，
			// 运维在列表里看到的是 20 条故障，实际只有 1 台机器在抖。
			//
			// 复用后：行数 = 真实故障数，事件流仍是 reopened（时间线不断），
			// 而 first_seen_at 保留最早那次 —— 于是「这次故障从什么时候开始」
			// 这个最该看的信息不会因为抖动而被重置。
			var priorID string
			reviveErr := s.db.QueryRow(ctx, `
UPDATE alerts
   -- 2026-11-04：复活时清掉 notified_at。原来保留上一段的值：这次 reopened 推送若被
   -- 1 小时冷却挡下，notified_at 仍非空 ⇒ 升级重推永远不触发，运维最后看到的是「已恢复」，节点却在宕。
   SET status='open', resolved_at=NULL, last_seen_at=now(), severity=$4, notified_at=NULL
 WHERE id = (
     SELECT id FROM alerts
      WHERE type=$1 AND status='resolved' AND server_id=$2::uuid
        AND COALESCE(node_id::text, '') = $3
        AND resolved_at IS NOT NULL AND resolved_at > now() - make_interval(secs => $5)
      ORDER BY resolved_at DESC
      LIMIT 1)
RETURNING id`,
				condition.key.kind, condition.key.serverID, condition.key.nodeID,
				condition.severity, int(alertMergeWindow.Seconds())).Scan(&priorID)
			if reviveErr == nil {
				s.recordAlertEvent(ctx, priorID, "reopened", map[string]any{"severity": condition.severity, "merged": true})
				continue
			}
			if !errors.Is(reviveErr, pgx.ErrNoRows) {
				// 复用失败只记日志，继续走新建 —— 少一次归并，不能少了告警本身。
				s.logger.Warn("alerts: 归并近期已恢复告警失败，改为新建", "type", condition.key.kind, "error", reviveErr)
			}

			// Was this condition previously open and then resolved? If so this is
			// a recurrence, and the timeline should say so rather than pretending
			// the problem appeared for the first time.
			var priorResolved bool
			if err := s.db.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM alerts
    WHERE type=$1 AND status='resolved' AND server_id=$2::uuid AND COALESCE(node_id::text, '') = $3)`,
				condition.key.kind, condition.key.serverID, condition.key.nodeID).Scan(&priorResolved); err != nil {
				return err
			}
			var newID string
			if err := s.db.QueryRow(ctx, `
INSERT INTO alerts (server_id, node_id, type, severity, status, first_seen_at, last_seen_at)
VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, $4, 'open', now(), now())
RETURNING id`,
				condition.key.serverID, condition.key.nodeID, condition.key.kind, condition.severity).Scan(&newID); err != nil {
				s.logger.Warn("alerts: insert failed", "type", condition.key.kind, "error", err)
				continue
			}
			eventType := "triggered"
			if priorResolved {
				eventType = "reopened"
			}
			s.recordAlertEvent(ctx, newID, eventType, map[string]any{"severity": condition.severity})
			continue
		}
		if err != nil {
			return err
		}
		// 已有 open 告警：刷新活跃时间。
		//
		// ⚠️ 这里还必须处理「告警黑洞」（2026-10-24 加）：
		// 一条 open 告警在**状态首次出现**的那一刻只推送一次。若那一刻推送失败、
		// 被 1 小时冷却吞掉、或落在静默期里被放弃 —— 此后这条告警一直 open，
		// 而本分支原来只 UPDATE last_seen_at，**不再产生任何新事件、也不再重试**。
		// 于是面板知道故障、数据库有记录，运维却永远收不到 ——
		// 一条持续 7 小时的宕机全程静默（2026-10-23 美国-阿什本实测）。
		//
		// 修法：持续超过 alertEscalateAfter 且**从未推送成功**（notified_at IS NULL）
		// 的告警，重新走一次推送。已推送过的（notified_at 非空）不重复推 ——
		// 否则每次 reconcile 都会重复告警，刷屏比不告警更糟。
		escalated, err := s.alertShouldEscalate(ctx, existingID)
		if err != nil {
			s.logger.Warn("alerts: 查推送状态失败", "error", err)
		}
		if _, err := s.db.Exec(ctx, `UPDATE alerts SET last_seen_at = now(), severity = $1 WHERE id = $2::uuid`, condition.severity, existingID); err != nil {
			s.logger.Warn("alerts: refresh failed", "error", err)
		}
		// 2026-11-04：已确认（acknowledged）的告警不再升级重推 —— 运维已经接手了；
		// 同一条告警升级每小时最多记一条事件：原来渠道不可用时每 30 秒一轮都插一行 repeated
		// （单条告警一天约 2880 行），而冷却只限制了推送、没限制记事件。
		if escalated && s.alertNotifier != nil && !s.alertTypeMuted(ctx, condition.key.kind) && s.alertNotifier.allow("escalate|"+existingID, time.Now()) {
			s.logger.Warn("alerts: 持续未送达，升级重推", "type", condition.key.kind,
				"server_id", condition.key.serverID, "node_id", condition.key.nodeID)
			// 用 "repeated" 而不是新造事件类型：alert_events 的 CHECK 约束
			// 只允许 triggered/repeated/reopened/acknowledged/resolved，
			// 而 "repeated" 在此之前从未被任何代码写过 ——
			// 它的语义恰好就是「同一故障重复出现」，与升级重推完全吻合。
			// 复用它比改生产库的 CHECK 约束安全得多。
			s.recordAlertEvent(ctx, existingID, "repeated", map[string]any{"severity": condition.severity})
		}
	}

	// subject_gone：告警的主机或节点已被删除。2026-11-06：这类告警原来走下面的正常结案并记 resolved 事件，
	// 于是删掉一台离线主机 90 秒后，Telegram 收到一条「✅ [恢复] SSH 已恢复连接」—— 机器其实已经退租。
	// 主体没了不是「恢复」：直接归档，不记事件、不推送。
	rows, err = s.db.Query(ctx, `SELECT a.id, a.type, COALESCE(a.server_id::text, ''), COALESCE(a.node_id::text, ''), a.last_seen_at,
  (EXISTS (SELECT 1 FROM servers sv WHERE sv.id = a.server_id AND sv.deleted_at IS NOT NULL)
   OR EXISTS (SELECT 1 FROM hysteria_nodes hn WHERE hn.id = a.node_id AND hn.deleted_at IS NOT NULL)) AS subject_gone
FROM alerts a WHERE a.status <> 'resolved'`)
	if err != nil {
		return err
	}
	var stale, gone []string
	for rows.Next() {
		var id, kind, serverID, nodeID string
		var lastSeen time.Time
		var subjectGone bool
		if err := rows.Scan(&id, &kind, &serverID, &nodeID, &lastSeen, &subjectGone); err != nil {
			rows.Close()
			return err
		}
		if active[alertKey{kind, serverID, nodeID}] {
			continue
		}
		if subjectGone {
			gone = append(gone, id)
			continue
		}
		// 条件消失了，但先不急着 resolve：给它一个 alertResolveGrace 的观察期
		// （2026-10-24 审计 P0-2）。若抖动在下一次 reconcile 就回来，
		// 这条 alert 保持不变、只是 last_seen_at 被刷新 —— 时间线连续，
		// 不会多出一条 reopened 事件。
		//
		// 判据用 last_seen_at（最后一次确认条件存在的时刻）而不是 first_seen_at：
		// 一个挂了 3 小时的告警，刚恢复 30 秒又挂，它显然不该被 resolve。
		if time.Since(lastSeen) < alertResolveGrace {
			continue
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range stale {
		if _, err := s.db.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE id=$1::uuid`, id); err != nil {
			s.logger.Warn("alerts: resolve failed", "error", err)
			continue
		}
		s.recordAlertEvent(ctx, id, "resolved", map[string]any{"auto": true})
	}
	for _, id := range gone {
		if _, err := s.db.Exec(ctx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE id=$1::uuid AND status <> 'resolved'`, id); err != nil {
			s.logger.Warn("alerts: archive alert of deleted subject failed", "error", err)
		}
	}

	// 半下线巡检：找出「订阅映射指向已软删节点」的残留并清理（2026-10-24 加）。
	//
	// 放在告警 reconcile 的末尾：它的优先级低于「有故障要报」，
	// 清理失败也不该影响本轮告警的判定与推送。
	//
	// 为什么必须主动巡检而不是等兜底：兜底 handOffDeletedNodeAccount
	// 只在**订阅到期 sweeper** 里被调用 —— 也就是说，未到期的活跃订阅
	// 会一直挂着这种映射，直到自然到期（实测最长可挂数周）。
	// 而 manage.go 里 node_subscription_lingering 的检测 SQL 带
	// `AND n.deleted_at IS NULL`，恰好把这类情况排除 ——
	// 于是「映射指向已删节点」在整个系统里没有任何一处能发现（2026-10-23 实测）。
	if _, err := s.sweepHalfDecommissioned(ctx); err != nil {
		s.logger.Warn("半下线巡检失败", "error", err)
	}
	return nil
}

// recordAlertEvent appends one row to the alert timeline. It is intentionally
// best-effort: a failure here must never abort a reconcile pass, because the
// alerts table itself is already consistent at that point and losing an audit
// line is far less bad than leaving every alert stale.
func (s *Server) recordAlertEvent(ctx context.Context, alertID, eventType string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		payload = []byte("{}")
	}
	if _, err := s.db.Exec(ctx,
		`INSERT INTO alert_events (alert_id, event_type, details) VALUES ($1::uuid, $2, $3::jsonb)`,
		alertID, eventType, string(payload)); err != nil {
		s.logger.Warn("alerts: record event failed", "alert_id", alertID, "event", eventType, "error", err)
	}

	// 告警外发（2026-10-04 加）。
	//
	// 放在这里而不是两个调用点各自触发：recordAlertEvent 是**所有**告警事件的
	// 唯一汇聚处（触发 / 重现 / 恢复都走它），在这里挂一次就全覆盖，
	// 将来新增事件类型也不会漏掉通知。
	//
	// ⚠️ 必须在独立 goroutine 里做，且**用 Background ctx**：
	// 传入的 ctx 属于采集循环，循环一结束就被 cancel，
	// 会让"告警刚发出就被掐断"——这类 bug 只在采集器偶发慢时暴露，极难排查。
	go s.notifyAlertFor(ctx, alertID, eventType, s.shutdownDone)
}

// notifyAlertFor 按 alertID 查回上下文再外发。
//
// 为什么要查库而不是让调用方把上下文传进来：调用方（采集循环）手里
// 只有 alertID 与 condition，而「人可读的描述」需要 server/node 的名字 ——
// 那些要从库里 join 出来。在通知这一侧查一次，比让每个调用点
// 都先把名字查好再传进来更省事，也不会漏。
func (s *Server) notifyAlertFor(ctx context.Context, alertID, eventType string, shutdown shutdownChan) {
	defer func() {
		// 通知链路的任何 panic 都不该影响采集循环 —— 它是独立的 goroutine，
		// 但 panic 仍会打印刺眼的堆栈、让人以为面板坏了。
		if r := recover(); r != nil {
			s.logger.Warn("alert notify: panic", "alert_id", alertID, "recover", r)
		}
	}()

	// 降噪（2026-10-16）：原来每次触发 / 恢复都立刻推送，指标在阈值附近抖动、
	// 刚装好的主机采集还没稳定时，一小时能刷十几条「告警 → 恢复 → 再次出现」。
	//   1. 触发后先等 alertNotifyGrace，期间已自动恢复的整件事都不推送；
	//   2. 新登记的主机 / 新接入的节点有 alertNewTargetQuiet 静默期；
	//   3. 只有推送过「告警」的，才推送对应的「恢复」。
	if s.alertNotifier == nil {
		return
	}

	var (
		kind, severity, serverName, host, nodeName, status string
		port                                               int
		young                                              bool
		nodeID, serverID                                   string
	)
	// 先查上下文再睡：静默期按告警类型分级（alertGraceFor），
	// 而类型必须先查出来才知道。若先睡后查，所有类型都得用同一个门槛 ——
	// 而 10 分钟对「整机失联」太长（2026-10-24 加）。
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.db.QueryRow(queryCtx, `
SELECT a.type, a.severity, a.status,
       COALESCE(sv.name, ''), COALESCE(sv.host, ''),
       COALESCE(nd.name, ''), COALESCE(nd.listen_port, 0),
       COALESCE(a.node_id::text, ''), COALESCE(a.server_id::text, ''),
       GREATEST(COALESCE(sv.created_at, 'epoch'), COALESCE(nd.created_at, 'epoch')) > now() - $2::interval
FROM alerts a
LEFT JOIN servers sv ON sv.id = a.server_id
LEFT JOIN hysteria_nodes nd ON nd.id = a.node_id
WHERE a.id = $1::uuid`, alertID, fmt.Sprintf("%d seconds", int(alertNewTargetQuiet.Seconds()))).Scan(
		&kind, &severity, &status, &serverName, &host, &nodeName, &port, &nodeID, &serverID, &young)
	queryCancel()
	if err != nil {
		s.logger.Warn("alert notify: 查上下文失败", "alert_id", alertID, "error", err)
		return
	}
	if s.alertTypeMuted(context.Background(), kind) {
		return
	}

	switch eventType {
	case "triggered", "reopened":
		// ⚠️ 2026-10-30：这里原来是 time.Sleep(alertGraceFor(kind))。
		// 进程退出时正在睡眠的 goroutine 会被直接掐断 —— 告警事件已经写进
		// alert_events，但推送永远没发出去，面板里只剩一条"未送达"。
		// 改为可中断的等待：服务仍在跑时按静默期等，正在关闭时立刻返回。
		// shutdown 为 nil 时 select 永不触发，行为与 time.Sleep 一致。
		if shutdown != nil {
			select {
			case <-time.After(alertGraceFor(kind)):
			case <-shutdown:

				s.logger.Warn("alert notify: 静默期被服务关闭打断", "alert_id", alertID, "event", eventType)
				return
			}
		} else {
			time.Sleep(alertGraceFor(kind))
		}
	}
	// "repeated" 刻意**不睡**：它本身就是「已经等了很久还没送达」的补偿，
	// 再睡一轮等于把升级推迟到没人等得起的时候。而且它只在
	// notified_at IS NULL 时触发，不会与首次推送重复。

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if eventType == "resolved" || eventType == "acknowledged" {
		// 内存查不到时回落查 alerts.notified_at —— 重启前推送过的告警也能推「恢复」。
		if !s.wasAnnounced(ctx, alertID, eventType == "resolved") {
			return
		}
	}

	if eventType == "triggered" || eventType == "reopened" || eventType == "repeated" {
		// 2026-11-04：repeated（升级重推）同样要重查 —— 原来不查，已确认的告警、
		// 新加目标的静默期内都会照推。
		// 静默期内告警可能已自动恢复：上面查出的 status 是等待前的旧值，
		// 必须重新查一次，否则降噪不起作用（2026-11-02 审计 M1）。
		if err := s.db.QueryRow(ctx, `SELECT status FROM alerts WHERE id = $1::uuid`, alertID).Scan(&status); err != nil {
			s.logger.Warn("alert notify: 重查告警状态失败", "alert_id", alertID, "error", err)
			return
		}
		if status == "resolved" || young || (eventType == "repeated" && status != "open") || status != "open" || s.alertTypeMuted(ctx, kind) {
			return // 等待期内已恢复、目标刚加进来、已被确认，或该类型已被静音
		}
	}

	ev := buildAlertNotification(eventType, kind, severity, serverName, host,
		nodeName, port, "", time.Now())
	// 把三个 ID 补进载荷：Telegram 的处置按钮要靠它们定位对象
	//（2026-10-24 加）。放在这里而不是 buildAlertNotification 里，
	// 是因为纯函数不该被塞进三个与文案无关的 ID 参数。
	ev.AlertID = alertID
	ev.NodeID = nodeID
	ev.ServerID = serverID
	if eventType == "triggered" || eventType == "reopened" || eventType == "repeated" {
		// 先过节流、真正放行后才记「已推送」（admitTrigger），再投递。
		//
		// repeated 与前两者共用 admitTrigger 是有意的：
		// 1 小时冷却按 类型|目标|事件 走，repeated 是独立事件类型，
		// 首次升级推送不会被「刚推过 triggered」挡掉；
		// 而重复的 repeated（每 30 秒一轮 reconcile）仍受冷却保护，不会刷屏。
		if s.admitTrigger(ctx, alertID, ev) && s.deliverAlert(ev) {
			s.markDelivered(ctx, alertID)
		}
		return
	}
	s.notifyAlert(ev)
}

func describeAlert(kind, serverName, host, nodeName string, port int) string {
	switch kind {
	case "server_offline":
		return fmt.Sprintf("%s（%s）主机不可达，SSH 连接失败", serverName, host)
	case "server_warning":
		return fmt.Sprintf("%s 资源使用超过阈值（CPU 持续 ≥85%% 或磁盘 ≥90%%）", serverName)
	case "server_unknown":
		return fmt.Sprintf("%s 尚未成功采集，检查是否已配置 SSH 凭据", serverName)
	case "server_stale":
		return fmt.Sprintf("%s 主机可达但采集停滞（超过数个周期没有新指标），采集器可能异常", serverName)
	case "server_ssh_broken":
		return fmt.Sprintf("%s 的 SSH 不可用（凭据失效、连接失败或主机指纹不一致，详见主机页的状态原因），资源指标已停止采集；"+
			"节点经 API 探测仍在线、未从订阅下线。请在「VPS 主机」页按原因处理", serverName)
	case "server_blind":
		// 措辞必须说清「不是机器坏了」—— 否则运维第一反应是去查机器，
		// 而实际问题是没有凭据、面板看不到它。
		return fmt.Sprintf("%s 未配置 SSH 凭据，面板从未采集到它的任何资源指标"+
			"（CPU/内存/磁盘一律不可见）；机器本身可能正常，请补录凭据或在主机页确认「仅监控节点」", serverName)
	case "server_expiring":
		return fmt.Sprintf("%s（%s）7 天内到期，记得续费，否则其上的节点会随主机一起消失", serverName, host)
	case "server_expired":
		return fmt.Sprintf("%s（%s）已过到期日；若已续费，请在主机页更新到期日", serverName, host)
	case "node_blocked":
		return fmt.Sprintf("节点 %s 疑似被墙：国内探测点 TCP 与 ping 都不通，而海外仍可连接；可用「迁移节点」换一台 VPS", nodeName)
	case "node_china_loss":
		return fmt.Sprintf("节点 %s 回国线路持续丢包：近 1 小时内至少一家运营商平均丢包 ≥30%%，"+
			"客户可能卡顿或断流；可在「节点监控 → 详情 → 三网质量趋势」看是哪家、从何时开始", nodeName)
	case "node_offline":
		return fmt.Sprintf("节点 %s 的端口 %d 未监听", nodeName, port)
	case "node_unstable":
		return fmt.Sprintf("节点 %s 探测不稳定（连续失败达阈值）", nodeName)
	case "node_unknown":
		// 2026-10-04 加。
		//
		// 此前这个类型没有分支，落进 default 拼出 `node_unknown · 东京-AWS`
		// 这种机器人话 —— 而它恰恰是**最常见**的节点故障形态
		// （节点 API 密钥失效，主机好着、端口也在监听，但面板读不到它的状态）。
		// 实测：造一个节点后第一次巡检就会命中它，告警文案却完全无法指导行动。
		return fmt.Sprintf("节点 %s 状态未知：节点门户 API 读不到（多半是 api_key 失效或门户未安装），"+
			"主机与端口可能是正常的", nodeName)
	case "node_traffic_exceeded":
		return fmt.Sprintf("节点 %s 本月流量已超过限额，节点服务已自动停止（下个重置日或调高限额后自动恢复）", nodeName)
	case "node_subscription_lingering":
		return fmt.Sprintf("节点 %s 存在已到期订阅用户滞留未注销（注销重试未成功），客户可能仍可连入", nodeName)
	case "node_revoke_exhausted":
		// 与上面那条的区别必须写清楚，否则运维会把两条当成同一条处理：
		// lingering 是「还没收回来，系统在重试」；这一条是「重试已经放弃了，
		// 不来人处理它就永远收不回来」。
		return fmt.Sprintf("节点 %s 上有已到期订阅账号自动回收连续失败十余次，重试已退避到上限，"+
			"若不人工处理（退租机器后删节点，或恢复机器）该账号将长期存活，客户可能仍可连入", nodeName)
	default:
		return fmt.Sprintf("%s · %s", kind, serverName)
	}
}

// describeAlertRecovery 说明恢复后的状态。复用故障描述会让恢复通知
// 仍然写着「失败」「不可达」，与 [恢复] 标签自相矛盾。
func describeAlertRecovery(kind, serverName, host, nodeName string, port int) string {
	switch kind {
	case "server_offline":
		return fmt.Sprintf("%s（%s）SSH 已恢复连接", serverName, host)
	case "server_warning":
		return fmt.Sprintf("%s 资源使用已回到阈值以下", serverName)
	case "server_unknown":
		return fmt.Sprintf("%s 已成功采集到指标", serverName)
	case "server_stale":
		return fmt.Sprintf("%s 已恢复采集新指标", serverName)
	case "server_ssh_broken":
		return fmt.Sprintf("%s 的 SSH 已恢复可用", serverName)
	case "server_blind":
		return fmt.Sprintf("%s 已能采集到资源指标（SSH 凭据已配置）", serverName)
	case "server_expiring", "server_expired":
		return fmt.Sprintf("%s（%s）不再处于临期或过期状态", serverName, host)
	case "node_blocked":
		return fmt.Sprintf("节点 %s 国内探测已恢复连通", nodeName)
	case "node_china_loss":
		return fmt.Sprintf("节点 %s 回国线路丢包已回落到 15%% 以下", nodeName)
	case "node_offline":
		return fmt.Sprintf("节点 %s 的端口 %d 已恢复监听", nodeName, port)
	case "node_unstable":
		return fmt.Sprintf("节点 %s 探测已恢复稳定", nodeName)
	case "node_unknown":
		return fmt.Sprintf("节点 %s 门户 API 已恢复读取", nodeName)
	case "node_traffic_exceeded":
		return fmt.Sprintf("节点 %s 已不再处于超流量停机状态", nodeName)
	case "node_subscription_lingering":
		return fmt.Sprintf("节点 %s 上已到期订阅账号已注销，不再有滞留账号", nodeName)
	case "node_revoke_exhausted":
		return fmt.Sprintf("节点 %s 上的滞留账号已完成回收，自动重试也恢复正常", nodeName)
	default:
		return fmt.Sprintf("%s 状态已恢复正常", describeAlert(kind, serverName, host, nodeName, port))
	}
}

func (s *Server) acknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	alertID := r.PathValue("id")
	tag, err := s.db.Exec(r.Context(), `UPDATE alerts SET status='acknowledged' WHERE id=$1::uuid AND status='open'`, alertID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "告警不存在或已处理"})
		return
	}
	actor := ""
	if claims, ok := claimsFrom(r.Context()); ok {
		actor = claims.Email
		if actor == "" {
			actor = claims.Subject
		}
	}
	s.recordAlertEvent(r.Context(), alertID, "acknowledged", map[string]any{"by": actor})
	s.audit(r.Context(), r, "acknowledge_alert", "alert", alertID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

func (s *Server) updateServer(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Name        *string   `json:"name"`
		Region      *string   `json:"region"`
		Provider    *string   `json:"provider"`
		Tags        *[]string `json:"tags"`
		Host        *string   `json:"host"`
		SSHPort     *int      `json:"ssh_port"`
		SSHUser     *string   `json:"ssh_user"`
		ResetHost   bool      `json:"reset_host_key"`
		TrafficURL  *string   `json:"traffic_api_url"`
		TrafficFile *string   `json:"traffic_secret_file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 2026-11-03：改 SSH 地址/端口/用户或重置主机指纹会清空已登记的指纹，
	// 下一次连接按「首次信任」接受新主机的密钥，并把已存的 SSH 凭据（含明文口令）发过去。
	// 新建主机、上传凭据、接受指纹、换 IP 都只给管理员；这里原来任何登录用户都能做，
	// 等于 operator 能把凭据引到自己的机器上。日常字段（名称/地区/标签…）仍对 operator 开放。
	touchesEndpoint := input.Host != nil || input.SSHPort != nil || input.SSHUser != nil || input.ResetHost || input.TrafficURL != nil || input.TrafficFile != nil
	if touchesEndpoint && claims.Role != "admin" {
		s.auditDenied(r, "forbidden_admin_only")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "修改 SSH 连接信息、流量采集地址/密钥路径或重置主机指纹需要管理员角色"})
		return
	}

	fields := make([]string, 0, 7)
	args := make([]any, 0, 8)
	add := func(column string, value any) {
		args = append(args, value)
		fields = append(fields, column+"=$"+strconv.Itoa(len(args)))
	}
	if input.Name != nil && strings.TrimSpace(*input.Name) != "" {
		add("name", strings.TrimSpace(*input.Name))
		// 人工改了名字 → 标记来源为 manual，此后自动识别不再覆盖它。
		//
		// 这一步是「已手工填过的不覆盖」那条约定能成立的**关键**：
		// 若只写值不改来源，字段还挂着 auto，下一次自动识别就会把这个
		// 手工名字冲掉 —— 正好是用户明确不要的行为，而且是静默发生的
		// （用户改完当时看着没问题，某次加节点后才莫名变回去）。
		add("name_source", sourceManual)
	}
	if input.Region != nil {
		add("region", strings.TrimSpace(*input.Region))
		// 同上。注意这里**不能**用"值非空才标 manual"来判断：
		// 用户把 region 清空也是一个人工意图（"我不想标地区"），
		// 清空后来源标成 manual 会让它保持为空而不再被自动填 ——
		// 这正是用户想要的。所以只要字段**被提交**就标 manual。
		add("region_source", sourceManual)
	}
	if input.Provider != nil {
		add("provider", strings.TrimSpace(*input.Provider))
		add("provider_source", sourceManual)
	}
	if input.Tags != nil {
		tags := *input.Tags
		if tags == nil {
			tags = []string{}
		}
		add("tags", tags)
	}
	endpointChanged := false
	if input.Host != nil && strings.TrimSpace(*input.Host) != "" {
		host := strings.TrimSpace(*input.Host)
		if strings.ContainsAny(host, " \t\r\n/") || controlChars.MatchString(host) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机地址包含非法字符"})
			return
		}
		add("host", host)
		endpointChanged = true
	}
	if input.SSHPort != nil {
		if *input.SSHPort < 1 || *input.SSHPort > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需为 1-65535"})
			return
		}
		add("ssh_port", *input.SSHPort)
		endpointChanged = true
	}
	if input.SSHUser != nil && strings.TrimSpace(*input.SSHUser) != "" {
		sshUser := strings.TrimSpace(*input.SSHUser)
		if strings.ContainsAny(sshUser, " \t\r\n") || controlChars.MatchString(sshUser) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "SSH 用户名包含非法字符"})
			return
		}
		add("ssh_user", sshUser)
		endpointChanged = true
	}
	if input.TrafficURL != nil || input.TrafficFile != nil {
		url, file := "", ""
		if input.TrafficURL != nil {
			url = strings.TrimSpace(*input.TrafficURL)
		}
		if input.TrafficFile != nil {
			file = strings.TrimSpace(*input.TrafficFile)
		}
		if !validTrafficEndpoint(url, file) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "流量接口地址只能是回环 http（如 http://127.0.0.1:29862），密钥路径需为绝对路径；两者需同时提供或同时留空"})
			return
		}
		args = append(args, nilIfEmpty(url), nilIfEmpty(file))
		fields = append(fields, "traffic_api_url=$"+strconv.Itoa(len(args)-1), "traffic_secret_file=$"+strconv.Itoa(len(args)))
	}
	if endpointChanged || input.ResetHost {
		args = append(args, nil)
		fields = append(fields, "host_fingerprint=$"+strconv.Itoa(len(args)))
	}
	if len(fields) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有可更新字段"})
		return
	}
	serverID := r.PathValue("id")
	args = append(args, serverID)
	query := "UPDATE servers SET " + strings.Join(fields, ", ") + ", updated_at=now() WHERE id=$" + strconv.Itoa(len(args)) + "::uuid AND deleted_at IS NULL"
	tag, err := s.db.Exec(r.Context(), query, args...)
	if err != nil {
		// 2026-11-06：改成一台已登记的地址 / 端口会撞 UNIQUE(host, ssh_port)。createServer 早就映射成 409，
		// 这里却走 databaseError 回 500「database request failed」+ ERROR 日志 —— 运维只会以为面板坏了。
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "已登记过同地址同 SSH 端口的主机"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	s.audit(r.Context(), r, "update_server", "server", serverID, map[string]any{"fields": fields})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除主机需要 X-Confirm-Dangerous: yes"})
		return
	}
	serverID := r.PathValue("id")
	// 2026-11-04：安装 / 迁移进行中（或中断待核验）不能删主机。原来只看在役节点数：安装进行到一半时
	// 主机上还没有节点记录，删除照样成功 —— 远端装好后「登记节点」因「所属主机不存在或已删除」失败，
	// 机器上跑着一台面板不知道的节点；迁移则把节点归属写到一台已删除的主机上。与换 IP 同一判据。
	if busy, err := s.serverHasActiveHostJob(r.Context(), serverID); err != nil {
		s.databaseError(w, r, err)
		return
	} else if busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这台主机有安装/迁移任务在进行（或待核验结案），完成后再删除"})
		return
	}
	var nodeCount int
	if err := s.db.QueryRow(r.Context(), `SELECT COUNT(*) FROM hysteria_nodes WHERE server_id=$1::uuid AND deleted_at IS NULL`, serverID).Scan(&nodeCount); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if nodeCount > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("该主机下仍有 %d 个节点，请先删除节点", nodeCount)})
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE servers SET deleted_at=now() WHERE id=$1::uuid AND deleted_at IS NULL`, serverID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	// 2026-11-06：主机级告警就地归档（与 deleteNode 一致），不记 resolved 事件 —— 主机删了不是「恢复」。
	if _, err := s.db.Exec(r.Context(), `UPDATE alerts SET status='resolved', resolved_at=now() WHERE server_id=$1::uuid AND status <> 'resolved'`, serverID); err != nil {
		s.logger.Warn("delete server: 归档主机告警失败", "server_id", serverID, "error", err)
	}
	// 2026-11-03：主机删了，它的排队任务也就没有执行对象了；置为取消，任务页不再显示「排队中」。
	if _, err := s.db.Exec(r.Context(), `UPDATE operation_tasks SET status='cancelled', finished_at=now(),
		result_summary='主机已删除，任务取消' WHERE server_id=$1::uuid AND status='queued'`, serverID); err != nil {
		s.logger.Warn("delete server: 取消排队任务失败", "server_id", serverID, "error", err)
	}
	s.audit(r.Context(), r, "delete_server", "server", serverID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除节点需要 X-Confirm-Dangerous: yes"})
		return
	}
	nodeID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	// 2026-11-04：主机正在安装 / 迁移时拒绝删除（与绑定 / 解绑同一判据）。此时 loadPortalTarget
	// 会拒绝 ⇒ 取不到凭据快照 ⇒ 每个买家账号都回收失败；随后软删除抹掉 api_url/api_key，
	// 登记的待补删记录也没有密钥 ——「补删」永远无法执行。迁移任务还会继续改写这台已删除的节点。
	if s.nodeMaintenance(r.Context(), nodeID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点所在主机正在安装或迁移，完成后再删除节点"})
		return
	}
	// 凭据快照必须在删除**之前**取：loadPortalTarget 的 SQL 带 AND deleted_at IS NULL，
	// 而下面这条 UPDATE 会把 deleted_at 置上、并按安全设计抹掉 api_url/api_key。
	// 先删后回收的话，回收必然拿不到凭据，买家账号会原封不动留在节点上继续可用，
	// 而控制台这边却记成「已成功注销」。
	var snapshot *portalTarget
	if t, err := s.loadPortalTarget(r.Context(), nodeID); err == nil {
		snapshot = t
	} else {
		s.logger.Warn("deleteNode: 取节点 API 凭据快照失败，删除后无法物理注销买家账号",
			"node_id", nodeID, "error", err)
	}
	// 注销登记的同时忘掉节点侧 API 凭据：那把 key 能读出 obfs/auth 口令，
	// 不该在节点已删除后还以明文密文形式留在库里。
	//
	// 但要拿一份节点名与地址用于**孤儿账号登记**（下面 recordOrphanAccounts 用）——
	// 否则补删时运维只看得到一串 uuid，不知道是哪台机器。
	var nodeName, nodeAPIURL string
	if err := s.db.QueryRow(r.Context(),
		`SELECT name, COALESCE(api_url,'') FROM hysteria_nodes WHERE id=$1::uuid`,
		nodeID).Scan(&nodeName, &nodeAPIURL); err != nil {
		s.logger.Warn("deleteNode: 读取节点名与地址失败（仅影响孤儿登记的可读性）",
			"node_id", nodeID, "error", err)
	}
	// ⚠️ 2026-10-24 审计 P1-4 附：软删除必须同时把 status 置成 offline。
	//
	// 原来只抹凭据、不碰 status，于是已删的节点保留着删除前那一刻的
	// online/warning。实测库里 7 台已软删节点仍是 status='online'
	// （阿什本、Karachi、马尼拉、node4.example.com …）。
	//
	// 后果不是显示问题，是**统计口径被污染**：
	//   - 看板「节点总数/在线数」把已删节点算进去，运维看到的在线数偏高；
	//   - 任何 `WHERE status='online'` 的查询（订阅构建、报表）都要额外
	//     记得加 deleted_at IS NULL，漏一处就出错；
	//   - 更隐蔽的是：这个残留状态会让人误判「这台机器还在跑」，
	//     而它恰恰是已经下架的那台。
	//
	// 采集器对已删节点不再写入（它按 deleted_at IS NULL 取目标），
	// 所以这个错误的 status 会**永久冻结**在那里，不会自愈。
	// Read and revoke subscriptions before soft deletion so a DB enumeration failure leaves credentials retryable.
	outcome := s.revokeNodeFromAllSubscriptions(r.Context(), nodeID, snapshot)
	if outcome.Err != nil {
		s.logger.Error("deleteNode: subscription enumeration failed; preserving node for retry", "node_id", nodeID, "error", outcome.Err)
		s.databaseError(w, r, outcome.Err)
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE hysteria_nodes SET deleted_at=now(), status='offline', status_error='', api_url=NULL, api_key=NULL, users_count=NULL, api_checked_at=NULL, updated_at=now() WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	// 节点已软删，立即失效故障闸门、订阅下发缓存与能力探测缓存，避免客户继续拿到已下架节点。
	invalidateHealthGateCache()
	InvalidateSubscriptionCache()
	invalidateNodeCapsCache(nodeID)

	// 节点行软删除已提交，后续凭据擦除、孤儿登记、结案、告警归档与审计均使用脱离请求取消的有界上下文，防客户端断开漏留痕。
	postCtx, stopPost := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer stopPost()

	// 同步抹除门户网页登录凭据与路径令牌；先检查三列是否存在以兼容最小测试表夹具，生产库始终清空。
	var portalCredColCount int
	if colErr := s.db.QueryRow(postCtx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'hysteria_nodes' AND column_name IN ('portal_basic_user','portal_basic_password','portal_path_token')`).Scan(&portalCredColCount); colErr == nil && portalCredColCount == 3 {
		if _, colErr = s.db.Exec(postCtx, `UPDATE hysteria_nodes SET portal_basic_user=NULL, portal_basic_password=NULL, portal_path_token=NULL WHERE id=$1::uuid`, nodeID); colErr != nil {
			s.logger.Warn("deleteNode: 抹除门户网页凭据失败", "node_id", nodeID, "error", colErr)
		}
	}

	// 把没删掉的账号**连凭据快照一起**落盘。
	//
	// 为什么必须落盘：下面紧接着就返回 207 说"需手动到节点清理"，
	// 但此刻 hysteria_nodes.api_url / api_key 已被抹成 NULL，
	// subscription_tokens.node_accounts 里虽然留着映射，
	// **却没有任何地方记着该连哪个地址、用哪把钥匙**。
	// 于是那条映射永远重试不了，买家账号永久留在节点上继续可用，
	// 而控制台这边连"还有这件事"都看不到了 —— 节点都删了。
	// 这是「钱退了，货还在」的同一类病，只是发生在"下架节点"上。
	// 实测证据：清理订阅残留时发现已软删的「悉尼-AWS」上还挂着 sub_00000001。
	// decommissioned=1：操作者确认这台机器已永久停用（关机/退租）。
	// 账号仍照常登记（留痕，可在「节点回收」里查到），但直接结案 ——
	// 机器不在了，"等恢复后补删"永远等不到，挂着只会变成永远消不掉的待办。
	decommissioned := r.URL.Query().Get("decommissioned") == "1"
	closeoutFailed := false
	orphanRecordIncomplete := false
	savedOrphans := 0
	if len(outcome.Failed) > 0 {
		s.recordOrphanAccounts(postCtx, nodeID, nodeName, nodeAPIURL, snapshot, outcome.Failed)
		// 实查落盘条数复核登记完整性，避免数据库写入部分失败却对运营谎报可一键补删。
		failedUserSet := make(map[string]struct{}, len(outcome.Failed))
		failedUsers := make([]string, 0, len(outcome.Failed))
		for _, f := range outcome.Failed {
			if f.UserID != "" {
				if _, dup := failedUserSet[f.UserID]; !dup {
					failedUserSet[f.UserID] = struct{}{}
					failedUsers = append(failedUsers, f.UserID)
				}
			}
		}
		if len(failedUsers) > 0 {
			if countErr := s.db.QueryRow(postCtx, `SELECT COUNT(DISTINCT user_id) FROM orphan_node_accounts WHERE node_id=$1::uuid AND resolved_at IS NULL AND user_id = ANY($2::text[])`, nodeID, failedUsers).Scan(&savedOrphans); countErr != nil {
				s.logger.Warn("deleteNode: 复核孤儿账号登记条数失败", "node_id", nodeID, "error", countErr)
			} else if savedOrphans < len(failedUsers) {
				orphanRecordIncomplete = true
				decommissioned = false
				s.logger.Error("deleteNode: 部分孤儿账号登记失败", "node_id", nodeID, "saved", savedOrphans, "total_failed", len(failedUsers))
			}
		}
		if !orphanRecordIncomplete && decommissioned {
			if _, err := s.db.Exec(postCtx, `UPDATE orphan_node_accounts
				SET resolved_at=now(), resolved_note='节点已永久停用，删除时直接结案'
				WHERE node_id=$1::uuid AND resolved_at IS NULL`, nodeID); err != nil {
				s.logger.Error("deleteNode: 停用节点的待补删账号结案失败", "node_id", nodeID, "error", err)
				decommissioned = false
				closeoutFailed = true
			}
		}
	}
	// 节点没了，它未了的告警也就没有主体了；就地归档，避免告警中心留着一条永远消不掉的记录。
	if _, err := s.db.Exec(postCtx, `UPDATE alerts SET status='resolved', resolved_at=now() WHERE node_id=$1::uuid AND status <> 'resolved'`, nodeID); err != nil {
		s.logger.Warn("resolve node alerts failed", "node_id", nodeID, "error", err)
	}
	s.audit(postCtx, r, "delete_node", "hysteria_node", r.PathValue("id"),
		map[string]any{"decommissioned": decommissioned, "unrevoked_accounts": len(outcome.Failed), "orphans_saved": savedOrphans})
	if len(outcome.Failed) > 0 && decommissioned {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "deleted_decommissioned",
			"revoked_count": outcome.Revoked,
			"failed_count":  len(outcome.Failed),
		})
		return
	}
	// 回收没有全部成功时必须让操作者看见：那意味着买家账号还在节点上能连，
	// 而节点凭据已随删除一并抹掉、重试不了。返回 207 让前端能分开处理「删掉了」
	// 与「账号没注销干净」。
	if len(outcome.Failed) > 0 {
		// 2026-11-04：能不能补删取决于删除前有没有取到凭据快照（recordOrphanAccounts 会把它加密存进待补删记录）。
		// 原来一律说「凭据已抹除，无法自动重试，需手动到节点清理」：正常情况下快照是有的，
		// 在「孤儿账号」里点一下就能补删，运维却被引去上机手工清理。
		retryable := snapshot != nil
		next := "已登记到「孤儿账号」，可在运维任务页一键补删"
		if !retryable {
			next = "删除前没能取到节点 API 凭据，无法自动补删，需到节点上手工删除（已登记在「孤儿账号」）"
		}
		if orphanRecordIncomplete {
			next = fmt.Sprintf("仅登记了 %d/%d 条孤儿记录（数据库写入部分失败），未登记的残留账号无法一键补删，请结合日志手工核查", savedOrphans, len(outcome.Failed))
		}
		// 2026-11-06：要求永久下线、但结案写库失败：如实说出来。原来悄悄退回普通删除的文案「可一键补删」——
		// 机器已经退租，补删永远连不上；运维该做的是到「孤儿账号」里手动结案。
		if closeoutFailed {
			next = "已按要求永久下线，但待补删记录结案失败（数据库错误），请到运维任务页的「孤儿账号」里手动结案"
		}
		writeJSON(w, http.StatusMultiStatus, map[string]any{
			"status":          "deleted_with_revoke_failures",
			"revoked_count":   outcome.Revoked,
			"failed_count":    len(outcome.Failed),
			"orphans_saved":   savedOrphans,
			"failed":          outcome.Failed,
			"retryable":       retryable && !closeoutFailed && !orphanRecordIncomplete,
			"closeout_failed": closeoutFailed,
			"error": fmt.Sprintf("节点已删除，但有 %d 个买家账号未能从节点侧注销，%s: %s",
				len(outcome.Failed), next, outcome.FailedSummary()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "revoked_count": outcome.Revoked})
}

// recordOrphanAccounts 把"删除节点时没能注销的买家账号"登记进 orphan_node_accounts，
// **连同当时的 portal 凭据快照**。
//
// 为什么需要它（2026-10-02 实测发现的死局）：
//
//	删除节点失败时，现有实现已经做对了两件事 —— 保留 node_accounts 映射、
//	返回 207 明确报错。但它随后把 hysteria_nodes.api_url/api_key 抹成 NULL
//	（安全上是对的：那把 key 能读出 obfs/auth 口令，不该留在库里）。
//	三者合起来的后果是：映射留着，**却没有钥匙**。
//	于是「重试」在物理上不可能，那句"需手动到节点清理"也缺了最关键的信息 ——
//	手动去删时，你不知道连哪个地址、用哪把 key、删哪个 uid。
//
//	本函数把这三样一起落盘，让补删从"不可能"变成"可执行"。
//
// 凭据为空的处理：快照没取到时（loadPortalTarget 失败）也照样登记 ——
// 那种情况补删基本无解，但"留痕"本身有价值：至少运维知道这台机器上
// 还站着买家的账号，能去机器上手工处理。
//
// 幂等：靠 idx_orphan_accounts_unique_pending 这个部分唯一索引，
// 同一次删除重试时不会堆出重复行（ON CONFLICT DO UPDATE 刷新原因与时间）。
func (s *Server) recordOrphanAccounts(
	ctx context.Context,
	nodeID, nodeName, nodeAPIURL string,
	snapshot *portalTarget,
	failed []revokeFailure,
) int {
	// 密文快照：snapshot.APIKey 是已解密的明文，这里要重新加密后入库。
	// 复用 secrets.Box（与 hysteria_nodes.api_key 同一套），不落明文。
	var cipher []byte
	if snapshot != nil && snapshot.APIKey != "" {
		if s.box == nil {
			s.logger.Warn("recordOrphanAccounts: 服务端未配置加密密钥，凭据快照无法留存",
				"node_id", nodeID)
		} else if encrypted, err := s.box.Seal([]byte(snapshot.APIKey)); err != nil {
			s.logger.Error("recordOrphanAccounts: 加密凭据快照失败", "node_id", nodeID, "error", err)
		} else {
			cipher = encrypted
		}
	}
	// 快照里的地址优先于库里的（库里刚被抹成 NULL，前面读到的是抹之前的值，
	// 两者本该相同；用快照更贴近"当时实际能用"的那个）。
	if snapshot != nil && snapshot.BaseURL != "" {
		nodeAPIURL = snapshot.BaseURL
	}

	saved := 0
	for _, f := range failed {
		if f.UserID == "" {
			continue
		}
		var subID any
		if f.SubID != "" {
			subID = f.SubID
		}
		if _, err := s.db.Exec(ctx, `
INSERT INTO orphan_node_accounts
    (node_id, node_name, node_api_url, user_id, subscription_id, api_key_cipher, reason)
VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7)
ON CONFLICT (node_id, user_id) WHERE resolved_at IS NULL
DO UPDATE SET reason = EXCLUDED.reason,
              api_key_cipher = COALESCE(EXCLUDED.api_key_cipher, orphan_node_accounts.api_key_cipher),
              -- 2026-11-06：地址与密钥成对保留。原来地址无条件覆盖：第二次登记没有凭据快照（节点已解绑 / 抹掉）时，
              -- 地址被写成空串、旧密钥却留着 —— 补删时拿着正确的密钥去连一个空地址，永远失败。
              node_api_url = CASE WHEN EXCLUDED.api_key_cipher IS NOT NULL AND EXCLUDED.node_api_url <> ''
                                  THEN EXCLUDED.node_api_url ELSE orphan_node_accounts.node_api_url END`,
			nodeID, nodeName, nodeAPIURL, f.UserID, subID, cipher, f.Reason,
		); err != nil {
			s.logger.Error("recordOrphanAccounts: 登记孤儿账号失败（这条补删线索丢了）",
				"node_id", nodeID, "user_id", f.UserID, "error", err)
			continue
		}
		saved++
		// 登记成功后，这条补删的责任已移交给 orphan_node_accounts，
		// 必须同时从订阅映射里摘掉这个已删节点。
		//
		// 不摘的后果（2026-10-14 达卡节点实测）：节点已软删，loadPortalTarget
		// 永远失败 ⇒ 到期 sweeper 的收回永远"不完整" ⇒ 该订阅永远不会被
		// 标记 enabled=FALSE，日志每 2 分钟刷一次 "revoke incomplete"。
		// 只在登记成功时才摘：登记失败就保留映射，至少还有一处线索。
		if f.SubID != "" {
			if _, err := s.db.Exec(ctx, `UPDATE subscription_tokens
				SET node_accounts=COALESCE(node_accounts,'{}'::jsonb)-$1::text
				WHERE id=$2::uuid`, nodeID, f.SubID); err != nil {
				s.logger.Error("recordOrphanAccounts: 摘除已删节点映射失败",
					"node_id", nodeID, "sub_id", f.SubID, "error", err)
			}
		}
	}
	if saved > 0 {
		s.logger.Warn("deleteNode: 已登记待补删账号，节点恢复可达后可在「节点回收」中重试",
			"node_id", nodeID, "count", saved, "has_credential", cipher != nil)
	}
	// 返回实际登记条数 —— 调用方要如实汇报"登记了几条孤儿线索"，
	// 不能自己猜一个数出来（0 会被误读成"什么都没登记"）。
	return saved
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	taskID := r.PathValue("id")
	// 与创建、重试任务保持同一权限边界：写类任务仅允许管理员取消，避免运维角色越权中断管理员排队的变更任务。
	var operation, status string
	err := s.db.QueryRow(r.Context(), `SELECT operation_type, status FROM operation_tasks WHERE id=$1::uuid`, taskID).Scan(&operation, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "只有排队中的任务可以取消"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if !readOnlyOperations[operation] && claims.Role != "admin" {
		s.auditDenied(r, "forbidden_admin_only")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "取消写类主机任务（重启服务、系统预设等）仅限管理员；只读任务不受限制"})
		return
	}
	if status != "queued" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "只有排队中的任务可以取消"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE operation_tasks SET status='cancelled', finished_at=now() WHERE id=$1::uuid AND status='queued'`, taskID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "只有排队中的任务可以取消"})
		return
	}
	s.audit(r.Context(), r, "cancel_task", "operation_task", taskID, map[string]any{"operation_type": operation})
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// trafficHourly returns real per-hour throughput buckets. Samples written by the
// portal usage reader are cumulative counters, not rates, so they are excluded.
func (s *Server) trafficHourly(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT date_trunc('hour', ts.sampled_at) AS bucket,
       COALESCE(SUM(ts.rx_bytes), 0)::bigint AS rx,
       COALESCE(SUM(ts.tx_bytes), 0)::bigint AS tx
FROM traffic_samples ts
JOIN hysteria_nodes n ON n.id = ts.node_id
JOIN servers s ON s.id = n.server_id
WHERE ts.sampled_at > now() - interval '24 hours' AND ts.source <> 'portal_used'
  AND s.deleted_at IS NULL AND n.deleted_at IS NULL
  -- 2026-11-03：同一节点有整机网卡计量（node_iface，已含 Hysteria）时不再叠加
  -- hysteria_traffic，否则这类节点的每小时流量被算两遍。
  AND (ts.source = 'node_iface' OR NOT (n.traffic_sampled_at > now() - interval '24 hours' IS TRUE))
GROUP BY bucket ORDER BY bucket ASC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0, 24)
	for rows.Next() {
		var bucket time.Time
		var rx, tx int64
		if err := rows.Scan(&bucket, &rx, &tx); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{"bucket": bucket, "rx_bytes": rx, "tx_bytes": tx})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) auditLogs(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}
	rows, err := s.db.Query(r.Context(), `
SELECT a.id, a.action, a.resource_type, COALESCE(a.resource_id::text, ''), COALESCE(u.email, ''), COALESCE(a.ip_address::text, ''), a.created_at
FROM audit_logs a LEFT JOIN users u ON u.id = a.user_id
ORDER BY a.created_at DESC LIMIT $1`, limit)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var action, resourceType, resourceID, email, ip string
		var createdAt time.Time
		if err := rows.Scan(&id, &action, &resourceType, &resourceID, &email, &ip, &createdAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{"id": id, "action": action, "resource_type": resourceType, "resource_id": resourceID, "actor": email, "ip": ip, "created_at": createdAt})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `SELECT id, email, role, created_at, last_login_at FROM users ORDER BY created_at ASC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, email, role string
		var createdAt time.Time
		var lastLogin *time.Time
		if err := rows.Scan(&id, &email, &role, &createdAt, &lastLogin); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{"id": id, "email": email, "role": role, "created_at": createdAt, "last_login_at": lastLogin})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// normalizeUserEmail 去空白、转小写，并要求是一个裸地址（不带显示名）、长度 ≤ 254、无控制字符。
func normalizeUserEmail(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" || len(v) > 254 {
		return "", false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	addr, err := mail.ParseAddress(v)
	if err != nil || addr.Name != "" || addr.Address != v {
		return "", false
	}
	return v, true
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	// The admin requirement is enforced by requireAdmin at the route level.
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "邮箱必填"})
		return
	}
	// 2026-11-03：先去空白再判空，并校验格式。原来空判在 TrimSpace 之前：
	// 「   」能建出一个邮箱为空、永远登录不了的账号；控制字符也能进库并出现在 Telegram 登录提醒里。
	email, ok := normalizeUserEmail(input.Email)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "邮箱格式不正确"})
		return
	}
	// 按字符校验（见 auth.ValidatePassword 的说明）。
	if err := auth.ValidatePassword(input.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if input.Role != "admin" && input.Role != "operator" && input.Role != "viewer" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role 需为 admin/operator/viewer"})
		return
	}
	hash, err := auth.HashPassword(input.Password)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-04：按不区分大小写查重。users.email 的 UNIQUE 区分大小写，而种子管理员按配置原样存
	// （如 Admin@X.com）、这里按小写存 ⇒ 原来能再建出一个 admin@x.com。登录按 lower(email) 匹配，
	// 同一个邮箱对应两个账号：用哪个密码、登进哪个角色取决于输入的大小写。
	var dup bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1))`, email).Scan(&dup); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if dup {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该邮箱已存在"})
		return
	}
	var id string
	if err := s.db.QueryRow(r.Context(), `INSERT INTO users (email, password_hash, role) VALUES ($1, $2, $3) RETURNING id`, email, hash, input.Role).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该邮箱已存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_user", "user", id, map[string]any{"email": email, "role": input.Role})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "会话无效"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if err := auth.ValidatePassword(input.Next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var hash string
	err := s.db.QueryRow(r.Context(), `SELECT password_hash FROM users WHERE id=$1::uuid`, claims.Subject).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "账号不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-06：猜当前密码要与登录同样计次封禁（5 次 / 15 分钟，封 30 分钟）。原来只有写限流
	// （约每小时 7200 次）、猜错不计数也不审计：偷到一枚 12 小时有效的令牌，就能在线爆破出明文密码，
	// 令牌过期 / 被撤销后照样登录。
	guardKey := changePasswordGuardKey(claims.Subject)
	if until := loginGuardState.bannedUntil(guardKey, time.Now()); !until.IsZero() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "当前密码连续输错次数过多，请稍后再试"})
		return
	}
	if !auth.VerifyPassword(hash, input.Current) {
		banned := loginGuardState.fail(guardKey, time.Now())
		s.audit(r.Context(), r, "change_password_failed", "user", claims.Subject, map[string]any{"locked": banned})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码不正确"})
		return
	}
	loginGuardState.success(guardKey)
	updated, err := auth.HashPassword(input.Next)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-04：改密会作废所有已签发的令牌（token_version+1），包括本次请求自己的会话。
	// 原来只回 {"status":"updated"}：页面提示「下次登录生效」，紧接着的下一个请求就 401、被踢回登录页。
	// 现在为本会话签发一枚新版本令牌：其它设备上的旧会话照样作废，当前页面不掉线。
	var version int64
	var role, email string
	if err := s.db.QueryRow(r.Context(), `UPDATE users SET password_hash=$1, token_version=token_version+1 WHERE id=$2::uuid
RETURNING token_version, role, email`, updated, claims.Subject).Scan(&version, &role, &email); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "change_password", "user", claims.Subject, nil)
	resp := map[string]string{"status": "updated"}
	if token, err := s.signer.IssueVersion(claims.Subject, email, role, version); err == nil {
		resp["token"] = token
	} else {
		s.logger.Warn("change password: issue fresh token failed; client must log in again", "error", err)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) runtimeStatus(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	var hosts, nodes, credentials, openTasks int
	if err := s.db.QueryRow(r.Context(), `
SELECT
  (SELECT COUNT(*) FROM servers WHERE deleted_at IS NULL),
  (SELECT COUNT(*) FROM hysteria_nodes WHERE deleted_at IS NULL),
  -- 2026-11-06：只数在役主机。原来直接数 server_credentials：软删主机（deleteServer 保留凭据行）仍被计入，
  -- 这个数字会同屏大于同一张卡片上的「主机 N」，也会与「主机页里已配置凭据的台数」对不上。
  (SELECT COUNT(DISTINCT c.server_id) FROM server_credentials c
     JOIN servers sv ON sv.id = c.server_id AND sv.deleted_at IS NULL),
  (SELECT COUNT(*) FROM operation_tasks WHERE status = 'queued')`).Scan(&hosts, &nodes, &credentials, &openTasks); err != nil {
		s.databaseError(w, r, err)
		return
	}
	var lastSample *time.Time
	// 按主机取最新再取最大：走 (server_id, sampled_at) 唯一索引，
	// 不再全表扫 server_metrics（2026-11-02 审计 M2）。server_metrics.server_id
	// 是 ON DELETE RESTRICT 外键，每行都有对应的 servers 行，结果与全表 MAX 相同。
	if err := s.db.QueryRow(r.Context(), `SELECT MAX((SELECT MAX(sampled_at) FROM server_metrics m WHERE m.server_id = s.id)) FROM servers s`).Scan(&lastSample); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servers":                 hosts,
		"nodes":                   nodes,
		"servers_with_credential": credentials,
		"queued_tasks":            openTasks,
		"last_sample_at":          lastSample,
		"collector_interval":      s.collectorSeconds,
		"executor_interval":       s.executorSeconds,
		"jwt_ttl_minutes":         int(s.signer.TTL() / time.Minute),
	})
}

func (s *Server) serverDetail(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	serverID := r.PathValue("id")
	var id, name, host, sshUser, region, provider, status string
	var sshPort int
	var tags []string
	var createdAt, updatedAt time.Time
	var latency *int
	var fingerprint *string
	var allowWrite bool
	var writeUnits []string
	var hasCredential bool
	err := s.db.QueryRow(r.Context(), `
SELECT id, name, host, ssh_port, ssh_user, region, provider, status, tags, created_at, updated_at,
       latency_ms, allow_write_ops, write_units, host_fingerprint,
       EXISTS (SELECT 1 FROM server_credentials sc WHERE sc.server_id = servers.id) AS has_credential
FROM servers WHERE id=$1::uuid AND deleted_at IS NULL`, serverID).Scan(
		&id, &name, &host, &sshPort, &sshUser, &region, &provider, &status, &tags, &createdAt, &updatedAt,
		&latency, &allowWrite, &writeUnits, &fingerprint, &hasCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "host": host, "ssh_port": sshPort, "ssh_user": sshUser,
		"region": region, "provider": provider, "status": status, "tags": tags,
		"created_at": createdAt, "updated_at": updatedAt, "latency_ms": latency,
		"allow_write_ops": allowWrite, "write_units": writeUnits,
		"host_fingerprint": fingerprint, "has_credential": hasCredential,
	})
}

// alertShouldEscalate 判断一条持续中的告警是否该「升级重推」：持续超过 alertEscalateAfter、从未推送成功、
// 未确认，且最近一次 triggered / reopened 也已满 alertEscalateAfter，目标不在新接入静默期内。
func (s *Server) alertShouldEscalate(ctx context.Context, alertID string) (bool, error) {
	var escalated bool
	err := s.db.QueryRow(ctx, `
SELECT notified_at IS NULL AND status = 'open' AND first_seen_at < now() - make_interval(secs => $2)
   -- 2026-11-06：从最近一次 triggered / reopened 起也要满 alertEscalateAfter。复活会清 notified_at
   -- 但保留 first_seen_at：一条 20 分钟前开始、刚抖动复活的告警原来下一轮就推「持续未送达 · 升级」，
   -- 绕过静默期，文案「此前未成功通知」也不实（首次告警早已送达）。
   AND NOT EXISTS (SELECT 1 FROM alert_events e WHERE e.alert_id = alerts.id
                    AND e.event_type IN ('triggered','reopened')
                    AND e.created_at > now() - make_interval(secs => $2))
   -- 2026-11-06：新目标静默期内不升级。notifyAlertFor 对静默期内的目标一律不推，但升级的 1 小时冷却
   -- 已在这里被消耗、repeated 事件也已写下 —— 首条真实通知被推迟到约 75 分钟，时间线上留一条没推出去的「升级」。
   AND NOT EXISTS (SELECT 1 FROM servers sv WHERE sv.id = alerts.server_id AND sv.created_at > now() - make_interval(secs => $3))
   AND NOT EXISTS (SELECT 1 FROM hysteria_nodes hn WHERE hn.id = alerts.node_id AND hn.created_at > now() - make_interval(secs => $3))
FROM alerts WHERE id=$1::uuid`, alertID, int(alertEscalateAfter.Seconds()), int(alertNewTargetQuiet.Seconds())).Scan(&escalated)
	return escalated, err
}
