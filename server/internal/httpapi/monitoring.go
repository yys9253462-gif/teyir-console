package httpapi

// monitoring.go — 「节点监控」页的后端。
//
// 这个页面回答一个问题：**每台节点现在到底好不好用**。
// 因此它把三类数据聚到一起：
//
//   1. 主机/节点的实时状态（来自 collector 的周期采集）
//   2. 面板到主机的**真实网络延迟**（不是 SSH 握手耗时，见下）
//   3. 大陆三网测速结果（在节点本机测出，见 speedtest 包）
//
// 关于「延迟」这个词在本项目里的两处含义
// --------------------------------------
// servers.latency_ms 是 **SSH 握手耗时**（collector 从拨号开始计时到
// SSH 建连完成）。它对诊断有价值，但对"节点好不好用"这个问题是错的口径：
// 一个 SSH 端口被封、但代理服务正常的节点会显示"离线、无延迟"。
//
// 所以本页展示的是 net_latency_ms —— 面板到该主机 SSH 端口的**纯 TCP 建连
// 耗时**（不含 SSH 握手）。两个字段同时返回，前端分别标注，避免用户
// 把一个当成另一个。
//
// 关于"三网测速"的诚实边界
// ------------------------
// 测速在**节点本机**执行，测的是「节点 ↔ 大陆三网」。这能反映这台 VPS
// 回国的线路质量（CN2 GIA / 三网直连说的就是这个）。
//
// 它**不能**告诉你「你家宽带连这台节点的速度」—— 那取决于你家到节点的
// 链路，任何从服务器侧发起的测量都测不到。界面上必须把这一点讲清楚，
// 否则用户会把"节点到大陆 50ms"误读成"我用起来就是 50ms"。

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"nodeops/server/internal/speedtest"
)

// carrierLabels 把运营商标识翻译成展示名。
//
// 与迁移 016 的 CHECK 约束一一对应。放在这里而不是前端，
// 是因为后端已在别处（如告警文案）需要它，两处各写一份必然分叉。
var carrierLabels = map[string]string{
	"telecom": "电信",
	"unicom":  "联通",
	"mobile":  "移动",
	"other":   "公共",
}

func carrierLabel(key string) string {
	if label, ok := carrierLabels[key]; ok {
		return label
	}
	// 未知分类不隐藏、也不编造，原样返回 —— 看到 "foo" 比看到空白
	// 更容易定位"谁往表里写了脏值"。
	return key
}

// nodeMonitoring 是 GET /api/monitoring/nodes 的响应单元。
type nodeMonitoring struct {
	NodeID     string `json:"node_id"`
	NodeName   string `json:"node_name"`
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	Host       string `json:"host"`
	Region     string `json:"region"`
	Provider   string `json:"provider"`

	NodeStatus string `json:"node_status"` // online / offline / unknown
	// 状态是怎么来的：api / ssh / 空（从未探测）。
	// 前端据此说明「这个节点没指标是因为它走的是 API 探测，
	// 不是采集器坏了」—— 没有这一栏，用户看到空白指标会误判。
	StatusSource string `json:"status_source"`
	// 节点级错误，例如 "unreachable" / "unauthorized"。
	// 前端 hover 节点状态时显示具体原因，而不是只看到一个"offline"。
	NodeStatusError string `json:"node_status_error"`
	ListenPort      int    `json:"listen_port"`
	UsersCount      int    `json:"users_count"`

	// 主机状态（collector 采到的那份）。
	ServerStatus string `json:"server_status"`
	// 主机级错误。最常见的是「SSH 凭据失效」—— 此时节点可能还在线（走 API
	// 探测的话仍显示 online），但**没有 CPU/内存/流量数据**。
	// 之前没暴露这一栏，前端就不知道为啥数据是空的，会以为采集器坏了。
	ServerStatusError string  `json:"server_status_error"`
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
	DiskPercent       float64 `json:"disk_percent"`
	LoadAverage       string  `json:"load_average"`
	// MetricsSampledAt 是上面四个指标对应的采样时刻。
	//
	// ⚠️ 2026-10-30：原来只给数值、不给时间。而查询取的是「该主机最新一条
	// server_metrics」，**不限时效** —— 一台 SSH 已经断了一周的机器，
	// 页面上仍然显示它一周前的 CPU 百分比，而且和刚采到的一模一样。
	// 这正是本项目反复要避开的「把陈旧说成现在」。
	//
	// 现在把采样时刻一并给出：值为 nil（或超出新鲜窗口）时前端显示「—」，
	// 并说明「指标已过期」，而不是拿一个旧数字冒充当前负载。
	MetricsSampledAt *time.Time `json:"metrics_sampled_at,omitempty"`

	// 网络延迟：面板 → 该主机的纯 TCP 建连耗时。
	// nil 表示还没测过（与 0ms 不同）。
	NetLatencyMs *int       `json:"net_latency_ms"`
	NetCheckedAt *time.Time `json:"net_latency_checked_at"`

	// SSH 握手耗时（保留原名语义，供诊断，前端单独标注）。
	SSHHandshakeMs *int `json:"ssh_handshake_ms"`

	TxBytes24h int64 `json:"tx_bytes_24h"`
	RxBytes24h int64 `json:"rx_bytes_24h"`

	// Speedtest 是这台节点最近一轮的三网测速结果（可能为空）。
	Speedtest *nodeSpeedtestSummary `json:"speedtest"`
}

// nodeSpeedtestSummary 是"最近一轮"的测速结果聚合。
type nodeSpeedtestSummary struct {
	MeasuredAt time.Time `json:"measured_at"`
	// ProbeOK 为 false 时上面的 Targets 仍可能有内容，
	// 但每条都带着自己的 ProbeError（比如 NO_PING）。
	ProbeOK    bool                  `json:"probe_ok"`
	ProbeError string                `json:"probe_error"`
	Targets    []speedtestTargetView `json:"targets"`
	// ByCarrier 是给前端直接渲染"三网卡片"用的分组视图。
	ByCarrier map[string]carrierAggregate `json:"by_carrier"`
}

type speedtestTargetView struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Carrier      string `json:"carrier"`
	CarrierLabel string `json:"carrier_label"`

	ICMPAvgMs    *float64 `json:"icmp_avg_ms"`
	ICMPLossPct  *float64 `json:"icmp_loss_pct"`
	TCPConnectMs *float64 `json:"tcp_connect_ms"`
	DownloadMbps *float64 `json:"download_mbps"`
	UploadMbps   *float64 `json:"upload_mbps"`

	ProbeOK    bool   `json:"probe_ok"`
	ProbeError string `json:"probe_error"`
}

// carrierAggregate 是某个运营商的汇总（用于三网卡片）。
type carrierAggregate struct {
	Carrier string `json:"carrier"`
	Label   string `json:"label"`
	// AvgMs 是该运营商**所有可用靶点**的平均延迟。
	// nil 表示这个运营商一个都没测到 —— 前端据此显示"未测得"而不是 0。
	AvgMs *float64 `json:"avg_ms"`
	// AvgLossPct 平均丢包率。
	AvgLossPct *float64 `json:"avg_loss_pct"`
	// BestTarget 是延迟最低的靶点名（"移动-北京 54ms"这种展示用）。
	BestTarget string   `json:"best_target"`
	BestMs     *float64 `json:"best_ms"`
	// MeasuredTargets 是实际测到值的靶点数 / 总靶点数。
	// 分母很重要：3 个靶点里只测到 1 个时，"平均 50ms"的说服力完全不同。
	MeasuredTargets int `json:"measured_targets"`
	TotalTargets    int `json:"total_targets"`
}

func (s *Server) nodeMonitoring(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}

	// 一次查询把节点、主机、指标、延迟全部取出。
	//
	// 为什么用聚合而不是逐节点查：这个页面是"总览"性质，节点数一多，
	// N+1 查询会让加载时间线性增长。用一个带 LATERAL 的查询一次拿完，
	// 数据库只扫一遍。
	//
	// ⚠️ 两个都在**真机才暴露**的坑，都记在这里：
	//
	// 【坑 1】`server_metrics.load_average` 是 **jsonb** 类型，不是 text。
	// 最初写 `COALESCE(m.load_average, '')` —— 拿空字符串去和 jsonb 比，
	// Postgres 报：
	//     ERROR: invalid input syntax for type json
	//     CONTEXT: JSON data, line 1:
	// 这个报错**完全没提 load_average**，只说"json 语法错"，
	// 所以我一开始去翻哪里有 JSON 字段、怀疑是 uuid 数组传参，
	// 白找了两轮。教训：报错里的类型名不等于出问题的字段名。
	// 正确写法是 `COALESCE(m.load_average::text, '')`。
	//
	// 【坑 2】指标取的是**每台主机最新一条** server_metrics：
	// 不加 ORDER BY ... LIMIT 1 会让同一主机返回多行、节点被重复列出。
	rows, err := s.db.Query(r.Context(), `
SELECT n.id, n.name, n.status, COALESCE(n.status_source,''), COALESCE(n.status_error,''), n.listen_port, COALESCE(n.users_count, -1),
       s.id, s.name, s.host, COALESCE(s.region, ''), COALESCE(s.provider, ''),
       s.status, COALESCE(s.status_error,''), s.net_latency_ms, s.net_latency_checked_at, s.latency_ms,
       COALESCE(m.cpu_percent, 0), COALESCE(m.memory_used_bytes, 0), COALESCE(m.memory_total_bytes, 0),
       COALESCE(m.disk_used_bytes, 0), COALESCE(m.disk_total_bytes, 0),
       COALESCE(m.load_average::text, ''), m.sampled_at,
       COALESCE(t.tx, 0), COALESCE(t.rx, 0)
FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id
LEFT JOIN LATERAL (
    SELECT cpu_percent, memory_used_bytes, memory_total_bytes, disk_used_bytes, disk_total_bytes, load_average, sampled_at
    FROM server_metrics x
    -- ⚠️ 2026-10-30：必须带时效窗口。原来只 ORDER BY ... LIMIT 1，
    -- 主机 SSH 停采一周后仍会把一周前那一行当成"最新指标"返回，
    -- 页面上和刚采到的完全一样。超过窗口就不给数值（NULL ⇒ 前端显示 —）。
    WHERE x.server_id = s.id AND x.sampled_at > now() - interval '15 minutes'
    ORDER BY x.sampled_at DESC LIMIT 1
) m ON true
LEFT JOIN LATERAL (
    -- 24 小时流量。
    --
    -- ⚠️ 两种 source 的**语义完全不同，必须分开算**（2026-10-02 修：
    -- 原来无差别 SUM，导致阿什本显示 11.88 TB 而真实值是 91 MB，差 13 万倍）。
    --
    --   portal_used      —— portal.json 的 used_bytes 是**累计计数器**，
    --                       每轮覆盖写同一个分钟桶。它的 24h 用量是
    --                       max - min（区间内的增量），**绝不能 SUM**：
    --                       把 322 个几乎相同的累计值相加就是那个 11.88 TB。
    --   hysteria_traffic —— /traffic?clear=true 每轮清空，
    --                       每次读到的就是"上轮以来的字节数"，是真增量，
    --                       同一分钟内多次读还会累加进同一个桶。
    --                       它的 24h 用量是**求和**，这是对的。
    --
    -- 为什么这个 BUG 一直没被发现：哥伦布那台只有 hysteria_traffic，
    -- 用 SUM 恰好正确 —— **列表里有一台显示正常，看着就不像坏了**。
    --
    -- 两者同时存在时以 hysteria_traffic 为准（它是真实上下行、还分 tx/rx）；
    -- 只有它缺失时才退回 portal 口径。
    -- 2026-11-06：有整机网卡计量（node_iface）时只用它（已包含 Hysteria 的流量，与节点页同一口径）。
    -- 原来完全不读它：只接 API 的节点没有 hysteria_traffic / portal_used，24h 流量显示 0、合计与排序都少算。
    SELECT
        COALESCE(
            SUM(ts.tx_bytes) FILTER (WHERE ts.source = 'node_iface'),
            SUM(ts.tx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic'),
            -- 2026-11-03：portal_used 是全体用户 used_bytes 之和，删掉一个用户（到期清理、
            -- 商城撤单）总和就会**下降**；max-min 会把这次下降当成新增流量
            -- （删一个用过 50 GB 的用户 ⇒ 凭空多出 50 GB）。改为逐节点只累加正向增量；
            -- 单调时与 max-min 完全相等。
            (SELECT SUM(GREATEST(d.delta, 0)) FROM (
                SELECT p.tx_bytes - LAG(p.tx_bytes) OVER (PARTITION BY p.node_id ORDER BY p.sampled_at) AS delta
                FROM traffic_samples p
                WHERE p.node_id = n.id AND p.source = 'portal_used' AND p.sampled_at > now() - interval '24 hours'
            ) d),
            0
        )::bigint AS tx,
        COALESCE(
            SUM(ts.rx_bytes) FILTER (WHERE ts.source = 'node_iface'),
            SUM(ts.rx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic'),
            0
        )::bigint AS rx
    FROM traffic_samples ts
    -- 2026-11-04：按**节点**取。这一页一行一个节点，原来按主机（hn.server_id = s.id）汇总，
    -- 同一主机上的 3 个节点每个都显示整机总量，页面顶部合计再加一遍就是 3 倍。
    WHERE ts.node_id = n.id AND ts.sampled_at > now() - interval '24 hours'
) t ON true
WHERE n.deleted_at IS NULL AND s.deleted_at IS NULL
ORDER BY s.created_at, n.created_at`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	items := make([]nodeMonitoring, 0)
	nodeIDs := make([]string, 0)
	for rows.Next() {
		var item nodeMonitoring
		var memUsed, memTotal, diskUsed, diskTotal int64
		if err := rows.Scan(
			&item.NodeID, &item.NodeName, &item.NodeStatus, &item.StatusSource, &item.NodeStatusError, &item.ListenPort, &item.UsersCount,
			&item.ServerID, &item.ServerName, &item.Host, &item.Region, &item.Provider,
			&item.ServerStatus, &item.ServerStatusError, &item.NetLatencyMs, &item.NetCheckedAt, &item.SSHHandshakeMs,
			&item.CPUPercent, &memUsed, &memTotal, &diskUsed, &diskTotal, &item.LoadAverage, &item.MetricsSampledAt,
			&item.TxBytes24h, &item.RxBytes24h,
		); err != nil {
			s.databaseError(w, r, err)
			return
		}
		item.MemoryPercent = percent(memUsed, memTotal)
		item.DiskPercent = percent(diskUsed, diskTotal)
		items = append(items, item)
		nodeIDs = append(nodeIDs, item.NodeID)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}

	// 一次性把所有这些节点的最近一轮测速结果取出来。
	//
	// 「最近一轮」的判定：取每个节点里 measured_at 最大的那个时间戳，
	// 再取该时间戳附近（同一轮探针是连续插入的）的全部行。
	// 用一个 60 秒的窗口而不是精确相等 —— 探针是逐行插入的，
	// 同一轮里各行的时间戳可能差几百毫秒（now() 每次求值）。
	summaries, err := s.loadSpeedtestSummaries(r.Context(), nodeIDs)
	speedtestError := ""
	if err != nil {
		// 测速数据缺失不该让整个监控页打不开 —— 那是"锦上添花"的数据，
		// 主数据（节点状态、指标）已经拿到了。
		// 2026-11-04：但必须告诉前端「这次没读到」。原来只记日志，每张卡片都显示「未测得」，
		// 与真的没有测速数据的节点长得一模一样。
		s.logger.Warn("monitoring: 读取测速结果失败", "error", err)
		speedtestError = "三网测速结果读取失败，本次未显示（不代表没有测速数据），请稍后刷新"
	} else {
		for i := range items {
			if summary, ok := summaries[items[i].NodeID]; ok {
				items[i].Speedtest = summary
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":           items,
		"count":           len(items),
		"speedtest_error": speedtestError,
		// 三网测速的语义边界，直接由后端给出，避免前端各处自行措辞不一致。
		"speedtest_note": "测速在节点本机执行，反映的是「节点 ↔ 大陆三网」的线路质量；" +
			"它不代表你家宽带连这台节点的速度。",
	})
}

// 百分比计算复用 server.go 里既有的 percent()（它已处理 total<=0 与负值）。
// 这里刻意不重复实现：同一件事有两份实现，迟早会在边界上分叉，
// 而"内存占用 -1%"这种数字很难归因到是哪一份算出来的。

// loadSpeedtestSummaries 批量读每个节点的最近一轮测速结果。
//
// ⚠️ 「最近一轮」的判定比看起来难，这里踩过一次：
//
// 第一版用 `MAX(measured_at)` + 「60 秒窗口」（BETWEEN max-60s AND max）
// 来圈定一轮。理由是同轮的各行是逐条 INSERT 的，时间戳会差几百毫秒。
// 但真机上出了问题：**测速每跑一次就写一轮，两轮间隔只要小于 60 秒，
// 就都会被圈进来** —— 表现为同一个靶点在界面上出现两次，
// 且 by_carrier 的平均值把两轮混在一起算（读数偏了）。
//
// 正确的判据是**轮次本身**，不是时间窗口。所以给 node_speedtests 加一个
// round_id（见迁移 017）：同一个节点的一次探针执行共享一个 round_id。
// 这样"取最近一轮"就是 `round_id = (该节点最新那轮的 id)`，精确且无歧义。
//
// 为什么不用「按 (node_id, target_key) 取最新一条」：
// 那样虽然也能去重，但如果某一轮里某个靶点**没测到**（探针漏了或失败），
// 会退回到上一轮的旧值 —— 于是页面上显示的是一个"拼起来"的轮次，
// 你不知道哪些数字是新的、哪些是旧的。那比重复更糟。
func (s *Server) loadSpeedtestSummaries(ctx context.Context, nodeIDs []string) (map[string]*nodeSpeedtestSummary, error) {
	out := make(map[string]*nodeSpeedtestSummary, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}

	// 取每个节点**最近一轮**的结果行。
	//
	// 每个节点用 LATERAL ... LIMIT 1 沿 (node_id, measured_at DESC) 索引取最新一行的
	// round_id，再据此取该轮全部行。原来的 DISTINCT ON 要把 7 天内所有行
	// （约 10 万行）读出来排序后再去重（2026-11-02 审计）。
	//
	// 关于 $1 传参：pgx 会把 Go 的 []string 编成 text[]，所以这里用
	// `unnest(...)::uuid` 显式转换，而不是 `= ANY($1::uuid[])`
	// （后者在真机上会报一个与 uuid 无关的 json 语法错，见下面主查询的注释）。
	rows, err := s.db.Query(ctx, `
WITH latest AS (
    SELECT ids.node_id, l.round_id
    FROM (SELECT DISTINCT unnest($1::text[])::uuid AS node_id) ids
    CROSS JOIN LATERAL (
        SELECT round_id
        FROM node_speedtests
        WHERE node_id = ids.node_id
          AND measured_at > now() - interval '7 days'
        ORDER BY measured_at DESC
        LIMIT 1
    ) l
)
SELECT st.node_id, st.target_key, st.target_name, st.target_host, st.carrier,
       st.icmp_avg_ms, st.icmp_loss_pct, st.tcp_connect_ms, st.download_mbps, st.upload_mbps,
       st.probe_ok, st.probe_error, st.measured_at
FROM node_speedtests st
JOIN latest ON latest.node_id = st.node_id AND latest.round_id = st.round_id
ORDER BY st.node_id, st.carrier, st.target_key`, nodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID string
		var view speedtestTargetView
		var measuredAt time.Time
		var probeOK bool
		var probeErr string
		if err := rows.Scan(&nodeID, &view.Key, &view.Name, &view.Host, &view.Carrier,
			&view.ICMPAvgMs, &view.ICMPLossPct, &view.TCPConnectMs, &view.DownloadMbps, &view.UploadMbps,
			&probeOK, &probeErr, &measuredAt); err != nil {
			return nil, err
		}
		view.CarrierLabel = carrierLabel(view.Carrier)
		view.ProbeOK = probeOK
		view.ProbeError = probeErr

		summary, ok := out[nodeID]
		if !ok {
			summary = &nodeSpeedtestSummary{
				MeasuredAt: measuredAt,
				ProbeOK:    true,
				ByCarrier:  map[string]carrierAggregate{},
			}
			out[nodeID] = summary
		}
		summary.Targets = append(summary.Targets, view)
		// 只要有一条探针失败，整轮就标记为不完整 —— 但要保留每条的
		// ProbeError，因为"哪个靶点失败、为什么"才是可操作的信息。
		if !probeOK {
			summary.ProbeOK = false
			if summary.ProbeError == "" {
				summary.ProbeError = probeErr
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, summary := range out {
		summary.ByCarrier = aggregateByCarrier(summary.Targets)
	}
	return out, nil
}

// aggregateByCarrier 把逐靶点结果按运营商汇总。
//
// 全部是纯计算，单独抽出来便于单测 —— 这里的边界条件（一个都没测到、
// 部分测得、值为 0）正是容易算错又难发现的地方。
func aggregateByCarrier(targets []speedtestTargetView) map[string]carrierAggregate {
	groups := map[string][]speedtestTargetView{}
	for _, t := range targets {
		groups[t.Carrier] = append(groups[t.Carrier], t)
	}

	out := make(map[string]carrierAggregate, len(groups))
	for carrier, list := range groups {
		agg := carrierAggregate{
			Carrier:      carrier,
			Label:        carrierLabel(carrier),
			TotalTargets: len(list),
		}
		var sumMs, sumLoss float64
		var countMs, countLoss int
		for _, t := range list {
			if t.ICMPAvgMs != nil {
				sumMs += *t.ICMPAvgMs
				countMs++
				if agg.BestMs == nil || *t.ICMPAvgMs < *agg.BestMs {
					value := *t.ICMPAvgMs
					agg.BestMs = &value
					agg.BestTarget = t.Name
				}
			}
			if t.ICMPLossPct != nil {
				sumLoss += *t.ICMPLossPct
				countLoss++
			}
		}
		if countMs > 0 {
			avg := sumMs / float64(countMs)
			agg.AvgMs = &avg
		}
		if countLoss > 0 {
			avg := sumLoss / float64(countLoss)
			agg.AvgLossPct = &avg
		}
		agg.MeasuredTargets = countMs
		out[carrier] = agg
	}
	return out
}

// ---------------------------------------------------------------------------
// 测速靶点管理
// ---------------------------------------------------------------------------

// speedtestTargetRow 是靶点在 API 上的形态。
type speedtestTargetRow struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Carrier      string `json:"carrier"`
	CarrierLabel string `json:"carrier_label"`
	Enabled      bool   `json:"enabled"`
	SortOrder    int    `json:"sort_order"`
}

// GET /api/speedtest/targets — 列出全部靶点（含禁用的，便于用户启用）。
func (s *Server) speedtestTargets(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT id, target_key, name, host, carrier, enabled, sort_order
FROM speedtest_targets ORDER BY sort_order, target_key`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	result := make([]speedtestTargetRow, 0)
	for rows.Next() {
		var row speedtestTargetRow
		if err := rows.Scan(&row.ID, &row.Key, &row.Name, &row.Host, &row.Carrier, &row.Enabled, &row.SortOrder); err != nil {
			s.databaseError(w, r, err)
			return
		}
		row.CarrierLabel = carrierLabel(row.Carrier)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// maxProbeConcurrency 限制同时进行的 SSH 测速数。
//
// 为什么需要限制：测速要对每台主机开一个 SSH 会话、每个会话串行跑 12 个
// ping（最坏 72 秒）。节点一多，同时发起会让面板自己的内存与文件描述符
// 吃紧（api 容器 mem_limit 只有 256m），也会让被测量的节点同时承受负载 ——
// 那会影响测速结果本身（我们正在测它的网络，却自己制造了拥塞）。
//
// 4 是个保守值：生产环境 6 台主机，两轮就能跑完；再多的集群也只是
// 多花几个周期，而面板不会因为一次测速被打爆。
const maxProbeConcurrency = 4

// POST /api/speedtest/run — 手动触发一轮测速。
//
// 手动触发的价值：用户改了靶点、或刚给节点配好 SSH 凭据后，
// 不想等下一个周期（默认 15 分钟）就想看到结果。
func (s *Server) speedtestRun(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	// 这是**写操作**：它会 SSH 到所有节点执行命令。用写限流，
	// 因为它对目标机器的打扰是真实的（每个节点跑 12 个 ping）。
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if s.prober == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "测速功能不可用：服务端未配置凭据加密密钥",
		})
		return
	}

	// 2026-11-04：同一范围（全量，或同一个节点）同一时刻只跑一轮。原来每点一次就在后台再起一轮：
	// 双击、刷新重发或 Telegram 重试时，同一批节点被并发 SSH / ping 好几轮（每台 18 个靶点），
	// 写限流（10 秒 20 次）挡不住。进行中再触发时直接告诉操作者，不再叠加；不同节点互不影响。
	// 占位在查库之前：没有后台任务要启动的返回路径都会释放它，只有后台任务结束时才由它释放。
	scope := strings.TrimSpace(r.URL.Query().Get("node_id"))
	if scope != "" {
		norm, ok := canonicalUUID(scope)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "节点 ID 格式不正确"})
			return
		}
		scope = norm
	}
	if _, busy := manualSpeedtestRunning.LoadOrStore(scope, struct{}{}); busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "上一轮测速还在进行，完成后再触发"})
		return
	}
	release := func() { manualSpeedtestRunning.Delete(scope) }
	started := false
	defer func() {
		if !started {
			release()
		}
	}()

	// 取待测速的节点。
	//
	// 两条通道：
	//   - 有 SSH 凭据的主机 → 面板 SSH 过去跑探针脚本（原有路径，节点零改动）
	//   - 没凭据但绑了 portal API 的节点 → 调节点的 /api/v1/speedtest
	//     （节点自己跑 ping，见 speedtest/portal.go）
	//
	// 原来这条 SQL 里有 `JOIN LATERAL (... server_credentials ...) hascred ON true`，
	// 把没凭据的主机**整个排除**—— 于是页面上是四行「未测得」，
	// 而真实情况是"压根没启用这条通道"，不是线路问题（实测 2/5 台如此）。
	//
	// 两条都拿不到的（既没 SSH 也没绑 portal）仍然跳过，但会记进
	// skipped 里回给用户，不让"点了测速却没反应"变成谜题。
	rows, err := s.db.Query(r.Context(), `
SELECT s.id, s.name, n.id::text, n.name, COALESCE(n.api_url, ''), n.api_key,
       EXISTS (SELECT 1 FROM server_credentials x WHERE x.server_id = s.id)
FROM servers s
JOIN hysteria_nodes n ON n.server_id = s.id AND n.deleted_at IS NULL
WHERE s.deleted_at IS NULL
  AND ($1 = '' OR n.id::text = $1)
ORDER BY s.name, n.name`, scope)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// portalJob 是一次「让节点自己测速」的调用。
	//
	// 需要 serverID：node_speedtests 表上冗余存了一份 server_id，
	// 而 portal 通道不像 SSH 通道那样能从调用处拿到它（SSH 路径的
	// serverID 是"跑探针的那台主机"，这里必须显式带上，否则插库缺列）。
	type portalJob struct {
		nodeID     string
		nodeName   string
		serverID   string
		serverName string
		baseURL    string
		apiKey     string
	}
	type probeJob struct {
		serverID   string
		serverName string
		nodeIDs    []string
		// fallback：这台主机上绑了节点 API 的节点。SSH 测速失败（凭据失效、指纹未登记、sshd 不可达）时
		// 改由节点自己测（2026-11-06）。原来「凭据坏了」两条通道都不走，这些节点永远测不到。
		fallback []portalJob
	}
	var jobs []probeJob
	portalJobs := make([]portalJob, 0)
	skipped := make([]string, 0)
	// serverIdx 把 serverID 映射到 jobs 里的下标，让同一主机的多个节点
	// 归到同一个 SSH 任务（共用一轮 SSH 会话，与原行为一致）。
	serverIdx := map[string]int{}
	for rows.Next() {
		var serverID, serverName, nodeID, nodeName, baseURL string
		var ciphertext []byte
		var hasCred bool
		if err := rows.Scan(&serverID, &serverName, &nodeID, &nodeName, &baseURL, &ciphertext, &hasCred); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		if hasCred {
			// 有 SSH 凭据 → 归到该主机的 SSH 任务；绑了 API 的节点同时备好 portal 兜底。
			i, ok := serverIdx[serverID]
			if !ok {
				i = len(jobs)
				serverIdx[serverID] = i
				jobs = append(jobs, probeJob{serverID: serverID, serverName: serverName})
			}
			jobs[i].nodeIDs = append(jobs[i].nodeIDs, nodeID)
			if baseURL != "" && len(ciphertext) > 0 && s.box != nil {
				if plain, err := s.box.Open(ciphertext); err == nil {
					apiKey := string(plain)
					clear(plain)
					jobs[i].fallback = append(jobs[i].fallback, portalJob{
						nodeID: nodeID, nodeName: nodeName,
						serverID: serverID, serverName: serverName,
						baseURL: baseURL, apiKey: apiKey,
					})
				}
			}
			continue
		}
		// 没 SSH 凭据：看有没有绑 portal API。
		if baseURL == "" || len(ciphertext) == 0 {
			skipped = append(skipped, fmt.Sprintf("%s/%s（无 SSH 凭据且未绑定节点 API）", serverName, nodeName))
			continue
		}
		if s.box == nil {
			skipped = append(skipped, fmt.Sprintf("%s/%s（服务端未配置解密密钥）", serverName, nodeName))
			continue
		}
		plain, err := s.box.Open(ciphertext)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s/%s（API 密钥解密失败）", serverName, nodeName))
			continue
		}
		apiKey := string(plain)
		clear(plain)
		portalJobs = append(portalJobs, portalJob{
			nodeID: nodeID, nodeName: nodeName,
			serverID: serverID, serverName: serverName,
			baseURL: baseURL, apiKey: apiKey,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	rows.Close()

	if len(jobs) == 0 && len(portalJobs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"started": 0,
			"skipped": skipped,
			"message": "没有可测速的节点（需要已绑定 SSH 凭据的主机，或已绑定节点 API 的节点）",
		})
		return
	}

	// 拿到真实 IP 后立刻返回，测速在后台跑。
	//
	// 为什么不同步等待：一轮全量测速最坏要 100+ 秒 × 批次数，HTTP 请求
	// 挂那么久会被反代（Caddy）超时切断，用户看到的是"请求失败"
	// 而实际测速已经跑完 —— 那是纯粹的误导。改成后台跑 + 前端轮询。
	//
	// 用 context.Background() 而不是 r.Context()：请求一返回，
	// r.Context() 就被取消，后台任务会被立刻中断（这是很容易写错的一点）。
	started = true
	go func() {
		defer release()
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("speedtest: 后台测速主协程异常恢复", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		sem := make(chan struct{}, maxProbeConcurrency)
		total := len(jobs) + len(portalJobs)
		done := make(chan struct{}, total)
		for _, job := range jobs {
			go func(job probeJob) {
				defer func() { done <- struct{}{} }()
				defer func() {
					if rec := recover(); rec != nil {
						s.logger.Error("speedtest: 主机测速子协程异常恢复", "server", job.serverName, "panic", rec, "stack", string(debug.Stack()))
					}
				}()
				sem <- struct{}{}
				defer func() { <-sem }()
				if _, err := s.prober.RunOnServer(ctx, job.serverID, job.nodeIDs); err != nil {
					s.logger.Warn("speedtest: 主机测速失败",
						"server", job.serverName, "error", err, "portal_fallback", len(job.fallback))
					for _, f := range job.fallback {
						if ctx.Err() != nil {
							return
						}
						if err := s.prober.RunOnPortalAndStore(ctx, f.nodeID, f.serverID, f.baseURL, f.apiKey); err != nil {
							s.logger.Warn("speedtest: SSH 失败后改走节点 portal 也失败",
								"node", f.nodeName, "error", err)
						}
					}
				}
			}(job)
		}
		// portal 通道：每台主机一个任务（同主机的多个节点各自调一次 ——
		// 它们的测速在节点本机执行，与"同主机共用一次 SSH 会话"不同）。
		for _, job := range portalJobs {
			go func(job portalJob) {
				defer func() { done <- struct{}{} }()
				defer func() {
					if rec := recover(); rec != nil {
						s.logger.Error("speedtest: 门户测速子协程异常恢复", "node", job.nodeName, "panic", rec, "stack", string(debug.Stack()))
					}
				}()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := s.prober.RunOnPortalAndStore(ctx, job.nodeID, job.serverID, job.baseURL, job.apiKey); err != nil {
					s.logger.Warn("speedtest: 节点 portal 测速失败",
						"node", job.nodeName, "error", err)
				}
			}(job)
		}
		for i := 0; i < total; i++ {
			<-done
		}
		s.logger.Info("speedtest: 手动触发的全量测速完成",
			"ssh_servers", len(jobs), "portal_nodes", len(portalJobs), "skipped", len(skipped))
	}()

	s.audit(r.Context(), r, "run_speedtest", "speedtest", "", map[string]any{
		"servers": len(jobs), "portal_nodes": len(portalJobs), "skipped": len(skipped),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":           true,
		"started":      len(jobs) + len(portalJobs),
		"skipped":      skipped,
		"skipped_note": "以下节点两条测速通道都不可用，本次未测",
		"message":      "测速已在后台开始，请稍后刷新查看结果",
	})
}

// manualSpeedtestRunning 记录仍在后台进行的手动测速，键为范围（"" = 全量，否则为节点 id）。
var manualSpeedtestRunning sync.Map

// 确保 speedtest 包被引用（LoadTargets 在状态端点里用）。
var _ = speedtest.Target{}
