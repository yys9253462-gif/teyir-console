package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 单节点利润报表与账号共享检测（2026-10-15）。

// ---------------------------------------------------------------- 收入

// orderPeriodDays 算这笔订单买了多少天，用于把金额折算成月收入。
// 商城可能只传 duration_days，也可能只传绝对的 expires_at（现网商城就是后者）：
//   - 有 duration_days 直接用；
//   - 否则用「新到期 − 起点」，起点是旧到期时间（续费）与现在中较晚的那个。
func orderPeriodDays(durationDays int, oldExpiry, newExpiry *time.Time) int {
	if durationDays > 0 {
		return durationDays
	}
	if newExpiry == nil {
		return 0
	}
	start := time.Now()
	if oldExpiry != nil && oldExpiry.After(start) {
		start = *oldExpiry
	}
	days := int(math.Round(newExpiry.Sub(start).Hours() / 24))
	if days < 1 {
		return 0
	}
	return days
}

// recordOrderRevenue 把商城订单金额折算成月收入记到订阅上。
// 商城没传金额（老版本）时什么都不做，保留原值 —— 不能把「没传」当成「0 元」。
// 返回 (是否记账, 未记账原因)。2026-11-06：原来静默 return —— 商城只传了 amount、没给周期
// （duration_days 缺省且未给 expires_at）时这笔已付金额既不进 revenue_monthly、也不留任何痕迹，
// 报表的「N 个订阅已填收入」也不会包含它。续费路径当时已经补了审计，下单路径没有。
func (s *Server) recordOrderRevenue(ctx context.Context, subscriptionID string, amount float64, currency string, durationDays int) (bool, string) {
	if amount <= 0 {
		return false, "" // 商城没传金额（老版本）：本来就不该记账，也不是异常
	}
	if durationDays <= 0 {
		return false, "no_period"
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "CNY"
	}
	if !billingCurrencies[currency] {
		s.logger.Warn("store: unsupported revenue currency, ignored", "currency", currency)
		return false, "unsupported_currency"
	}
	monthly := math.Round(amount/(float64(durationDays)/30)*100) / 100
	if _, err := s.db.Exec(ctx, `UPDATE subscription_tokens SET revenue_monthly=$2, revenue_currency=$3 WHERE id=$1::uuid`,
		subscriptionID, monthly, currency); err != nil {
		s.logger.Warn("store: record revenue failed", "subscription", subscriptionID, "error", err)
		return false, "db_error"
	}
	return true, ""
}

// PUT /api/subscriptions/{id}/revenue  {"revenue_monthly": 30, "revenue_currency": "CNY"}
func (s *Server) setSubscriptionRevenue(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Monthly  *float64 `json:"revenue_monthly"`
		Currency string   `json:"revenue_currency"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if in.Monthly != nil && (*in.Monthly < 0 || *in.Monthly > 1000000) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "月收入需在 0-1000000 之间"})
		return
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "CNY"
	}
	if !billingCurrencies[in.Currency] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "币种只支持 USD / CNY / EUR / HKD / JPY"})
		return
	}
	id := r.PathValue("id")
	tag, err := s.db.Exec(r.Context(), `UPDATE subscription_tokens SET revenue_monthly=$2, revenue_currency=$3 WHERE id=$1::uuid AND deleted_at IS NULL`, id, in.Monthly, in.Currency)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
		return
	}
	s.audit(r.Context(), r, "set_subscription_revenue", "subscription_token", id, map[string]any{"monthly": in.Monthly, "currency": in.Currency})
	writeJSON(w, http.StatusOK, map[string]any{"revenue_monthly": in.Monthly, "revenue_currency": in.Currency})
}

// ---------------------------------------------------------------- 利润报表

type moneyByCurrency map[string]float64

type nodeProfit struct {
	NodeID       string          `json:"node_id"`
	NodeName     string          `json:"node_name"`
	ServerID     string          `json:"server_id"`
	ServerName   string          `json:"server_name"`
	Revenue      moneyByCurrency `json:"revenue"`
	Cost         moneyByCurrency `json:"cost"`
	Profit       moneyByCurrency `json:"profit"`
	Subscribers  int             `json:"subscribers"`
	TrafficBytes int64           `json:"traffic_bytes"`
	CostShared   int             `json:"cost_shared_by"`
}

// GET /api/reports/profit
//
// 收入分摊：一个订阅横跨多个节点，按该订阅在各节点的实测流量占比分摊它的月收入；
// 没有流量数据的订阅在它的节点间平均分摊。主机月费在同主机的节点间平均分摊。
// 各币种分别汇总，不做汇率换算（换算用的汇率随时会过时，给出的总数看着精确其实不准）。
func (s *Server) profitReport(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。共享检测要扫全部生效订阅的 active_ips 与
	// subscription_live_connections，利润报表要跑多条聚合，都是重查询。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	ctx := r.Context()
	nodes := map[string]*nodeProfit{}
	order := []string{}
	rows, err := s.db.Query(ctx, `
SELECT n.id::text, n.name, s.id::text, s.name FROM hysteria_nodes n JOIN servers s ON s.id = n.server_id
WHERE n.deleted_at IS NULL AND s.deleted_at IS NULL ORDER BY s.name, n.name`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	nodesPerServer := map[string]int{}
	for rows.Next() {
		p := &nodeProfit{Revenue: moneyByCurrency{}, Cost: moneyByCurrency{}, Profit: moneyByCurrency{}}
		if err := rows.Scan(&p.NodeID, &p.NodeName, &p.ServerID, &p.ServerName); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		nodes[p.NodeID] = p
		order = append(order, p.NodeID)
		nodesPerServer[p.ServerID]++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	rows.Close()

	// 主机成本
	// 2026-11-06：只算未删除主机；没有在役节点的主机（节点刚删、准备迁移）成本记进「未分摊」，不再跳过 ——
	// 原来直接 continue，这台 VPS 的月费从合计里消失，利润被高估。
	unallocatedCost := moneyByCurrency{}
	crow, err := s.db.Query(ctx, `SELECT b.server_id::text, b.monthly_cost::float8, b.currency FROM server_billing b
JOIN servers sv ON sv.id = b.server_id AND sv.deleted_at IS NULL
WHERE b.monthly_cost IS NOT NULL`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	for crow.Next() {
		var sid, cur string
		var cost float64
		if err := crow.Scan(&sid, &cost, &cur); err != nil {
			crow.Close()
			s.databaseError(w, r, err)
			return
		}
		if nodesPerServer[sid] == 0 {
			unallocatedCost[cur] += cost
			continue
		}
		share := cost / float64(nodesPerServer[sid])
		for _, p := range nodes {
			if p.ServerID == sid {
				p.Cost[cur] += share
				p.CostShared = nodesPerServer[sid]
			}
		}
	}
	if err := crow.Err(); err != nil {
		crow.Close()
		s.databaseError(w, r, err)
		return
	}
	crow.Close()

	// 订阅收入按流量占比分摊
	type subRev struct {
		monthly  float64
		currency string
		usage    map[string]int64
		nodes    []string
	}
	subs := map[string]*subRev{}
	// 2026-11-06：收入分不到任何在役节点的订阅（开户待补、节点已全部移除）记进「未分摊」并列出 id。
	// 原来这笔收入不进合计，而「N 个订阅已填收入」照样算它 —— 计数与金额对不上。
	unallocatedRevenue := moneyByCurrency{}
	unallocatedSubs := []string{}
	srow, err := s.db.Query(ctx, `
SELECT t.id::text, t.revenue_monthly::float8, t.revenue_currency,
       -- node_accounts 是以节点 id 为键的对象：{"<node_id>": {"user_id": …}, …}
       CASE WHEN jsonb_typeof(t.node_accounts) = 'object'
            THEN COALESCE((SELECT array_agg(k) FROM jsonb_object_keys(t.node_accounts) k), '{}')
            ELSE '{}' END,
       COALESCE(jsonb_typeof(t.node_accounts) = 'object', false)
FROM subscription_tokens t
WHERE t.deleted_at IS NULL AND t.enabled AND t.revenue_monthly IS NOT NULL AND t.revenue_monthly > 0
  AND (t.expires_at IS NULL OR t.expires_at > now())`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	for srow.Next() {
		var id, cur string
		var monthly float64
		var nodeIDs []string
		var validNodeAccounts bool
		if err := srow.Scan(&id, &monthly, &cur, &nodeIDs, &validNodeAccounts); err != nil {
			srow.Close()
			s.databaseError(w, r, err)
			return
		}
		if !validNodeAccounts {
			srow.Close()
			s.databaseError(w, r, errors.New("subscription node_accounts must be a JSON object"))
			return
		}
		subs[id] = &subRev{monthly: monthly, currency: cur, usage: map[string]int64{}, nodes: nodeIDs}
	}
	if err := srow.Err(); err != nil {
		srow.Close()
		s.databaseError(w, r, err)
		return
	}
	srow.Close()
	// 2026-11-06：带上 quality_ok 与 collected_at，用来区分「这个节点真的没流量」与「这一轮没采到」。
	// 原来只取 measured_bytes > 0：没采到的节点（quality_ok=false，或压根没有行）被当成 0 流量，
	// 于是整笔收入按流量全分给了能测到的那台，其余节点收入 0 而成本照摊 —— 报表说它们「在亏钱」。
	urow, err := s.db.Query(ctx, `SELECT subscription_id::text, node_id::text, measured_bytes, quality_ok, collected_at FROM subscription_measured_usage`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// measuredTrustworthy 记录「这一对 (订阅,节点) 有可信计量」；缺失或质量不佳时不能按 0 参与分摊。
	measuredTrustworthy := map[string]bool{}
	freshAfter := time.Now().Add(-2 * sampleRetention / 3)
	for urow.Next() {
		var sid, nid string
		var measured int64
		var qualityOK bool
		var collectedAt time.Time
		if err := urow.Scan(&sid, &nid, &measured, &qualityOK, &collectedAt); err != nil {
			urow.Close()
			s.databaseError(w, r, err)
			return
		}
		if qualityOK && collectedAt.After(freshAfter) {
			measuredTrustworthy[sid+"|"+nid] = true
		}
		if sub, ok := subs[sid]; ok && measured > 0 {
			sub.usage[nid] += measured
		}
		if p, ok := nodes[nid]; ok {
			p.TrafficBytes += measured
		}
	}
	if err := urow.Err(); err != nil {
		urow.Close()
		s.databaseError(w, r, err)
		return
	}
	urow.Close()
	for subID, sub := range subs {
		// 只按订阅当前挂着的、仍在列表里的节点分摊：已从订阅移除或已删除的节点
		// 不该再分走收入，否则这部分收入在合计里凭空消失。
		current := make(map[string]bool, len(sub.nodes))
		for _, nid := range sub.nodes {
			if _, ok := nodes[nid]; ok {
				current[nid] = true
			}
		}
		var total int64
		for nid, b := range sub.usage {
			if _, ok := nodes[nid]; ok && current[nid] {
				total += b
			} else {
				delete(sub.usage, nid)
			}
		}
		// 只要有节点「没测到」，就不能按流量分摊（那会把 100% 给到能测到的那一台）。
		// 与 total==0 走同一分支：等额分摊，宁可粗略也不给假结论。
		unmeasured := false
		for nid := range current {
			if !measuredTrustworthy[subID+"|"+nid] {
				unmeasured = true
				break
			}
		}
		counted := map[string]bool{}
		if total > 0 && !unmeasured {
			for nid, b := range sub.usage {
				if p, ok := nodes[nid]; ok {
					p.Revenue[sub.currency] += sub.monthly * float64(b) / float64(total)
					counted[nid] = true
				}
			}
		} else if len(current) > 0 {
			for nid := range current {
				if p, ok := nodes[nid]; ok {
					p.Revenue[sub.currency] += sub.monthly / float64(len(current))
					counted[nid] = true
				}
			}
		}
		for nid := range counted {
			nodes[nid].Subscribers++
		}
		if len(counted) == 0 {
			unallocatedRevenue[sub.currency] += sub.monthly
			unallocatedSubs = append(unallocatedSubs, subID)
		}
	}

	out := make([]*nodeProfit, 0, len(order))
	totals := map[string]moneyByCurrency{"revenue": {}, "cost": {}, "profit": {}}
	for _, id := range order {
		p := nodes[id]
		for c, v := range p.Revenue {
			p.Revenue[c] = math.Round(v*100) / 100
			p.Profit[c] += v
		}
		for c, v := range p.Cost {
			p.Cost[c] = math.Round(v*100) / 100
			p.Profit[c] -= v
		}
		for c, v := range p.Profit {
			p.Profit[c] = math.Round(v*100) / 100
			totals["profit"][c] += v
		}
		for c, v := range p.Revenue {
			totals["revenue"][c] += v
		}
		for c, v := range p.Cost {
			totals["cost"][c] += v
		}
		out = append(out, p)
	}
	for c, v := range unallocatedCost {
		totals["cost"][c] += v
		totals["profit"][c] -= v
	}
	for c, v := range unallocatedRevenue {
		totals["revenue"][c] += v
		totals["profit"][c] += v
	}
	for _, m := range totals {
		for c, v := range m {
			m[c] = math.Round(v*100) / 100
		}
	}
	for _, m := range []moneyByCurrency{unallocatedCost, unallocatedRevenue} {
		for c, v := range m {
			m[c] = math.Round(v*100) / 100
		}
	}
	sort.Strings(unallocatedSubs)
	var withRevenue int
	// 2026-11-03：与上面计入收入的订阅同一口径（排除已到期）。原来这里不看到期，
	// 页面写着「N 个订阅已填收入」，月收入合计却只算了其中未到期的部分，两者对不上。
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE deleted_at IS NULL AND enabled AND revenue_monthly > 0
  AND (expires_at IS NULL OR expires_at > now())`).Scan(&withRevenue); err != nil {
		s.databaseError(w, r, err)
		return
	}
	var allSubs int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM subscription_tokens WHERE deleted_at IS NULL AND enabled`).Scan(&allSubs); err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 订阅清单（供页面逐条填写 / 修改月收入）
	type subRow struct {
		ID        string     `json:"id"`
		Label     string     `json:"label"`
		OrderID   string     `json:"order_id"`
		Monthly   *float64   `json:"revenue_monthly"`
		Currency  string     `json:"revenue_currency"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	subList := []subRow{}
	lrows, err := s.db.Query(ctx, `
SELECT id::text, COALESCE(label,''), COALESCE(order_id,''), revenue_monthly::float8, revenue_currency, expires_at
FROM subscription_tokens WHERE deleted_at IS NULL AND enabled ORDER BY created_at DESC LIMIT 500`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	for lrows.Next() {
		var sub subRow
		if err := lrows.Scan(&sub.ID, &sub.Label, &sub.OrderID, &sub.Monthly, &sub.Currency, &sub.ExpiresAt); err != nil {
			lrows.Close()
			s.databaseError(w, r, err)
			return
		}
		subList = append(subList, sub)
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		s.databaseError(w, r, err)
		return
	}
	lrows.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": out, "totals": totals, "subscriptions": subList,
		"unallocated_cost": unallocatedCost, "unallocated_revenue": unallocatedRevenue,
		"unallocated_subscription_ids": unallocatedSubs,
		"subscriptions_with_revenue":   withRevenue, "subscriptions_total": allSubs,
		"generated_at": time.Now(),
	})
}

// ---------------------------------------------------------------- 账号共享检测

type sharingVerdict struct {
	SubscriptionID string   `json:"subscription_id"`
	Label          string   `json:"label"`
	OrderID        string   `json:"order_id"`
	IPLimit        int      `json:"ip_limit"`
	LiveNetworks   int      `json:"live_networks"`
	DayNetworks    int      `json:"day_networks"`
	LiveIPs        []string `json:"live_ips"`
	Level          string   `json:"level"` // suspected / likely
	Reason         string   `json:"reason"`
}

// network16 把 IP 归到 /16（IPv6 取 /48）：同一家运营商同一地区的地址通常在同一段，
// 手机在 Wi-Fi 与 4G 之间切换一般只产生 2 个网段。
func network16(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ""
	}
	if v4 := p.To4(); v4 != nil {
		return net.IP(v4.Mask(net.CIDRMask(16, 32))).String()
	}
	return p.Mask(net.CIDRMask(48, 128)).String()
}

// detectSharing 判定规则：
//   - 同时在线（当前活动连接）来自 ≥3 个不同网段 → likely（很可能共享）
//   - 订阅的在线 IP 窗口内出现过 ≥4 个不同网段 → suspected（疑似共享）
//
// 2026-11-06：第二个判据原来写「24 小时内 ≥6 网段」，但数据源 subscription_tokens.active_ips 是
// subscriptionIPWindow（2 小时）的滑动账本 —— 每次拉订阅都会把超过 2 小时的条目物理删掉。
// 也就是说「24 小时」从来没有数据支撑，≥6 网段在 2 小时窗口里几乎不可达，持续共享反而测不出来。
// 现在窗口与数据源同源（subscriptionIPWindow），阈值按 2 小时重算为 4 个网段，文案也如实写 2 小时。
func (s *Server) detectSharing(ctx context.Context) ([]sharingVerdict, error) {
	type subInfo struct {
		label, order string
		limit        int
		live         map[string]bool
		liveNets     map[string]bool
		dayNets      map[string]bool
	}
	subs := map[string]*subInfo{}
	rows, err := s.db.Query(ctx, `
SELECT t.id::text, COALESCE(t.label,''), COALESCE(t.order_id,''), COALESCE(t.ip_limit,0), COALESCE(t.active_ips,'[]'::jsonb)
FROM subscription_tokens t WHERE t.deleted_at IS NULL AND t.enabled
  -- 2026-11-04：排除已到期（与设备上限巡检同一口径）。已到期的订阅节点侧早已拒绝认证，
  -- 原来照样参与判定，到期前 24 小时里的网段会让它出现在「疑似共享」名单里。
  AND (t.expires_at IS NULL OR t.expires_at > now())`)
	if err != nil {
		return nil, err
	}
	dayAgo := time.Now().Add(-subscriptionIPWindow)
	for rows.Next() {
		var id, label, order string
		var limit int
		var raw []byte
		if err := rows.Scan(&id, &label, &order, &limit, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		info := &subInfo{label: label, order: order, limit: limit, live: map[string]bool{}, liveNets: map[string]bool{}, dayNets: map[string]bool{}}
		var seen []struct {
			IP     string `json:"ip"`
			SeenAt string `json:"seen_at"`
		}
		if err := json.Unmarshal(raw, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		for _, e := range seen {
			if t, err := time.Parse(time.RFC3339Nano, e.SeenAt); err == nil && t.After(dayAgo) {
				if n := network16(e.IP); n != "" {
					info.dayNets[n] = true
				}
			}
		}
		subs[id] = info
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	lrows, err := s.db.Query(ctx, `SELECT subscription_id::text, ips FROM subscription_live_connections WHERE collected_at > now() - interval '10 minutes'`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var id string
		var raw []byte
		if err := lrows.Scan(&id, &raw); err != nil {
			lrows.Close()
			return nil, err
		}
		info, ok := subs[id]
		if !ok {
			continue
		}
		var ips []struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(raw, &ips); err != nil {
			lrows.Close()
			return nil, err
		}
		for _, e := range ips {
			if n := network16(e.IP); n != "" {
				info.live[e.IP] = true
				info.liveNets[n] = true
				info.dayNets[n] = true
			}
		}
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		return nil, err
	}
	lrows.Close()

	var out []sharingVerdict
	for id, info := range subs {
		v := sharingVerdict{SubscriptionID: id, Label: info.label, OrderID: info.order, IPLimit: info.limit, LiveNetworks: len(info.liveNets), DayNetworks: len(info.dayNets)}
		for ip := range info.live {
			v.LiveIPs = append(v.LiveIPs, ip)
		}
		sort.Strings(v.LiveIPs)
		switch {
		case len(info.liveNets) >= 3:
			v.Level, v.Reason = "likely", "同一时间从 "+strconv.Itoa(len(info.liveNets))+" 个不同网段在线"
		case len(info.dayNets) >= 4:
			v.Level, v.Reason = "suspected", "最近 "+strconv.Itoa(int(subscriptionIPWindow.Hours()))+" 小时内出现在 "+strconv.Itoa(len(info.dayNets))+" 个不同网段"
		default:
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == "likely"
		}
		return out[i].LiveNetworks+out[i].DayNetworks > out[j].LiveNetworks+out[j].DayNetworks
	})
	return out, nil
}

// GET /api/subscriptions/sharing
func (s *Server) listSharing(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。共享检测要扫全部生效订阅的 active_ips 与
	// subscription_live_connections，利润报表要跑多条聚合，都是重查询。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	out, err := s.detectSharing(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if out == nil {
		out = []sharingVerdict{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
