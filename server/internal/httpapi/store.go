package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// store.go is the commercial layer of the panel: many independent subscriptions,
// each with its own expiry, device (IP) limit and order provenance, plus the
// machine-to-machine API a shop uses to provision them.
//
// Design constraints that shaped this file:
//
//  1. The shop is an *untrusted* caller. It authenticates with a shared secret
//     compared in constant time, and it may only ever touch subscriptions it
//     created (matched by order_id). It cannot list other orders, cannot see
//     the fleet inventory, and cannot reach any /api/ admin route.
//  2. A subscription's expiry must be enforced at *read* time, not only by a
//     background sweeper. A sweeper can be late; the token endpoint is hit
//     continuously by clients, so the check that matters lives there.
//  3. IP limiting must never lock out a legitimate client network. Clients may
//     egress from several addresses (dual-stack, CGNAT pools, mobile handover),
//     so the limit bounds *distinct addresses within a moving window*, and the
//     window slides: addresses are forgotten once they go quiet. A one-shot
//     static set would permanently consume the quota on the first few pulls.
//  4. Nothing here writes to a Hysteria node. Provisioning only mints a
//     subscription; the node credentials come from the same live portal read
//     the operator-facing subscription already uses.

const (
	// storeSecretKey is the system_settings row holding the shop's shared secret.
	storeSecretKey = "store_api_key"

	// subscriptionIPWindow is how long a client address counts against the
	// device limit after its last pull. Long enough that a normal client's
	// periodic refresh keeps its slot, short enough that a device which is gone
	// frees the slot the same day.
	subscriptionIPWindow = 2 * time.Hour

	// subscriptionExpiryGrace is how long an expired subscription is retained
	// after lapsing before auto_delete physically removes it.
	//
	// It is deliberately ZERO: the product rule is that an expired subscription
	// is reclaimed the moment it lapses — the delivery link stops working and no
	// node is served, with no retention window. Every read compares against
	// `expires_at + subscriptionExpiryGrace`, so with a zero grace the reclamation
	// happens on the very first read past the deadline, and `writeSubscriptionRefusal`
	// never has a chance to answer 403 "expired" for a row that is already gone.
	//
	// This trades a friendlier "已到期, 请续费" page for an unambiguous one: an
	// expired link is indistinguishable from a token that never existed (404),
	// which is what the operator asked for. Do NOT quietly restore a retention
	// window here — the customer-facing consequence is described in tests/README.md
	// under 「到期立即回收」.
	subscriptionExpiryGrace = 0 * time.Second
)

// ---------------------------------------------------------------------------
// Public shape
// ---------------------------------------------------------------------------

// subscriptionRecord is one row of the commercial subscription table.
type subscriptionRecord struct {
	ID             string     `json:"id"`
	Token          string     `json:"token"`
	Label          string     `json:"label"`
	Enabled        bool       `json:"enabled"`
	ExpiresAt      *time.Time `json:"expires_at"`
	IPLimit        int        `json:"ip_limit"`
	AutoDelete     bool       `json:"auto_delete"`
	OrderID        string     `json:"order_id"`
	UserEmail      string     `json:"user_email"`
	Note           string     `json:"note"`
	ActiveIPCount  int        `json:"active_ip_count"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAccessedAt *time.Time `json:"last_accessed_at"`
	AccessCount    int64      `json:"access_count"`
	DeletedAt      *time.Time `json:"deleted_at"`

	// rawIPs is the stored active_ips JSON exactly as it came out of the row.
	// enforceSubscriptionAccess needs the authoritative list (not the pruned
	// view ActiveIPCount was computed from) so one UPDATE writes the whole set.
	// Unexported so it never reaches a JSON response.
	rawIPs []byte

	// nodeAccounts maps node id -> the credentials this subscription owns ON
	// that node (see migration 010). Empty for legacy rows minted before the
	// shop started provisioning per-buyer accounts; those still fall back to
	// the node's shared portal password on read.
	nodeAccounts map[string]subscriptionNodeAccount

	// CategoryID 限定这条订阅只包含某个分类下的节点（AI 专用 / 跨境专用…）。
	// 为空 = 不按分类圈定 = 全部分类，与加分类功能之前的行为完全一致。
	//
	// 用 *string 而不是 string：空串与"指向一个不存在的分类"必须区分开 ——
	// 前者是"没选分类"，后者是"数据有问题"，两者的处理方式不同。
	CategoryID *string

	// NodeIDs 直接圈定的节点（手工开通时多选）。非空时优先于 CategoryID。
	NodeIDs []string
}

// subscriptionNodeAccount is one buyer's own account on one node.
//
// The password lives here rather than being re-read from the node because the
// delivery endpoint must keep serving a working config even when the node-side
// portal is briefly unreachable — and because the whole point of the feature is
// that this password is NOT the node's shared one.
type subscriptionNodeAccount struct {
	UserID    string `json:"user_id"`
	Password  string `json:"password"`
	CreatedAt int64  `json:"created_at"`
}

// expired reports whether the subscription has lapsed at the given time.
func (s *subscriptionRecord) expired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// daysLeft is the remaining validity in whole days, floored at zero. Used by the
// delivery page countdown; a nil expiry means perpetual.
func (s *subscriptionRecord) daysLeft(now time.Time) int {
	if s.ExpiresAt == nil {
		return -1
	}
	remaining := s.ExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	// 2026-11-12：先整除、有余数再加一（向上取整）。原来写成 (remaining + 24h - 1) / 24h：
	// 到期时间超过约 292 年时 Sub 饱和为最大值，再加就回绕成负数，一条有效订阅被显示成「已到期，请续费」。
	days := remaining / (24 * time.Hour)
	if remaining%(24*time.Hour) != 0 {
		days++
	}
	return int(days)
}

// subscriptionIP tracks one client address that pulled the subscription.
type subscriptionIP struct {
	IP     string    `json:"ip"`
	SeenAt time.Time `json:"seen_at"`
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

const subscriptionColumns = `id, token, label, enabled, expires_at, ip_limit, auto_delete,
	COALESCE(order_id,''), COALESCE(user_email,''), note, active_ips, created_at,
	last_accessed_at, access_count, deleted_at, node_accounts, category_id,
	COALESCE(node_ids::text[], '{}')`

func scanSubscription(row pgx.Row) (*subscriptionRecord, error) {
	var rec subscriptionRecord
	var rawAccounts []byte
	if err := row.Scan(&rec.ID, &rec.Token, &rec.Label, &rec.Enabled, &rec.ExpiresAt, &rec.IPLimit,
		&rec.AutoDelete, &rec.OrderID, &rec.UserEmail, &rec.Note, &rec.rawIPs, &rec.CreatedAt,
		&rec.LastAccessedAt, &rec.AccessCount, &rec.DeletedAt, &rawAccounts, &rec.CategoryID, &rec.NodeIDs); err != nil {
		return nil, err
	}
	rec.ActiveIPCount = len(parseActiveIPs(rec.rawIPs, time.Now()))
	rec.nodeAccounts = parseNodeAccounts(rawAccounts)
	return &rec, nil
}

// parseNodeAccounts decodes the node_accounts JSON. A malformed or empty blob
// yields a nil map rather than an error: it only ever means "this subscription
// owns no node-side account", which every caller already handles — and failing
// the whole load would take the delivery page down over a column that is
// allowed to be absent.
func parseNodeAccounts(raw []byte) map[string]subscriptionNodeAccount {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]subscriptionNodeAccount
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// isPrivateOrLoopbackIP 判断一个地址是否**绝不可能**是买家设备的真实出口。
//
// ⚠️ 为什么必须有这道过滤（2026-10-04 实测）：
//
//	「设备数」= active_ips 里 24 小时窗口内的不同地址数，它直接参与限流判定。
//	当请求**不经过最外层 Caddy 覆盖**时（容器内直连、docker 网桥转发、
//	宿主机探测等），X-Forwarded-For 拿不到真实访问者，于是 requestClientIP
//	回落到 RemoteAddr —— 那是 **docker 网桥地址（如 172.19.0.1）**。
//	实测：从 127.0.0.1:8081 拉一次订阅，就多出一个「设备」172.19.0.1。
//
//	后果是**设备数虚高**：买家买了 5 台，可能被内部地址占掉 1-2 个名额，
//	轻则显示难看，重则把一个正常用户误判成超限而拒绝服务。
//
// 判据是**网络语义**而不是"看着像内网"：
//   - 回环（127/8、::1）
//   - 链路本地（169.254/16、fe80::/10）
//   - RFC1918 私网（10/8、172.16/12、192.168/16）—— docker 网桥正落在 172.17-172.31
//   - 唯一本地地址（fc00::/7）
//   - 未指定地址（0.0.0.0、::）
//
// 这些地址**在公网上不可路由**，真实客户端不可能以它们为出口地址。
//
// ⚠️ 注意：运营商级 NAT（CGNAT，100.64/10）**不在此列** ——
// 它是公网侧真实存在的地址段，有些移动网络用户的确从那里出来，
// 过滤掉会把真人当空气。宁可不滤，也不能误伤真实用户。
func isPrivateOrLoopbackIP(raw string) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		// 解析不出来的一律**保留**：宁可多算一个设备，也不要因为
		// 一个格式怪异的地址把真实用户从计数里抹掉。
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// parseActiveIPs decodes the active_ips JSON and drops entries that have fallen
// outside the sliding window, so callers always see a live view of the quota.
//
// 同时**剔除内部地址**（见 isPrivateOrLoopbackIP）—— 历史数据里已经混进了
// 网桥地址，不在这里滤掉的话，面板会一直显示偏高的设备数。
// 过滤只作用于「计数与展示」；记录侧也一起修了（见 recordSubscriptionIP），
// 两层都做是为了让**已有脏数据**立刻不再影响判断。
func parseActiveIPs(raw []byte, now time.Time) []subscriptionIP {
	if len(raw) == 0 {
		return nil
	}
	var list []subscriptionIP
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	cutoff := now.Add(-subscriptionIPWindow)
	live := list[:0]
	for _, entry := range list {
		if entry.IP == "" || !entry.SeenAt.After(cutoff) {
			continue
		}
		if isPrivateOrLoopbackIP(entry.IP) {
			continue
		}
		live = append(live, entry)
	}
	return live
}

// subscriptionUsageAgg 是订阅级的用量聚合（跨该订阅名下全部节点）。
type subscriptionUsageAgg struct {
	// UsedBytes 为 nil 表示**从未采到过** —— 与"采到了 0"不同。
	// 前端据此显示「未采集」而不是「0 B」。
	//
	// UsedBytes 只承载真实计量。LegacyBytes 是不可验证的旧鉴权值，
	// 单独保留用于解释历史，不再与真实计数器相加（否则新版门户会重复计费）。
	//
	// ⚠️ UsedBytes 必须保持 `*int64`（指针）：
	// 值类型无法表达"从未采到过"，前端会把 null 当成 0 显示 ——
	// 那是本项目反复要避开的"把未知说成 0"。
	UsedBytes *int64
	// LegacyBytes 是旧鉴权记账估算，仅供展示来源，不计入 UsedBytes。
	LegacyBytes int64
	// MeasuredBytes 是真实计数器累计（只从计量基线起算）。
	MeasuredBytes int64
	LimitBytes    *int64
	// OK 表示最近一轮采集是否全部成功。
	OK bool
	// Reason 是采集失败的原因（空表示成功）。
	Reason          string
	SampledAt       *time.Time
	StartedAt       *time.Time
	StartedAtLatest *time.Time
	ExpectedNodes   int
	MeasuredNodes   int
	CurrentHealthy  bool
	// HysteriaOnlineDevices is the sum of the core's /online connection-instance
	// counters. It deliberately excludes Reality and is nil when any current
	// sample needed for the count is unavailable.
	HysteriaOnlineDevices *int64
	OnlineCountNodes      int
	HistoricalGaps        int
}

// mergeSubscriptionLimit adds a node limit to the subscription aggregate. A nil
// accumulator means no sample has been seen yet; zero is a real "unlimited"
// value and must not be used as the initialization sentinel.
func mergeSubscriptionLimit(current *int64, limit int64) *int64 {
	if current == nil {
		value := limit
		return &value
	}
	if *current == 0 || limit == 0 {
		unlimited := int64(0)
		return &unlimited
	}
	total := *current + limit
	return &total
}

// loadSubscriptionUsage 批量读订阅用量并聚合成订阅级。
//
// 聚合口径说明：
//   - used_bytes 求和 —— 一条订阅在多个节点上各有账号，总用量就是它们之和。
//   - limit_bytes 求和，但只要有一个节点是"不限"（0），整体就不限 ——
//     否则会出现"总上限比实际可用量还小"的荒谬结果。
//   - ok 要求**全部**节点都采成功。任何一个失败都算不完整，
//     并把原因带出去 —— 把"部分节点没采到"说成"用量就是这样"是谎报。
func (s *Server) loadSubscriptionUsage(ctx context.Context, records []subscriptionRecord) map[string]subscriptionUsageAgg {
	out := make(map[string]subscriptionUsageAgg, len(records))
	if len(records) == 0 {
		return out
	}
	ids := make([]string, 0, len(records))
	expected := make(map[string]map[string]subscriptionNodeAccount, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
		expected[rec.ID] = rec.nodeAccounts
		agg := subscriptionUsageAgg{OK: true, CurrentHealthy: true}
		for _, acc := range rec.nodeAccounts {
			if acc.UserID != "" {
				agg.ExpectedNodes++
			}
		}
		if agg.ExpectedNodes == 0 {
			agg.CurrentHealthy = false
			agg.OK = false
			agg.Reason = "没有可计量的节点账号"
		}
		out[rec.ID] = agg
	}
	rows, err := s.db.Query(ctx, `SELECT subscription_id,node_id,user_id,measured_bytes,started_at,quality_ok,error,collected_at,sources,gaps,current_ok,online_devices
 FROM subscription_measured_usage WHERE subscription_id=ANY($1::uuid[])`, ids)
	if err != nil {
		s.logger.Warn("subscription-center: 读取真实计量失败", "error", err)
		for id, agg := range out {
			agg.OK = false
			agg.CurrentHealthy = false
			agg.Reason = "真实计量不可用"
			out[id] = agg
		}
		return out
	}
	// 显式 Close（不是 defer）：下面还要在同一连接上跑第二轮查询，
	// rows 未释放时 pgx 的连接池不会把它借出去。
	for rows.Next() {
		var subID, nodeID, userID, reason string
		var used *int64
		var started *time.Time
		var quality bool
		var sampled time.Time
		var sources, gaps json.RawMessage
		var currentOK *bool
		var online *int64
		if err := rows.Scan(&subID, &nodeID, &userID, &used, &started, &quality, &reason, &sampled, &sources, &gaps, &currentOK, &online); err != nil {
			rows.Close()
			s.logger.Warn("subscription-center: measured usage row scan failed", "error", err)
			for id, agg := range out {
				agg.OK = false
				agg.CurrentHealthy = false
				agg.Reason = "真实计量读取不完整"
				out[id] = agg
			}
			return out
		}
		acc, exists := expected[subID][nodeID]
		if !exists || acc.UserID == "" || acc.UserID != userID {
			continue
		}
		agg := out[subID]
		if used != nil && started != nil && *used >= 0 {
			// 只累加 measured；legacy 在下面单独一轮查询里累加，
			// 两段在循环结束后统一合并成一个 UsedBytes。
			//
			// ⚠️ 这里不做合并是有意的：合并要在**所有节点都累完之后**
			// 做一次，否则中间状态会被后续节点反复覆盖，
			// 且溢出判断要在两段都到齐之后才有意义。
			if *used > math.MaxInt64-agg.MeasuredBytes {
				agg.OK = false
				agg.Reason = "真实计量总量超出范围"
			} else {
				agg.MeasuredBytes += *used
				agg.MeasuredNodes++
			}
			if agg.StartedAt == nil || started.Before(*agg.StartedAt) {
				t := *started
				agg.StartedAt = &t
			}
			if agg.StartedAtLatest == nil || started.After(*agg.StartedAtLatest) {
				t := *started
				agg.StartedAtLatest = &t
			}
		} else {
			quality = false
			reason = "部分节点尚未开始真实计量"
		}
		if time.Since(sampled) > 90*time.Second {
			quality = false
			reason = "部分节点计量数据陈旧"
		}
		current, gapCount := measurementHealth(sources, gaps, sampled, used != nil && started != nil)
		if currentOK != nil {
			current = current && *currentOK
		} else if !quality && gapCount == 0 {
			current = false
		}
		if current && online != nil && *online >= 0 {
			if agg.HysteriaOnlineDevices == nil {
				value := int64(0)
				agg.HysteriaOnlineDevices = &value
			}
			if *online <= math.MaxInt64-*agg.HysteriaOnlineDevices {
				*agg.HysteriaOnlineDevices += *online
				agg.OnlineCountNodes++
			}
		}
		agg.CurrentHealthy = agg.CurrentHealthy && current
		agg.HistoricalGaps += gapCount
		if !quality && current && gapCount > 0 {
			reason = "当前采集正常，累计存在历史缺口"
		}
		if !quality {
			agg.OK = false
			if agg.Reason == "" {
				agg.Reason = reason
			}
		}
		// 最旧节点决定整份数据的新鲜度，而非最新节点掩盖旧值。
		if agg.SampledAt == nil || sampled.Before(*agg.SampledAt) {
			t := sampled
			agg.SampledAt = &t
		}
		out[subID] = agg
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("subscription-center: measured usage rows ended incompletely", "error", err)
		for id, agg := range out {
			agg.OK = false
			agg.CurrentHealthy = false
			agg.Reason = "真实计量读取不完整"
			out[id] = agg
		}
		return out
	}

	// 旧口径（portal 鉴权记账）单独一轮。
	//
	// 为什么放在 measured 之后而不是一条 SQL JOIN：
	//   两者的 user_id 可能对不上（节点侧换过账号、订阅改过 node_accounts），
	//   分开查才能各自按"node_accounts 里的账号仍然匹配"来过滤 ——
	//   不匹配的行会被静默算进总量，显示出一个虚高的数字。
	legacy, limits, err := s.loadSubscriptionLegacyUsage(ctx, records, expected)
	if err != nil {
		s.logger.Warn("subscription-center: 读取旧口径记账失败", "error", err)
	}

	for id, agg := range out {
		agg.LimitBytes = limits[id]
		if err != nil {
			agg.OK = false
			agg.Reason = "配额/历史信息读取失败"
		}
		if v, ok := legacy[id]; ok {
			agg.LegacyBytes = v
		}
		// 对外总量只来自真实计数器。旧鉴权值单列保留，绝不再次相加。
		total := agg.MeasuredBytes
		if total < 0 {
			agg.OK = false
			agg.Reason = "用量总量超出可表示范围"
		} else if agg.MeasuredNodes > 0 {
			merged := total
			agg.UsedBytes = &merged
		}

		if agg.MeasuredNodes < agg.ExpectedNodes {
			agg.CurrentHealthy = false
			agg.OK = false
			if agg.Reason == "" {
				agg.Reason = "部分节点尚未开始真实计量"
			}
		}
		// 2026-11-03：字段注释写的是「任一所需样本不可用时为 nil」，实现却把能读到的
		// 节点加起来照样返回：4 台节点只有 1 台当前可用时，界面显示的「Hy2核心：N 个连接实例」
		// 只是那 1 台的数，看着像总数。凑不齐就给 nil（界面显示「未知」）。
		if agg.OnlineCountNodes < agg.ExpectedNodes {
			agg.HysteriaOnlineDevices = nil
		}
		if rows.Err() != nil {
			agg.OK = false
			agg.Reason = "真实计量读取不完整"
		}
		out[id] = agg
	}
	return out
}

// loadSubscriptionLegacyUsage 读旧口径记账值（portal 的 traffic_used_bytes）。
//
// 它只在该客户端**重新认证**时前进，但计量上线前的历史流量只有它记得
// —— 真实计量刻意不做初值（"不声称进程内历史属于新窗口"），
// 所以那一段历史只能从这里补回来。
//
// 返回缺失订阅的条目即为"没有旧数据"，不是 0：前端靠这个区分
// "这条订阅从来没有鉴权记账"和"记账为 0"。
func (s *Server) loadSubscriptionLegacyUsage(ctx context.Context, records []subscriptionRecord,
	expected map[string]map[string]subscriptionNodeAccount) (map[string]int64, map[string]*int64, error) {

	out := make(map[string]int64, len(records))
	limits := make(map[string]*int64, len(records))
	// limitNodes 记录每条订阅有几台节点贡献了上限：凑不齐全部开户节点时不给总上限。
	limitNodes := make(map[string]int, len(records))
	if len(records) == 0 {
		return out, limits, nil
	}
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
	}
	// 2026-11-06：不再按 ok 过滤整行。采集器现在会把「这次没采到」的行标成 ok=false（只改 ok，不动 used_bytes）：
	// 历史用量是累计值，照常计入；只有上限要求本轮采到才算（见下方 limitNodes）。
	rows, err := s.db.Query(ctx, `SELECT subscription_id,node_id,user_id,used_bytes,limit_bytes,ok
 FROM subscription_usage WHERE subscription_id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var subID, nodeID, userID string
		var used, limit int64
		var ok bool
		if err := rows.Scan(&subID, &nodeID, &userID, &used, &limit, &ok); err != nil {
			rows.Close()
			return nil, nil, err
		}
		acc, exists := expected[subID][nodeID]
		if !exists || acc.UserID == "" || acc.UserID != userID {
			continue
		}
		if ok && limit >= 0 {
			limits[subID] = mergeSubscriptionLimit(limits[subID], limit)
			limitNodes[subID]++
		}
		if used > 0 {
			if used > math.MaxInt64-out[subID] {
				continue
			}
			out[subID] += used
		}
	}
	// 2026-11-03：查询带 `AND ok`，最近一次采集失败的节点不在结果里，上限就少算一台
	// （4 台各 100 GB、1 台没采到 ⇒ 显示 300 GB），且没有任何标记说明不完整。
	// 凑不齐全部开户节点就不给总上限（nil ⇒ 界面显示未知）。「不限」不受影响：
	// 任一节点不限，整体就不限，与缺不缺其它节点无关。
	for subID, limit := range limits {
		if limit != nil && *limit == 0 {
			continue
		}
		want := 0
		for _, acc := range expected[subID] {
			if acc.UserID != "" {
				want++
			}
		}
		if limitNodes[subID] < want {
			delete(limits, subID)
		}
	}
	return out, limits, rows.Err()
}

// loadSubscriptionOnlineIPs 批量读订阅在各节点上的真实在线 IP。
//
// 与 subscription_tokens.active_ips 分工不同：
//
//	本函数 —— 节点侧鉴权时记录的 ip_tracker，"谁最近鉴权"，180 秒窗
//	active_ips —— 面板侧记的"谁拉取过订阅链接"，可配置滑窗
//
// 两者都要，但都不是实时连接清单。Hy2 实际连接实例数另读核心 /online。
//
// 按节点分组返回，前端能直接显示"这个 IP 在哪台机器上"。
func (s *Server) loadSubscriptionOnlineIPs(ctx context.Context, records []subscriptionRecord) (map[string][]map[string]any, error) {

	out := make(map[string][]map[string]any, len(records))
	if len(records) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(records))
	expected := make(map[string]map[string]subscriptionNodeAccount, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
		expected[rec.ID] = rec.nodeAccounts
	}
	rows, err := s.db.Query(ctx, `
SELECT o.subscription_id, o.ip, o.last_seen, n.id, n.name
  FROM subscription_online_ips o
  LEFT JOIN hysteria_nodes n ON n.id = o.node_id
 WHERE o.subscription_id = ANY($1::uuid[])
 AND o.last_seen > extract(epoch from now()) - 180
 AND o.collected_at > now() - interval '90 seconds'
 AND n.deleted_at IS NULL
 ORDER BY o.last_seen DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var subID, ip string
		var lastSeen int64
		var nodeID, nodeName *string
		if err := rows.Scan(&subID, &ip, &lastSeen, &nodeID, &nodeName); err != nil {
			rows.Close()
			return nil, err
		}
		// 节点已被删除或节点账号已改 → 这条在线记录不再有意义。
		if nodeID == nil {
			continue
		}
		if acc, ok := expected[subID][*nodeID]; !ok || acc.UserID == "" {
			continue
		}
		item := map[string]any{
			"ip":        ip,
			"last_seen": lastSeen,
			"node_id":   *nodeID,
		}
		if nodeName != nil {
			item["node_name"] = *nodeName
		}
		out[subID] = append(out[subID], item)
	}
	return out, rows.Err()
}

// loadSubscriptionByToken 用 token 查一条订阅。
func (s *Server) loadSubscriptionByToken(ctx context.Context, token string) (*subscriptionRecord, error) {
	row := s.db.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscription_tokens WHERE token=$1 AND deleted_at IS NULL`, token)
	return scanSubscription(row)
}

func (s *Server) loadSubscriptionByID(ctx context.Context, id string) (*subscriptionRecord, error) {
	row := s.db.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscription_tokens WHERE id=$1::uuid AND deleted_at IS NULL`, id)
	return scanSubscription(row)
}

func (s *Server) loadSubscriptionByOrder(ctx context.Context, orderID string) (*subscriptionRecord, error) {
	row := s.db.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscription_tokens WHERE order_id=$1 AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`, orderID)
	return scanSubscription(row)
}

// listSubscriptions returns every live subscription, newest first. The default
// operator subscription created by the panel itself has an empty order_id.
func (s *Server) listSubscriptions(ctx context.Context) ([]subscriptionRecord, error) {
	rows, err := s.db.Query(ctx, `SELECT `+subscriptionColumns+` FROM subscription_tokens WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscriptionRecord
	now := time.Now()
	for rows.Next() {
		var rec subscriptionRecord
		// ⚠️ 这里的 Scan 目标必须与 subscriptionColumns 逐列对应，且一个不能少。
		//
		// 010 给 subscription_tokens 加了 node_accounts 之后，单行版
		// scanSubscription 跟着补了 &rawAccounts，但列表版漏了这一处 ——
		// 于是 SELECT 出 16 列、Scan 只接 15 个，pgx 直接报
		//   number of field descriptions must equal number of destinations, got 16 and 15
		// 整个「订阅中心」列表 500，而单条订阅的接口照常 200，
		// 看上去像"订阅中心坏了但订阅功能正常"。
		//
		// 加列时请同时改这两处：scanSubscription 与 listSubscriptions。
		var rawAccounts []byte
		// ⚠️ Scan 目标必须与 subscriptionColumns 逐列对应，一列都不能少。
		// 历史上漏过一次（列表版没跟上单行版），pgx 直接报
		// "number of field descriptions must equal number of destinations"，
		// 整个订阅中心列表 500、而单条订阅接口照常 200。
		if err := rows.Scan(&rec.ID, &rec.Token, &rec.Label, &rec.Enabled, &rec.ExpiresAt, &rec.IPLimit,
			&rec.AutoDelete, &rec.OrderID, &rec.UserEmail, &rec.Note, &rec.rawIPs, &rec.CreatedAt,
			&rec.LastAccessedAt, &rec.AccessCount, &rec.DeletedAt, &rawAccounts, &rec.CategoryID, &rec.NodeIDs); err != nil {
			return nil, err
		}
		rec.ActiveIPCount = len(parseActiveIPs(rec.rawIPs, now))
		rec.nodeAccounts = parseNodeAccounts(rawAccounts)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Enforcement
// ---------------------------------------------------------------------------

// enforceSubscriptionAccess decides whether a pull may proceed, and records the
// caller's address when it may. It returns a machine-readable refusal reason so
// every caller answers identically.
//
// Order matters: expiry is checked before the IP quota so an expired customer is
// told "已到期" rather than "设备超限", which is the more actionable message.
//
// 2026-11-12：到期也先于停用。到期清扫在收回节点账号后会把这条订阅置为 enabled=FALSE：
// 原来先判停用，保留型（auto_delete=false）订阅到期约两分钟后，拒绝原因就从「已到期，请续费」
// 变成「已被暂停，请联系客服」，而交付页与订阅中心对同一条订阅显示的都是「已到期」。
// 未到期的停用（退款、拒付、人工暂停）仍答 disabled。
//
// The IP ledger is updated by a single statement that takes a row lock
// (SELECT … FOR UPDATE inside the CTE). This is not an optimisation — an earlier
// read-decide-write version lost updates under concurrency: two pulls of the same
// subscription both read the old `active_ips`, both concluded "a slot is free",
// and the second UPDATE overwrote the first, so an address was allowed through
// and silently not recorded. With ip_limit=1 eight simultaneous requests from
// eight addresses produced two 200s. Doing the decision and the write in one
// statement makes concurrent pulls of the same subscription serialise.
func (s *Server) enforceSubscriptionAccess(ctx context.Context, rec *subscriptionRecord, ip string) (ok bool, reason string) {
	now := time.Now()
	if rec.expired(now) {
		return false, "expired"
	}
	if !rec.Enabled {
		return false, "disabled"
	}
	// A limited feed cannot be served without an address to charge. Keep the
	// expired/disabled checks above so their existing refusal takes precedence.
	if strings.TrimSpace(ip) == "" {
		if rec.IPLimit > 0 {
			return false, "ip_unknown"
		}
		return true, ""
	}
	// ⚠️ 2026-10-04：内部地址**既不参与限流、也不写进账本**。
	//
	// 这种地址出现在这里是「链路没带上真实访问者」的信号（容器内直连、
	// docker 网桥转发、宿主机探测），不是买家多了一台设备。
	// 实测：从 127.0.0.1:8081 拉一次订阅就会多出「设备 172.19.0.1」。
	//
	// 为什么是"放过且不记录"而不是"拒绝"：
	//   拒绝会让**合法但被代理隐藏了来源**的请求无法拉取订阅（比如
	//   运维自查、健康探测），那比多算一个设备更糟；
	//   而放过的代价仅仅是"这一次不计数" —— 真实买家从公网来，
	//   本来就不会命中这个分支。
	if isPrivateOrLoopbackIP(ip) {
		return true, ""
	}

	// The ledger is rewritten from the row that was just locked, falling back to
	// the caller's snapshot only for the unlimited case, where it is never read.
	verdict, err := s.recordSubscriptionHit(ctx, rec, ip, now)
	if err != nil {
		// Fail open on infrastructure trouble, matching the old behaviour of
		// logging and letting the pull through: a database hiccup must not lock
		// paying customers out of a subscription they can otherwise use.
		s.logger.Warn("subscription: record ip hit failed", "error", err)
		return true, ""
	}
	if verdict == "ip_limit" {
		return false, "ip_limit"
	}
	return true, ""
}

// recordSubscriptionHit atomically decides the quota and writes the ledger.
//
// The window filter and the "have I seen this address" test both happen in SQL
// against a locked row, so the answer cannot go stale between deciding and
// writing. Returns the refusal reason, or "" when the pull is allowed and the
// address has been recorded.
func (s *Server) recordSubscriptionHit(ctx context.Context, rec *subscriptionRecord, ip string, now time.Time) (string, error) {
	cutoff := now.Add(-subscriptionIPWindow)
	ip = strings.TrimSpace(ip)
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	}

	// 使用行锁内的权威 ip_limit、enabled、deleted_at 判断，避免并发改限额或停用后沿用旧快照。
	const q = `
WITH target AS (
    SELECT id, ip_limit, active_ips
    FROM subscription_tokens
    WHERE id = $1::uuid AND deleted_at IS NULL AND enabled = TRUE
    FOR UPDATE
),
live AS (
    SELECT COALESCE(jsonb_agg(e), '[]'::jsonb) AS entries
    FROM target t,
         LATERAL jsonb_array_elements(t.active_ips) e
    WHERE (e->>'seen_at')::timestamptz >= $2::timestamptz
      AND e->>'ip' <> $3
),
known AS (
    SELECT EXISTS (
        SELECT 1 FROM target t, LATERAL jsonb_array_elements(t.active_ips) e
        WHERE (e->>'seen_at')::timestamptz >= $2::timestamptz
          AND e->>'ip' = $3
    ) AS seen
),
decided AS (
    SELECT t.id,
           t.ip_limit,
           (SELECT seen FROM known) AS seen,
           jsonb_array_length((SELECT entries FROM live)) AS live_count,
           (SELECT entries FROM live) AS live_entries
    FROM target t
)
UPDATE subscription_tokens st
SET active_ips = (
        jsonb_build_array(jsonb_build_object('ip', $3::text, 'seen_at', $4::timestamptz))
        || (SELECT live_entries FROM decided)
    )
FROM decided d
WHERE st.id = d.id
  AND (d.ip_limit = 0 OR d.seen OR d.live_count < d.ip_limit)
RETURNING 1`

	var dummy int
	err := s.db.QueryRow(ctx, q, rec.ID, cutoff, ip, now).Scan(&dummy)
	if err == nil {
		return "", nil
	}
	// No row updated means the WHERE guard refused it: the quota is exhausted and
	// this is a new address.
	if errors.Is(err, pgx.ErrNoRows) {
		return "ip_limit", nil
	}
	return "", err
}

// ipLimitExceeded answers a refused pull. It uses a distinct status and a short
// human message so a client's log shows why, while still not confirming whether
// the token exists to an unauthenticated prober.
func writeSubscriptionRefusal(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	switch reason {
	case "expired":
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"subscription expired","message":"订阅已到期，请续费后使用"}`))
	case "ip_limit":
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"ip limit exceeded","message":"同时在线的网络数已达上限，同一 Wi-Fi 下的设备不受影响"}`))
	case "ip_unknown":
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"client ip unavailable","message":"无法确认客户端 IP，请稍后重试"}`))
	case "disabled":
		// Revoked, not missing. This used to fall into the default branch and
		// answer 404 "subscription not found" — indistinguishable from a token
		// that never existed or one purged after its grace window. A customer
		// whose subscription was stopped (refund, chargeback, manual hold) would
		// be told their link is invalid and go buy a new one, when the operator
		// can simply renew and revive the same token.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"subscription disabled","message":"订阅已被暂停，请联系客服或续费恢复"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("subscription not found\n"))
	}
}

// ---------------------------------------------------------------------------
// Store (shop) authentication
// ---------------------------------------------------------------------------

// storeSecret returns the configured shop secret, or "" when unset.
//
// 2026-11-04：读库失败要作为错误返回，不能当成「未配置」。原来一律回 ""：数据库抖一下，
// 商城接口就回 503「请先生成对接密钥」，订阅中心也显示「未生成密钥」并给出「生成对接密钥」按钮 ——
// 运营照着点，正在用的密钥就被换掉，商城随即全部 401。
func (s *Server) storeSecret(ctx context.Context) (string, error) {
	var value string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, storeSecretKey).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// requireStoreKey guards the machine-to-machine routes. The comparison is
// constant time so a wrong key cannot be recovered by timing.
func (s *Server) requireStoreKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expected, err := s.storeSecret(r.Context())
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if expected == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "商城对接尚未启用：请先在「订阅中心」生成对接密钥"})
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-Store-Key"))
		if provided == "" {
			provided = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "对接密钥无效"})
			return
		}
		signatureRequired, err := s.storeSignatureRequired(r)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if signatureRequired {
			protocolOK, err := s.acceptStoreProtocol(r)
			if err != nil {
				s.databaseError(w, r, err)
				return
			}
			if !protocolOK {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请升级商城为签名v2（绑定查询参数与独立nonce）"})
				return
			}
		}
		// 签名（见 storesign.go）：带了就必须对；没带时只有未开启「强制签名」才放行。
		signed, err := verifyStoreSignature(r, expected, time.Now())
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "签名校验失败：" + err.Error()})
			return
		}
		if !signed && signatureRequired {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "面板已开启强制签名：请求需带 X-Store-Timestamp 与 X-Store-Signature"})
			return
		}
		if signed {
			if err := s.persistStoreNonce(r, expected); err != nil {
				status := http.StatusConflict
				if errors.Is(err, errStoreNonceUnavailable) {
					status = http.StatusServiceUnavailable
					w.Header().Set("Retry-After", "5")
				}
				writeJSON(w, status, map[string]string{"error": err.Error()})
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// Order (shop) handlers
// ---------------------------------------------------------------------------

// POST /api/store/orders — the shop provisions a subscription after payment.
//
// The caller supplies the order id, the validity it sold, and the device limit;
// the panel mints a token and returns every URL the shop needs to deliver. The
// call is idempotent on order_id: a retried webhook returns the same
// subscription rather than minting a second one.
func (s *Server) storeCreateOrder(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrderID      string `json:"order_id"`
		Label        string `json:"label"`
		DurationDays int    `json:"duration_days"`
		ExpiresAt    int64  `json:"expires_at"`
		IPLimit      int    `json:"ip_limit"`
		// Amount / Currency 可选：订单实付金额，用于利润报表（按订单时长折算成月收入）。
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
		// Pointer so an absent field keeps the safe default (delete after the
		// grace window) while an explicit false is honoured. A plain bool would
		// make "false" indistinguishable from "not sent", and the shop would get
		// a 200 while its subscription was still being purged.
		AutoDelete *bool  `json:"auto_delete"`
		UserEmail  string `json:"user_email"`
		Note       string `json:"note"`
		// NodeCategory 是**分类的 slug**（不是 uuid）——
		// 商城库里存的也是 slug，可读、排障时一眼看出这个商品卖的是哪类。
		// 留空 = 不按分类圈定（全部节点），这是为了兼容不带该字段的老商城。
		NodeCategory string `json:"node_category"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	input.OrderID = strings.TrimSpace(input.OrderID)
	if input.OrderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id 必填"})
		return
	}
	if len(input.OrderID) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id 过长"})
		return
	}
	if input.DurationDays <= 0 && input.ExpiresAt <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 或 expires_at 至少填一个"})
		return
	}
	if input.DurationDays < 0 || input.DurationDays > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 需在 1-3650 之间"})
		return
	}
	if input.IPLimit < 0 || input.IPLimit > 1000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_limit 需在 0-1000 之间"})
		return
	}

	// Idempotency: a replayed webhook must not mint a second subscription.
	if existing, err := s.loadSubscriptionByOrder(r.Context(), input.OrderID); err == nil {
		// 2026-11-06：已过期 / 已停用的行不能再当成「下单成功」返回。
		// writeStoreOrder 对这类行不会给任何 *_url（那是正确的），但它同样不走 207 分支 ⇒ 商城收到
		// 200 + ok:true + 没有链接：交付「成功」了却没有任何可用的东西，也不会进重试队列。
		// 回 409 让商城走 /store/renew（延期并重新开户）或换新单号，两种情况都会有人看到。
		expired, disabled := existing.expired(time.Now()), !existing.Enabled
		if expired || disabled {
			reason := "该订单的订阅已过期"
			if disabled && !expired {
				reason = "该订单的订阅已停用"
			}
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "order_id": input.OrderID, "token": existing.Token,
				"expired": expired, "disabled": disabled,
				"error": reason + "，不能按同一订单号重复开通；请调用 /api/store/renew 续期，或使用新的订单号",
			})
			return
		}
		s.writeStoreOrder(w, r, existing)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-06：商城明确撤销过的订单号不能再开一份订阅（见迁移 056）。
	// 原来撤单软删后，同一 order_id 的重试下单会建一份新订阅、在所有节点上重新开户 —— 退款之后货又发了一次。
	var revoked bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id=$1)`, input.OrderID).Scan(&revoked); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if revoked {
		writeJSON(w, http.StatusGone, map[string]string{"error": "该订单已被商城撤销（退款 / 取消），不能再次开通；如需重新开通请用新的订单号"})
		return
	}

	// 分类圈定（2026-10-03）：商城按商品所属分类圈定买家能拿到哪些节点。
	//
	// 三态处理，别合并：
	//   node_category 为空  → categoryID = nil（不圈定，= 加分类前的老行为）
	//   slug 存在            → categoryID = 该分类的 id
	//   slug 不存在          → **报 400**，不能静默按「不圈定」处理
	//
	// ⚠️ 最后一条很关键：静默降级意味着买家本该只拿到 AI 专用节点、
	// 却拿到了全部节点 —— 配置写错时反而发得更多。
	// 而「字段为空」必须当作不圈定：老商城（或升级中的实例）不带这个字段，
	// 报错会让它全部下单失败。
	var categoryID *string
	if slug := strings.TrimSpace(input.NodeCategory); slug != "" {
		id, enabled, ok, err := s.resolveCategoryBySlug(r.Context(), slug)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "节点分类「" + slug + "」不存在，请先在面板的分类管理里建好；" +
					"留空 node_category 表示不按分类圈定（给全部节点）",
			})
			return
		}
		if !enabled {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "节点分类「" + slug + "」已停用，不再接受新订单；请在面板重新启用，或把商品改到其它分类",
			})
			return
		}
		categoryID = &id
	}

	now := time.Now()
	var expiresAt *time.Time
	if input.ExpiresAt > 0 {
		t := time.Unix(input.ExpiresAt, 0)
		// 2026-11-03：新订单的到期时间不能已经过去。
		if !t.After(now) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires_at 已经过去，新订单的到期时间必须晚于当前时间（单位是秒级 Unix 时间戳）"})
			return
		}
		// 限制最长 10 年，防止毫秒级时间戳被误当秒级写入公元 58849 年造成实质永久白嫖。
		if t.After(now.AddDate(10, 0, 1)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires_at 超过 10 年上限，请确认传入的是秒级 Unix 时间戳而非毫秒"})
			return
		}
		expiresAt = &t
	} else {
		t := now.AddDate(0, 0, input.DurationDays)
		expiresAt = &t
	}

	label := strings.TrimSpace(input.Label)
	if label == "" {
		label = "商城订阅 · " + input.OrderID
	}
	label = truncateRunes(label, 64)
	note := truncateRunes(strings.TrimSpace(input.Note), 256)
	email := truncateRunes(strings.TrimSpace(input.UserEmail), 190)
	// Absent auto_delete keeps the default (purge past the grace window); only an
	// explicit false makes the row outlive its expiry indefinitely.
	autoDelete := true
	if input.AutoDelete != nil {
		autoDelete = *input.AutoDelete
	}

	token, err := newSubscriptionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成订阅令牌失败"})
		return
	}
	// Expired orders are created disabled-agnostic but honest: the delivery page
	// will show "已到期" immediately, and the feed refuses to serve.
	//
	// ON CONFLICT is what makes the replay guarantee above actually hold. The
	// read-then-insert check is still worth keeping (it answers the common
	// replay without burning a token), but on its own it cannot survive
	// concurrency: concurrent requests all read "no rows" before any of them
	// commits, and then all of them insert. The partial unique index added in
	// migration 011 makes the database the final judge; DO NOTHING turns the
	// loser of that race into a no-op instead of a 500.
	//
	// Returns no row when the conflict loses, hence QueryRow can come back with
	// no rows: that is the signal to fall through and serve the winner's row.
	// 事务内按 order_id 加事务级 advisory 锁并与 store_revoked_orders 原子检查，彻底消除跨表写偏斜复活窗口。
	createTx, err := s.db.Begin(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer createTx.Rollback(r.Context())
	if _, err := createTx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext('hy2-ops:store-order:' || $1))`, input.OrderID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	row := createTx.QueryRow(r.Context(), `
INSERT INTO subscription_tokens (token, label, enabled, expires_at, ip_limit, auto_delete, order_id, user_email, note, category_id)
SELECT $1, $2, TRUE, $3, $4, $5, $6, NULLIF($7,''), $8, $9::uuid
WHERE NOT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id = $6)
ON CONFLICT (order_id) WHERE deleted_at IS NULL AND order_id IS NOT NULL DO NOTHING
RETURNING `+subscriptionColumns,
		token, label, expiresAt, input.IPLimit, autoDelete, input.OrderID, email, note, categoryID)
	created, err := scanSubscription(row)
	if err == nil {
		if commitErr := createTx.Commit(r.Context()); commitErr != nil {
			s.databaseError(w, r, commitErr)
			return
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		_ = createTx.Rollback(r.Context())
		if existing, lerr := s.loadSubscriptionByOrder(r.Context(), input.OrderID); lerr == nil {
			expired, disabled := existing.expired(time.Now()), !existing.Enabled
			if expired || disabled {
				reason := "该订单的订阅已过期"
				if disabled && !expired {
					reason = "该订单的订阅已停用"
				}
				writeJSON(w, http.StatusConflict, map[string]any{
					"ok": false, "order_id": input.OrderID, "token": existing.Token,
					"expired": expired, "disabled": disabled,
					"error": reason + "，不能按同一订单号重复开通；请调用 /api/store/renew 续期，或使用新的订单号",
				})
				return
			}
			s.writeStoreOrder(w, r, existing)
			return
		}
		var revokedNow bool
		if rerr := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id=$1)`, input.OrderID).Scan(&revokedNow); rerr == nil && revokedNow {
			writeJSON(w, http.StatusGone, map[string]string{"error": "该订单已被商城撤销（退款 / 取消），不能再次开通；如需重新开通请用新的订单号"})
			return
		}
		writeJSON(w, http.StatusConflict,
			map[string]string{"error": "该订单正在处理中，请稍后重试"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}

	// 2026-11-12：金额记账与下单审计放在节点开户之前，并用不随请求取消的短超时写库。
	//
	// 原来这三笔写库排在 provisionAndPersist 之后、沿用 r.Context()：开户要逐节点往返，有节点不可达时是十秒量级，
	// 商城的 HTTP 请求先超时断开 → r.Context() 已取消 → 收入没记进报表，两条审计也写不进去；
	// 商城重试时走上面的幂等分支直接返回，不再补记。订阅行此时已经存在，created.ID / ExpiresAt 都可用。
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	// 2026-11-06：金额没记进报表时留下审计 —— 否则「客户付了钱、利润报表里没有这笔收入」无从发现。
	if recorded, reason := s.recordOrderRevenue(recordCtx, created.ID, input.Amount, input.Currency, orderPeriodDays(input.DurationDays, nil, created.ExpiresAt)); !recorded && reason != "" {
		s.logger.Warn("store: 下单金额未记账", "order_id", input.OrderID, "reason", reason, "amount", input.Amount, "duration_days", input.DurationDays)
		s.audit(recordCtx, r, "store_revenue_skipped", "subscription_token", created.ID,
			map[string]any{"order_id": input.OrderID, "reason": reason, "amount": input.Amount, "duration_days": input.DurationDays})
	}
	s.audit(recordCtx, r, "store_create_order", "subscription_token", created.ID,
		map[string]any{"order_id": input.OrderID, "duration_days": input.DurationDays, "ip_limit": input.IPLimit, "auto_delete": autoDelete})
	cancelRecord()

	// Open this buyer's own account on every registered node, and remember the
	// credentials. This is what makes expiry enforceable: the config we hand out
	// carries a password whose account we can delete, instead of the node's
	// shared one (see provisioning.go). Best-effort by design — a node that is
	// down must not fail the order, it just does not appear in this buyer's
	// config until the operator re-runs provisioning.
	s.provisionAndPersist(r.Context(), created)

	// 纵深防御告警（2026-10-04）：商城侧曾把合并订阅的 ip_limit 写死成 0，
	// 商品配了设备上限、买家订阅却「不限」。根因已在商城侧修掉，这里补一道哨兵——
	// OPS 前缀的合并订阅下单时若 ip_limit=0，记一条 warning。
	// 不拦截、不改行为：「不限设备」本身是合法商品形态，硬拦会误伤真·不限的商品。
	// 只在真正新建这一条时告警：幂等重放走上面的 ErrNoRows / ON CONFLICT 分支已 return。
	if strings.HasPrefix(input.OrderID, "OPS-") && created.IPLimit == 0 {
		s.logger.Warn("store: OPS 合并订阅下单 ip_limit=0（OPS_IPLIMIT_ZERO）若非有意卖「不限设备」，请检查商城商品配置",
			"order_id", input.OrderID, "subscription", shortID(created.ID))
	}

	s.writeStoreOrder(w, r, created)
}

// provisionAndPersist opens the subscription's accounts on every node and stores
// the mapping on the row. Errors are logged, never surfaced to the shop: from
// the shop's point of view the order succeeded, and it did — a subscription
// exists and will serve whatever it could provision. The delivery page tells the
// truth about the rest through `excluded`.
func (s *Server) provisionAndPersist(ctx context.Context, rec *subscriptionRecord) {
	if err := s.synchronizeSubscription(ctx, rec); err != nil {
		s.logger.Warn("delivery provisioning pending", "subscription", shortID(rec.ID), "error", err)
	}
}

// storeRenewQuery 是续费的唯一写法（抽出来便于在 PostgreSQL 上直接测）。
const storeRenewQuery = `
UPDATE subscription_tokens
SET expires_at = CASE
        -- 2026-11-06：商城的目标到期时间只能把订阅往后推，不能缩短。
        -- 原来这里直接赋 $1，商城按「付款前已算好的目标」下发，运营在控制台手动延长过的
        -- 天数会被冲回商城算出的值；永久订阅（NULL）也会被改成有限期，随后被到期回收。
        WHEN $1::timestamptz IS NOT NULL AND expires_at IS NULL THEN NULL
        WHEN $1::timestamptz IS NOT NULL THEN GREATEST(expires_at, $1::timestamptz)
        -- $1 为空表示「按天续费」：沿用 2026-11-03 的永久订阅语义（NULL 保持 NULL）。
        WHEN expires_at IS NULL THEN NULL
        ELSE GREATEST(expires_at, now()) + make_interval(days => $2::int)
    END,
    ip_limit   = $3,
    enabled    = TRUE,
    active_ips = '[]'::jsonb
WHERE id = $4::uuid`

// POST /api/store/renew — extend an existing order's expiry (renewal or refund
// reversal). Only the subscription belonging to that order_id is touched.
func (s *Server) storeRenewOrder(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrderID      string  `json:"order_id"`
		RenewalID    string  `json:"renewal_id"`
		DurationDays int     `json:"duration_days"`
		ExpiresAt    int64   `json:"expires_at"`
		IPLimit      *int    `json:"ip_limit"`
		Amount       float64 `json:"amount"`
		Currency     string  `json:"currency"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	input.OrderID = strings.TrimSpace(input.OrderID)
	if input.OrderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id 必填"})
		return
	}
	if input.DurationDays <= 0 && input.ExpiresAt <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 或 expires_at 至少填一个"})
		return
	}
	if input.DurationDays < 0 || input.DurationDays > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 需在 1-3650 之间"})
		return
	}
	// 2026-11-04：与新建订单同一条规则：续期到一个已经过去的时间不是续期。原来照样执行：
	// 写入过去的 expires_at、置 enabled=TRUE，并在同一事务里删掉待办的停用 / 回收 ——
	// 退款后回收还在排队的订单被一次写错的续期调用（毫秒当成秒、时区算错）把回收作废，
	// 而订阅仍是到期状态，回包却是 200。
	if input.ExpiresAt > 0 {
		tNow := time.Now()
		tExp := time.Unix(input.ExpiresAt, 0)
		if !tExp.After(tNow) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires_at 已经过去，续期后的到期时间必须晚于当前时间（单位是秒级 Unix 时间戳）"})
			return
		}
		if tExp.After(tNow.AddDate(10, 0, 1)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires_at 超过 10 年上限，请确认传入的是秒级 Unix 时间戳而非毫秒"})
			return
		}
	}
	input.RenewalID = strings.TrimSpace(input.RenewalID)
	if len(input.RenewalID) > 200 || (input.ExpiresAt <= 0 && input.RenewalID == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "相对续期必须提供稳定 renewal_id（最多200字节）"})
		return
	}

	rec, err := s.loadSubscriptionByOrder(r.Context(), input.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到该订单对应的订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}

	var release func()
	var locked bool
	r, release, locked = s.lockSubscriptionRequest(w, r, rec)
	if !locked {
		return
	}
	defer release()
	// 2026-11-06：订阅已停用、且还有商城撤单留下的回收待办时，不接受不带 renewal_id 的续期。
	// 绝对时间续期不强制 renewal_id（商城的绝对续期不带），也就无法识别重放：
	// 「续期成功但商城超时 → 买家退款、撤单回收部分失败排队 → 商城重发那次续期」会重新启用订阅、
	// 作废回收待办、在所有节点上重新开户 —— 退款用户恢复了服务。真续费请带 renewal_id。
	// 2026-11-06：条件去掉 RenewalID 判断。相对续期**必须**带 renewal_id（上面已强制），所以原来
	// 这个闸门对相对续期永远不成立：退款（delete=false）部分节点回收失败留下 pending revoke 后，
	// 商城重发一次相对续期（新的 renewal_id）就会重新启用订阅、删掉回收待办、在所有节点重新开户 ——
	// 退款用户恢复服务。重开必须走人工/显式确认，不能由一次续期悄悄完成。
	// 已被商城明确撤销（delete=true）入黑名单的订单绝不允许通过续费复活。
	var orderRevoked bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id=$1)`, input.OrderID).Scan(&orderRevoked); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if orderRevoked {
		writeJSON(w, http.StatusGone, map[string]string{"error": "该订单已被商城永久撤销（退款 / 取消），不能再续费"})
		return
	}
	if !rec.Enabled {
		var pendingRevoke bool
		if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid AND action='revoke')`, rec.ID).Scan(&pendingRevoke); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if pendingRevoke {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该订单的撤销回收仍在进行（含未回收成功的节点），暂不接受续期，以免把已退款的订阅重新开通；请在控制台查看「孤儿账号」并确认回收完成后再续费"})
			return
		}
	}
	// Extending an expiry must happen in the database, for the same reason the IP
	// ledger does: read-modify-write loses updates. Five concurrent renewals of
	// +10 days used to read the same `expires_at`, each compute "old + 10", and
	// overwrite each other — the customer paid five times and got ten days. That
	// is money, not just a counter, so the arithmetic moves into the statement.
	//
	// The base is GREATEST(current expiry, now): renewing early extends from the
	// existing expiry so the customer is not punished for paying ahead, while a
	// lapsed subscription restarts from today.
	var expiresAt time.Time
	if input.ExpiresAt > 0 {
		expiresAt = time.Unix(input.ExpiresAt, 0)
	}

	ipLimit := rec.IPLimit
	if input.IPLimit != nil {
		if *input.IPLimit < 0 || *input.IPLimit > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_limit 需在 0-1000 之间"})
			return
		}
		ipLimit = *input.IPLimit
	}

	var expiryArg *time.Time
	if input.ExpiresAt > 0 {
		expiryArg = &expiresAt
	}
	// The unique business event and expiry change commit together. A new signed
	// transport nonce must never turn a retry into another purchased period.
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	replayed := false
	if input.RenewalID != "" {
		canonical, _ := json.Marshal(struct {
			Order    string
			Days     int
			Expiry   int64
			Limit    *int
			Amount   float64
			Currency string
		}{input.OrderID, input.DurationDays, input.ExpiresAt, input.IPLimit, input.Amount, input.Currency})
		tag, err := tx.Exec(r.Context(), `INSERT INTO store_renewal_events
			(renewal_id, subscription_id, request_payload) VALUES ($1,$2::uuid,$3::jsonb)
			ON CONFLICT (renewal_id) DO NOTHING`, input.RenewalID, rec.ID, string(canonical))
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if tag.RowsAffected() == 0 {
			var same bool
			if err := tx.QueryRow(r.Context(), `SELECT subscription_id=$2::uuid AND request_payload=$3::jsonb
				FROM store_renewal_events WHERE renewal_id=$1`, input.RenewalID, rec.ID, string(canonical)).Scan(&same); err != nil {
				s.databaseError(w, r, err)
				return
			}
			if !same {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "renewal_id 已用于不同的续期请求"})
				return
			}
			replayed = true
		}
	}
	if !replayed {
		if _, err := tx.Exec(r.Context(), storeRenewQuery, expiryArg, input.DurationDays, ipLimit, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
	}
	var renewalExpiry *time.Time
	var renewalIPLimit int
	if input.RenewalID != "" {
		if !replayed {
			if _, err := tx.Exec(r.Context(), `UPDATE store_renewal_events e SET
				result_expires_at=s.expires_at, result_ip_limit=s.ip_limit
				FROM subscription_tokens s WHERE e.renewal_id=$1 AND s.id=$2::uuid`, input.RenewalID, rec.ID); err != nil {
				s.databaseError(w, r, err)
				return
			}
		}
		if err := tx.QueryRow(r.Context(), `SELECT result_expires_at,result_ip_limit FROM store_renewal_events WHERE renewal_id=$1`, input.RenewalID).Scan(&renewalExpiry, &renewalIPLimit); err != nil {
			s.databaseError(w, r, err)
			return
		}
	}
	var skippedRevenueAudit map[string]any
	if !replayed && input.Amount > 0 {
		var newExpiry *time.Time
		if err := tx.QueryRow(r.Context(), `SELECT expires_at FROM subscription_tokens WHERE id=$1::uuid`, rec.ID).Scan(&newExpiry); err != nil {
			s.databaseError(w, r, err)
			return
		}
		// 2026-11-06：按**实际延长了多少天**折算，不再用 duration_days。
		// 到期时间由 expires_at 决定（storeRenewQuery 只用 $1），而 duration_days 只在这里被读到 ——
		// 两个参数不一致时会出现「按 30 天收费、只延长 3 天」的记账。另外目标时间被运营手工延长过、
		// 或同一次续费换了新 renewal_id 重放时，GREATEST 让到期不变，days 变成 0 —— 原来直接跳过，
		// 这笔钱静默不进月收入，而回包仍是 200。
		currency := strings.ToUpper(strings.TrimSpace(input.Currency))
		if currency == "" {
			currency = "CNY"
		}
		days := orderPeriodDays(0, rec.ExpiresAt, newExpiry)
		switch {
		case !billingCurrencies[currency]:
			s.logger.Warn("store: 续费金额未记账（不支持的币种）", "order_id", input.OrderID, "currency", currency)
			skippedRevenueAudit = map[string]any{"reason": "unsupported_currency", "currency": currency, "amount": input.Amount}
		case days <= 0:
			s.logger.Warn("store: 续费金额未记账（到期时间没有实际往后延）", "order_id", input.OrderID, "amount", input.Amount)
			skippedRevenueAudit = map[string]any{"reason": "no_extension", "amount": input.Amount, "expires_at": epochOrZero(newExpiry)}
		default:
			monthly := math.Round(input.Amount/(float64(days)/30)*100) / 100
			if _, err := tx.Exec(r.Context(), `UPDATE subscription_tokens SET revenue_monthly=$2, revenue_currency=$3 WHERE id=$1::uuid`, rec.ID, monthly, currency); err != nil {
				s.databaseError(w, r, err)
				return
			}
		}
	}
	// 续费把订阅重新置为生效：之前排队的停用/回收必须在**同一个事务里**作废。
	//
	// 挪进事务的原因（2026-10-30）：原来的写法是 commit 之后单独 DELETE。
	// 若续费事务已经提交但这一步失败（DB 抖动、连接断开），sweeper 下一轮
	// 仍会按旧待办去停用/回收刚续费的账号 —— 买家付了钱，账号却被停掉。
	// 放进事务后两者要么都成、要么都不成，不存在"续费成功但停用仍然排队"。
	//
	// 2026-11-03：重放（同一 renewal_id 再来一次）不改到期也不改启用状态，
	// 就不能作废待办。否则「续费 → 退款回收部分失败排队 → 商城重发那次续费」
	// 会把退款的回收待办删掉，行仍是停用，再没有任何路径去回收那台节点上的账号。
	if !replayed {
		if _, err := tx.Exec(r.Context(), `DELETE FROM subscription_pending_node_actions WHERE subscription_id=$1::uuid`, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 事务已提交：审计日志放在提交之后且脱离请求取消，既避免持事务时再借连接池产生死锁，也避免节点同步超时丢审计。
	auditCtx, stopAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	if skippedRevenueAudit != nil {
		s.audit(auditCtx, r, "renewal_revenue_skipped", "subscription_token", rec.ID, skippedRevenueAudit)
	}
	if !replayed {
		s.audit(auditCtx, r, "store_renew_order", "subscription_token", rec.ID,
			map[string]any{"order_id": input.OrderID, "duration_days": input.DurationDays, "renewal_id": input.RenewalID})
	}
	stopAudit()
	// 续费已提交；cancelPendingNodeAction 已在事务里做完，不再单独调用。
	updated, err := s.loadSubscriptionByID(r.Context(), rec.ID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}

	// Push the new deadline out to the node-side accounts as well. Without this
	// the node account would still lapse at the ORIGINAL date while the
	// subscription (and the delivery page) says the customer is paid up — the
	// buyer would be billed for time the node refuses to serve.
	// synchronizeSubscription 内部会自动补开缺失节点账号，无需再重复调用 provisionAndPersist。
	if updated.Enabled && !updated.expired(time.Now()) {
		s.renewSubscriptionAccounts(r.Context(), updated)
	}
	if input.RenewalID != "" && (!replayed || (updated.Enabled && !updated.expired(time.Now()))) {
		// 重放时仅当订阅仍处于实时有效状态才回填当时记录的续期快照，绝不用历史快照把已停用/已过期的订阅伪装为有效。
		updated.ExpiresAt = renewalExpiry
		updated.IPLimit = renewalIPLimit
	}
	s.writeStoreOrder(w, r, updated)
}

// renewSubscriptionAccounts extends every node-side account to match the
// subscription's new expiry. Best-effort, like provisioning: a node that is down
// is logged and skipped, and its account simply runs to its old date.
func (s *Server) renewSubscriptionAccounts(ctx context.Context, rec *subscriptionRecord) {
	if err := s.synchronizeSubscription(ctx, rec); err != nil {
		s.logger.Warn("delivery renewal pending", "subscription", shortID(rec.ID), "error", err)
	}
}

// subscriptionNodeRenewDeadline 返回绝对期限；到期订阅绝不重新激活。
// 有限订阅精确对齐截止秒数，永久订阅保留已有的更长期限。
func subscriptionNodeRenewDeadline(rec *subscriptionRecord, current int64, now time.Time) int64 {
	if rec.ExpiresAt != nil {
		if !rec.ExpiresAt.After(now) {
			return 0
		}
		return rec.ExpiresAt.Unix()
	}
	deadline := now.AddDate(0, 0, 3650).Unix()
	if current > deadline {
		return current
	}
	return deadline
}

// parseAccountExpiry picks one account out of a /users/list payload.
//
// Split out from the network call so the shape handling is unit-testable: the
// portal client refuses to talk to non-public addresses (SSRF guard), so a
// loopback test server is not an option.
func parseAccountExpiry(raw []byte, userID string) (int64, error) {
	var parsed portalUsersListResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, errors.New("节点返回的用户列表无法解析")
	}
	for _, u := range parsed.Users {
		if u.UserID == userID {
			return u.ExpiresAt, nil
		}
	}
	// Not "0, nil": a missing account would then look like "expires at epoch"
	// and the caller would hand the node a 3650-day extension.
	return 0, errors.New("节点上找不到该账号")
}

// nodeRenewDeltaDays returns how many more days the node account needs to reach
// the subscription's deadline — the DIFFERENCE, not the total.
//
// The node's /users/renew is relative (`base = max(expiry, now) + days`) while
// the subscription's deadline is absolute. Handing it the subscription's full
// remaining validity double-counts on every renewal: a 30-day renewal grew the
// account by 60 days and left it 29 days past the subscription, so a buyer who
// stopped paying kept connecting with the config already in their client. That
// is the exact failure this file exists to prevent — an account must not outlive
// the subscription that pays for it.
//
// Deriving the delta from the account's own expiry makes the call idempotent:
// running it twice in a row computes the same target instead of stacking days.
//
// Day granularity is all the node accepts, so the result rounds up. Overshooting
// by under a day is invisible; rounding down would cut a paying customer off
// before their subscription ends.
func nodeRenewDeltaDays(rec *subscriptionRecord, currentExpiry int64, now time.Time) int {
	if rec.ExpiresAt == nil {
		// Perpetual: provisioning already asked for the panel's own maximum, so
		// only push out an account that somehow lagged behind that window.
		if ceiling := now.AddDate(0, 0, 3650).Unix(); currentExpiry >= ceiling {
			return 0
		}
		return subscriptionToNodeDays(rec, now)
	}
	base := currentExpiry
	if nowUnix := now.Unix(); nowUnix > base {
		base = nowUnix
	}
	remaining := rec.ExpiresAt.Unix() - base
	if remaining <= 0 {
		return 0
	}
	days := int((remaining + 86399) / 86400)
	if days < 1 {
		days = 1
	}
	return days
}

// POST /api/store/revoke — the shop disables a subscription (refund, chargeback,
// abuse). The row is kept so a later reinstate is possible.
func (s *Server) storeRevokeOrder(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrderID string `json:"order_id"`
		Delete  bool   `json:"delete"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	input.OrderID = strings.TrimSpace(input.OrderID)
	if input.OrderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id 必填"})
		return
	}
	rec, err := s.loadSubscriptionByOrder(r.Context(), input.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 2026-11-06：订阅早已不在（到期后被清理）时，撤单（delete=true，退款 / 拒付）仍要登记订单号。
			// 与 storeCreateOrder 共享同一把按 order_id 的事务级锁，闭合「并发建单刚提交、撤单却当 404 返回」的写偏斜窗口。
			if input.Delete {
				revTx, txErr := s.db.Begin(r.Context())
				if txErr != nil {
					s.databaseError(w, r, txErr)
					return
				}
				if _, txErr = revTx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtext('hy2-ops:store-order:' || $1))`, input.OrderID); txErr != nil {
					_ = revTx.Rollback(r.Context())
					s.databaseError(w, r, txErr)
					return
				}
				if _, txErr = revTx.Exec(r.Context(), `INSERT INTO store_revoked_orders(order_id, subscription_id)
					VALUES($1, '00000000-0000-0000-0000-000000000000'::uuid) ON CONFLICT (order_id) DO NOTHING`, input.OrderID); txErr != nil {
					_ = revTx.Rollback(r.Context())
					s.databaseError(w, r, txErr)
					return
				}
				if txErr = revTx.Commit(r.Context()); txErr != nil {
					_ = revTx.Rollback(r.Context())
					s.databaseError(w, r, txErr)
					return
				}
				// 持锁提交黑名单后再复核一次是否被并发建单抢先落库；若存在则继续往下停用回收。
				if raced, rerr := s.loadSubscriptionByOrder(r.Context(), input.OrderID); rerr == nil {
					rec = raced
					err = nil
				} else if !errors.Is(rerr, pgx.ErrNoRows) {
					s.databaseError(w, r, rerr)
					return
				}
			}
			if err != nil {
				if input.Delete {
					s.audit(r.Context(), r, "store_revoke_order_gone", "store_order", "", map[string]any{"order_id": input.OrderID})
				}
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "未找到该订单对应的订阅", "already_gone": true, "revoked_recorded": input.Delete})
				return
			}
		} else {
			s.databaseError(w, r, err)
			return
		}
	}
	var release func()
	var locked bool
	r, release, locked = s.lockSubscriptionRequest(w, r, rec)
	if !locked {
		return
	}
	defer release()
	// Disable serving the subscription before contacting nodes, but retain its
	// row and node_accounts mapping until every node confirms revocation.
	if _, err := s.db.Exec(r.Context(), `UPDATE subscription_tokens SET enabled=FALSE, active_ips='[]'::jsonb WHERE id=$1::uuid`, rec.ID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-06：撤单（delete=true）的订单号在这里就落盘，不要等回收全部成功。
	// 原来只在「所有节点都撤干净」的事务里写：一旦有节点不可达，就只登记待办，订单号只存在于订阅行上；
	// 而待办真正执行时若该行已被 sweeper 硬删 / 运营结案删掉，待办直接作废，blacklist 永远不落 ——
	// 之后同一 order_id 的下单重放会重新开户，退款买家拿到一套新的可用凭证。
	if input.Delete {
		if _, err := s.db.Exec(r.Context(), `INSERT INTO store_revoked_orders(order_id, subscription_id)
			VALUES($1, $2::uuid) ON CONFLICT (order_id) DO NOTHING`, input.OrderID, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
	}
	outcome := s.revokeSubscriptionAccounts(r.Context(), rec)
	if !outcome.OK() {
		// 订阅已停用、未到期：到期/清理查询都不会再碰它，必须登记待办由后台补删。
		queueErr := s.queuePendingNodeActionFor(r.Context(), rec.ID, pendingActionRevoke, input.Delete, input.Delete, "商城撤销订单时部分节点回收失败")
		auditCtx, stopAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		s.audit(auditCtx, r, "store_revoke_order_failed", "subscription_token", rec.ID,
			map[string]any{"order_id": input.OrderID, "deleted": input.Delete,
				"revoked_count": outcome.Revoked, "failed": outcome.Failed, "queued": queueErr == nil})
		stopAudit()
		msg := "部分节点回收失败，保留记录等待重试"
		if queueErr != nil {
			// 没登记上就不能说「等待重试」：让商城自己重发撤销。
			msg = "部分节点回收失败，且后台重试登记失败；请重新发起撤销"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "pending": queueErr == nil, "error": msg,
			"order_id": input.OrderID, "deleted": false,
			"revoked_count": outcome.Revoked, "failed": publicRevokeFailures(outcome.Failed),
		})
		return
	}
	// 节点物理回收全部完成后，数据库软删与审计使用脱离请求取消的有界上下文，防调用方超时断开造成半成品状态。
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer commitCancel()
	if input.Delete {
		tx, err := s.db.Begin(commitCtx)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		defer tx.Rollback(commitCtx)
		if _, err := tx.Exec(commitCtx, `INSERT INTO store_revoked_orders(order_id, subscription_id) VALUES($1, $2::uuid) ON CONFLICT (order_id) DO NOTHING`, input.OrderID, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if _, err := tx.Exec(commitCtx, `UPDATE subscription_tokens SET deleted_at=now() WHERE id=$1::uuid`, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if _, err := tx.Exec(commitCtx, `DELETE FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, rec.ID); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if err := tx.Commit(commitCtx); err != nil {
			s.databaseError(w, r, err)
			return
		}
	}
	// 物理回收已全部确认：顺带清理历史待办与回收退避记录，避免后台继续做无用重试。
	s.cancelPendingNodeAction(commitCtx, rec.ID)
	s.clearAllRevokeBackoff(commitCtx, rec.ID)
	s.audit(commitCtx, r, "store_revoke_order", "subscription_token", rec.ID,
		map[string]any{"order_id": input.OrderID, "deleted": input.Delete})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": input.OrderID, "deleted": input.Delete,
		"revoked_count": outcome.Revoked})
}

// GET /api/store/orders?order_id=… — the shop asks for an order's status.
func (s *Server) storeGetOrder(w http.ResponseWriter, r *http.Request) {
	orderID := strings.TrimSpace(r.URL.Query().Get("order_id"))
	if orderID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id 必填"})
		return
	}
	rec, err := s.loadSubscriptionByOrder(r.Context(), orderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到该订单对应的订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.writeStoreOrder(w, r, rec)
}

// writeStoreOrder renders the provisioning response. Both a fresh create and an
// idempotent replay go through here so the shop always parses one shape.
func (s *Server) writeStoreOrder(w http.ResponseWriter, r *http.Request, rec *subscriptionRecord) {
	base := subscriptionBaseURL(r)
	now := time.Now()
	expired := rec.expired(now)
	payload := map[string]any{
		"ok":         true,
		"order_id":   rec.OrderID,
		"token":      rec.Token,
		"label":      rec.Label,
		"enabled":    rec.Enabled && !expired,
		"expired":    expired,
		"expires_at": epochOrZero(rec.ExpiresAt),
		// 2026-11-06：永久订阅没有到期时间，expires_at 只能是 0；商城据此区分
		// 「续费成功但订阅是永久的」与「续费没有生效」。
		"perpetual":       rec.ExpiresAt == nil,
		"expires_at_text": formatExpiryText(rec.ExpiresAt),
		"days_left":       rec.daysLeft(now),
		"ip_limit":        rec.IPLimit,
		"active_ip_count": rec.ActiveIPCount,
		// Echoed back so the shop can confirm the retention it asked for instead
		// of assuming its `auto_delete` was honoured.
		"auto_delete":  rec.AutoDelete,
		"delivery_url": base + "/user/" + rec.Token,
		"renew_url":    subscriptionRenewURL(rec, storeRenewBaseURL()),
	}
	// Same rule as subscriptionView: a lapsed or disabled subscription must not
	// carry working links. A shop that pastes these straight into a chat would
	// otherwise hand out URLs that only 403 — the customer cannot tell a dead
	// subscription from a broken purchase. Omitting them makes the shop render
	// "续费" instead, and keeps /sub from being the only place that knows.
	if rec.Enabled && !expired {
		payload["subscription_url"] = subLink(base, rec.Token, "")
		payload["clash_url"] = subLink(base, rec.Token, "clash")
		payload["singbox_url"] = subLink(base, rec.Token, "singbox")
		payload["uri_url"] = subLink(base, rec.Token, "uri")
	}
	status, message, err := s.subscriptionDeliveryState(r.Context(), rec.ID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	payload["delivery_status"] = status
	if rec.Enabled && !expired && (status == "pending" || status == "cancelled" || len(rec.nodeAccounts) == 0) {
		payload["ok"] = false
		payload["pending"] = true
		payload["error"] = "节点交付尚未完成，后台将自动重试"
		_ = message
		writeJSON(w, http.StatusMultiStatus, payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// subscriptionBaseURL derives the externally visible origin. Caddy preserves the
// Host header and sets X-Forwarded-Proto, so this yields the address a customer
// should actually open. Falls back to the relative path when Host is absent
// (direct API testing), matching subscriptionPublicURL's behaviour.
//
// 设置了交付域名（控制台设置 > 环境变量 SUBSCRIPTION_PUBLIC_BASE_URL，见 deliverydomain.go）时优先使用：
// 商城是通过面板域名调接口的，按请求 Host 推导会让发给买家的链接全是面板域名。
func subscriptionBaseURL(r *http.Request) string {
	if v := deliveryBaseOverride(); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func epochOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func formatExpiryText(t *time.Time) string {
	if t == nil {
		return "永久有效"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ---------------------------------------------------------------------------
// Public delivery endpoints (no JWT — the token in the path is the credential)
// ---------------------------------------------------------------------------

// GET /api/sub/view/{token} — everything the delivery page needs to render,
// with no secrets: node hosts and capacities only, never passwords or keys.
func (s *Server) subscriptionView(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	if !plausibleSubscriptionToken(token) {
		subscriptionNotFound(w)
		return
	}
	if !s.enforcePublicLimit(w, "subview:"+token) {
		return
	}
	rec, err := s.loadSubscriptionByToken(r.Context(), token)
	if err != nil {
		// 2026-11-06：只有查无此行才是 404。原来任何错误都回 404：数据库抖一下，客户端与交付页
		// 都被告知「订阅不存在」—— 交付页随即把付费买家的页面换成「订阅已被回收」。
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("subscription: 读取订阅失败", "error", err)
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取订阅，请稍后再试"})
			return
		}
		subscriptionNotFound(w)
		return
	}
	now := time.Now()
	// `!Before` (>=) so the reclaim instant matches `expired()`, which treats
	// `now == expires_at` as expired. With subscriptionExpiryGrace = 0 this makes
	// expiry and reclamation the same moment: an expired subscription is gone on
	// the first read past its deadline, from both endpoints.
	if rec.expired(now) && rec.AutoDelete && rec.ExpiresAt != nil &&
		!now.Before(rec.ExpiresAt.Add(subscriptionExpiryGrace)) {
		purgeErr := s.purgeSubscriptionOutcome(r.Context(), rec)
		if errors.Is(purgeErr, errRenewedDuringPurge) {
			fresh, rerr := s.loadSubscriptionByID(r.Context(), rec.ID)
			if rerr != nil {
				if errors.Is(rerr, pgx.ErrNoRows) {
					subscriptionNotFound(w)
					return
				}
				s.logger.Warn("subscription: 续费竞争后重读订阅失败", "subscription", shortID(rec.ID), "error", rerr)
				w.Header().Set("Retry-After", "30")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取订阅，请稍后再试"})
				return
			}
			rec = fresh
			now = time.Now()
		} else {
			subscriptionNotFound(w)
			return
		}
	}
	// ⚠️ 2026-10-04 补：过期就收回节点账号，与 auto_delete 解绑。
	//
	// 与 subscriptionFeed 里那条是同一个缺口的两处入口 ——
	// 只修一处的话，从另一个端点仍能拿到可用的活配置。
	// 判据用 node_accounts 非空 ⇒ 幂等，反复访问不会重复动作。
	if rec.expired(now) && rec.ExpiresAt != nil && len(rec.nodeAccounts) > 0 &&
		!now.Before(rec.ExpiresAt.Add(subscriptionExpiryGrace)) {
		s.revokeExpiredInBackground(rec)
	}

	// 2026-11-12：订阅是否仍生效。下面两处「暂时给不出节点」的 503 只对生效订阅有意义：
	// 已到期（保留型）或已停用的订阅，节点账号被收回后「范围内节点全都没有账号」是它的稳定状态，不是临时故障。
	// 原来一律回 503：这类订阅的交付页永远打不开，买家看到的是「订阅本身不受影响，请稍后重试」，
	// 而不是「已到期」和续费入口。失效订阅照常按 expired / enabled 渲染 200（节点清单为空）。
	active := rec.Enabled && !rec.expired(now)

	build, buildErr := s.subscriptionBuildFor(r.Context())
	nodes := []subscriptionNodePublic{}
	excluded := []subscriptionExcluded{}
	if buildErr != nil && active {
		// 2026-11-12：生效订阅的节点构建出错时与 /sub 一样回 503。原来跳过节点解析、照常回 200 加 0 个节点，
		// 也不记日志：买家的页面被换成「可用节点 0 个」且没有任何错误提示。响应用固定文案，错误原文只进日志。
		if r.Context().Err() == nil {
			s.logger.Warn("subscription: 交付页读取节点构建失败", "subscription", shortID(rec.ID), "error", buildErr)
		}
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法生成订阅，请稍后再试"})
		return
	}
	if buildErr == nil {
		// ⚠️ 必须走 subscriptionNodesFor —— 它是「这条订阅能看到哪些节点」的
		// 唯一出口，与 /sub/{token} 实际下发的内容保证一致（含分类圈定）。
		//
		// 原实现直接用 build.Nodes（全量共享构建），于是交付页报的是
		// 「全部在役节点」，而订阅里实际只给已开号且允许下发的节点 ——
		// 实测过：交付页 node_count=5，配置里只有 4 个。
		// 交付页是给买家看的，它描述的就是他将要拿到的东西，必须同源。
		served, servedErr := s.subscriptionNodesForChecked(r.Context(), build.Nodes, rec)
		if servedErr != nil && active {
			// 与 /sub 一致：暂时给不出节点时不回「0 个节点」——交付页前端遇到非 404 会保留已显示的内容。
			// 只对生效订阅这样做（见上面 active 的说明）；失效订阅的 served 是空切片，继续往下渲染。
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": servedErr.Error()})
			return
		}
		nodes = redactSubscriptionNodes(served)
		// ⚠️ 2026-10-31：公开交付页不能回传 excluded 的**内部原因**。
		//
		// 这个端点只凭 token 鉴权（买家本人就能打开），而 build.Excluded[i].Reason
		// 里是服务端的排障文本：节点 API 地址、"未绑定节点侧 API"、
		// dial 具体主机端口的错误等。交付页本身只显示条数，回传原因没有任何用处，
		// 却把内部拓扑和运维状态送到了买家浏览器里。
		// 这里只留节点名（用于说明"少的是哪台"），原因统一成一句可读文案。
		//
		// 2026-11-03：还必须按这条订阅的范围过滤。build.Excluded 是全站构建，
		// 原来买家能在交付页看到未上架的自用节点、别的分类的节点（都写着「暂时不可用」），
		// 既泄露内部节点名，也让买家以为自己少了这些节点。只列他确实开了号、
		// 本该拿到却暂时下发不了的那几台。
		for _, e := range s.excludedInScope(r.Context(), build.Excluded, rec) {
			excluded = append(excluded, subscriptionExcluded{Name: e.Name, Reason: "暂时不可用"})
		}
	}
	base := subscriptionBaseURL(r)
	expired := rec.expired(now)

	// The delivery page must never show working links for a dead subscription.
	// Instead of leaking them and hiding with CSS, the URLs are omitted entirely.
	payload := map[string]any{
		"label":           rec.Label,
		"enabled":         rec.Enabled && !expired,
		"expired":         expired,
		"expires_at":      epochOrZero(rec.ExpiresAt),
		"expires_at_text": formatExpiryText(rec.ExpiresAt),
		"days_left":       rec.daysLeft(now),
		"ip_limit":        rec.IPLimit,
		"active_ip_count": rec.ActiveIPCount,
		"ip_window_hours": int(subscriptionIPWindow.Hours()),
		// 跨节点统一设备上限：此刻被拦截的网络数（只在 enforce 模式下非零，见 devicelimit.go）。
		"device_limit_blocked": deviceLimitBlockedCount(rec.ID),
		"node_count":           len(nodes),
		"nodes":                nodes,
		"excluded":             excluded,
		"renew_url":            subscriptionRenewURL(rec, storeRenewBaseURL()),
	}
	if rec.Enabled && !expired {
		payload["subscription_url"] = subLink(base, rec.Token, "")
		payload["clash_url"] = subLink(base, rec.Token, "clash")
		payload["singbox_url"] = subLink(base, rec.Token, "singbox")
		payload["uri_url"] = subLink(base, rec.Token, "uri")
		backups := make([]string, 0)
		for _, b := range backupDeliveryBases(base) {
			backups = append(backups, subLink(b, rec.Token, ""))
		}
		payload["backup_subscription_urls"] = backups
	}
	// ⚠️ 2026-10-31：这个响应里有**订阅链接**（拿到它就能拉配置），
	// 而原实现没有任何缓存头。浏览器对没有缓存头的 JSON 会做启发式缓存，
	// 共享机器 / 中间代理都可能把这份凭证留在磁盘上；订阅被停用或删除后，
	// 缓存里那份仍然能被读出来。与 /sub/{token} 保持一致：一律 no-store。
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

// purgeSubscription removes a subscription, its node-side accounts and its IP
// ledger. Called lazily by the delivery endpoint and by the sweeper; both
// tolerate a missing row.
//
// Node accounts are revoked FIRST. If the delete below were to succeed while
// revocation failed, the row holding the account ids would be gone and the
// accounts would be orphaned on the nodes — alive, and under a password the
// buyer already has. Revoking first means the worst case is an orphaned row
// (inert) rather than a working credential (a leak).
//
// The precondition is that the row is still visible to loadSubscriptionByID,
// i.e. `deleted_at IS NULL`. Every caller must pass an id it just read live.
// Prefer purgeSubscriptionByRecord when you already hold the record: it is
// immune to the row being hidden between the read and the purge, which would
// silently skip revocation and leave an orphaned account behind (the delete
// below only removes a row the caller can no longer see, so it "succeeds"
// while doing nothing).
func (s *Server) purgeSubscription(ctx context.Context, id string) {
	rec, err := s.loadSubscriptionByID(ctx, id)
	if err != nil {
		// Row is already gone (or soft-deleted). There is nothing left to look
		// up, so we cannot revoke — but we must not stay silent about it,
		// because arriving here means an account MAY still be live on a node.
		// Log at error level so a real orphan is visible rather than implied.
		s.logger.Error("subscription: purge could not load row, node accounts may be orphaned",
			"subscription", shortID(id), "error", err)
		s.logger.Warn("subscription: purge failed", "error", err)
		return
	}
	s.purgeSubscriptionByRecord(ctx, rec)
}

// purgeSubscriptionByRecord is purgeSubscription given a record the caller
// already loaded. Use this whenever the record is in hand — it removes the
// re-read window entirely.
func (s *Server) purgeSubscriptionByRecord(ctx context.Context, rec *subscriptionRecord) {
	s.purgeSubscriptionOutcome(ctx, rec)
}

// purgeNotStarted 包住「还没开始回收」就失败的情况（拿不到订阅锁、读不到订阅）。
// 此时订阅没被停用、节点账号一个都没动：登记「回收后删除」待办没有意义 ——
// 待办执行时看到订阅仍生效，会直接作废它（2026-11-06）。
type purgeNotStarted struct{ err error }

func (e purgeNotStarted) Error() string { return e.err.Error() }
func (e purgeNotStarted) Unwrap() error { return e.err }

// purgeDeleteDBError 标识节点账号已全部回收完毕，但最终删除数据库行时发生的存储故障。
// 调用方据此走 500 脱敏错误且不再误登记「回收重试」待办。
type purgeDeleteDBError struct{ err error }

func (e purgeDeleteDBError) Error() string { return "删除订阅记录失败" }
func (e purgeDeleteDBError) Unwrap() error { return e.err }

// purgeSubscriptionOutcome 与 purgeSubscriptionByRecord 相同，但把结果交给调用方：
// 删除接口要据此如实告诉运营「行和节点账号还在」，不能一律回 200。
// 返回 nil 表示已删除或无需删除（已续费）；否则是失败原因。
// errRenewedDuringPurge：等锁期间订阅被续费、重新生效，清理放弃（2026-11-06）。
var errRenewedDuringPurge = errors.New("订阅在删除前被续费，已重新生效")

func (s *Server) purgeSubscriptionOutcome(ctx context.Context, rec *subscriptionRecord) error {
	if rec == nil {
		return nil
	}
	wasExpired := rec.expired(time.Now())
	lockedCtx, release, err := s.lockSubscription(ctx, rec.ID)
	if err != nil {
		s.logger.Warn("purge lock busy", "error", err)
		return purgeNotStarted{err}
	}
	defer release()
	ctx = lockedCtx
	current, err := s.loadSubscriptionByID(ctx, rec.ID)
	if err != nil {
		return purgeNotStarted{err}
	}
	if wasExpired && !current.expired(time.Now()) {
		// 2026-11-06：拿锁前它已到期、拿到锁时已被续费 —— 没删。原来返回 nil：删除接口回 200「已删除」、
		// 审计记 delete_subscription，sweeper 也把它计进 purged，订阅其实原样生效。
		return purgeNotStarted{errRenewedDuringPurge}
	}
	*rec = *current

	if _, err := s.db.Exec(ctx, `UPDATE subscription_tokens SET enabled=FALSE, active_ips='[]'::jsonb WHERE id=$1::uuid`, rec.ID); err != nil {
		s.logger.Error("subscription: purge could not disable row; proceeding with node revocation",
			"subscription", shortID(rec.ID), "error", err)
	}
	outcome := s.revokeSubscriptionAccounts(ctx, rec)
	if !outcome.OK() {
		summary := make([]string, 0, len(outcome.Failed))
		for _, failure := range outcome.Failed {
			summary = append(summary, fmt.Sprintf("node=%s user=%s: %s", failure.NodeID, failure.UserID, failure.Reason))
		}
		// 退避中的节点每轮都会出现在 Failed 里：只在真的拨过号时才记 ERROR，否则降为 Debug。
		if outcome.Revoked > 0 || !allBackedOff(outcome) {
			s.logger.Error("subscription: purge retained row and node-account mapping after incomplete revoke",
				"subscription", shortID(rec.ID), "revoked", outcome.Revoked, "failed", summary)
		}
		return fmt.Errorf("部分节点账号未能回收（%s），记录已保留并停用，节点恢复后会自动补删", strings.Join(summary, "；"))
	}
	// 节点账号已全部确认回收，删除订阅行与辅助状态表使用脱离请求取消的独立短预算，防止客户端断开残留空壳。
	delCtx, stopDel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopDel()
	if _, err := s.db.Exec(delCtx, `DELETE FROM subscription_tokens WHERE id=$1::uuid`, rec.ID); err != nil {
		s.logger.Warn("subscription: purge failed", "error", err)
		return purgeDeleteDBError{err}
	}
	// ⚠️ 2026-10-31：退避记录没有外键（故意：它得在订阅存在期间独立重试），
	// 所以删订阅不会级联清掉它。原来只有 closeExpiredSubscription 手动清理，
	// 走自动删除（auto_delete）的订阅会把这些行永久留在库里 ——
	// 一行一个 (订阅, 失败节点)，只增不减，且不会再有任何路径引用它。
	if _, err := s.db.Exec(delCtx, `DELETE FROM subscription_revoke_backoff WHERE subscription_id=$1::uuid`, rec.ID); err != nil {
		s.logger.Warn("subscription: 清理回收退避记录失败", "subscription", shortID(rec.ID), "error", err)
	}
	s.cancelPendingNodeAction(delCtx, rec.ID)
	return nil
}

func allBackedOff(o subscriptionRevokeOutcome) bool {
	for _, f := range o.Failed {
		if !strings.Contains(f.Reason, "退避中") {
			return false
		}
	}
	return len(o.Failed) > 0
}

// ---------------------------------------------------------------------------
// Expiry sweeper
// ---------------------------------------------------------------------------

// sweepExpiredSubscriptions disables lapsed subscriptions and removes the ones
// whose grace window has passed. It runs on a timer from the API process so the
// panel does not depend on an external cron; the read-time checks in the token
// endpoint remain the authoritative gate.
func (s *Server) sweepExpiredSubscriptions(ctx context.Context) {
	now := time.Now()
	// 1. Lapse: anything past its expiry stops serving immediately.
	//
	// ⚠️ 2026-10-04 修的关键缺口：**过期必须同时收回节点账号**，
	// 不能只把 enabled 置为 FALSE。
	//
	// 旧实现只跑一条 UPDATE，节点侧的账号（含 Reality UUID）原封不动 ——
	// 而真正删账号的 purge 带 `auto_delete=TRUE` 条件，于是
	// **auto_delete=false 的订阅过期后账号永久保留**：
	// 买家手里的 Reality 配置与 hy2 密码都还能用，属于「到期了但货还在」。
	//
	// 现在改成两步，语义上把「保留记录」与「保留账号」彻底解绑：
	//   (a) 先选出**刚过期且尚未收回**的行，收回它们的节点账号；
	//   (b) 再把它们标记为 enabled=FALSE。
	//
	// 顺序很重要：先收回、后标记。如果反过来，中间的标记动作会让这一行
	// 在下一步的筛选里消失（判据看 enabled），账号就再也收不回来了 ——
	// 那正是「静默泄漏」的成因。
	//
	// 用 node_accounts 是否为空作为「已收回」判据（收回成功时会把对应
	// 键从 jsonb 里删掉，见 revokeSubscriptionAccounts），因此这一步是
	// **幂等**的：反复跑不会重复调用节点、也不会误伤已收回的行。
	// 扫描所有已到期且仍挂着节点账号映射的订阅（不限 enabled=TRUE），防止保留型停用订阅遗留活账号；
	// 逐条回收前先获取订阅锁并重读权威状态，防止与并发续费竞争误删已续期账号。
	lapseRows, err := s.db.Query(ctx,
		`SELECT `+subscriptionColumns+` FROM subscription_tokens
		 WHERE deleted_at IS NULL
		   AND expires_at IS NOT NULL AND expires_at <= $1
		   AND node_accounts IS NOT NULL AND node_accounts <> '{}'::jsonb
		 ORDER BY enabled DESC, expires_at ASC`,
		now)
	if err != nil {
		s.logger.Warn("subscription sweeper: lapse select failed", "error", err)
	} else {
		var lapsed []*subscriptionRecord
		for lapseRows.Next() {
			rec, scanErr := scanSubscription(lapseRows)
			if scanErr != nil {
				s.logger.Warn("subscription sweeper: lapse scan failed", "error", scanErr)
				break
			}
			lapsed = append(lapsed, rec)
		}
		// 2026-11-03：迭代中断原来不检查（只有 Scan 失败会记日志）。读到的部分照常回收（各条互不依赖），
		// 没读到的下一轮再来 —— 但这次「少扫了」必须留痕。
		if err := lapseRows.Err(); err != nil {
			s.logger.Warn("subscription sweeper: lapse iteration incomplete; remaining rows retried next sweep", "read", len(lapsed), "error", err)
		}
		lapseRows.Close()
		for _, rec := range lapsed {
			lockedCtx, release, lockErr := s.lockSubscription(ctx, rec.ID)
			if lockErr != nil {
				continue
			}
			current, loadErr := s.loadSubscriptionByID(lockedCtx, rec.ID)
			if loadErr != nil || !current.expired(time.Now()) || len(current.nodeAccounts) == 0 {
				release()
				continue
			}
			outcome := s.revokeSubscriptionAccounts(lockedCtx, current)
			release()
			if !outcome.OK() {
				// 收回不完整：**保留行、保留映射**，留给下一轮重试。
				// 绝不能在这里标记 enabled=FALSE 后放手不管 ——
				// 那会让这行不再进入本查询，失败的部分永远不会被重试。
				summary := make([]string, 0, len(outcome.Failed))
				for _, f := range outcome.Failed {
					summary = append(summary, fmt.Sprintf("node=%s user=%s: %s",
						f.NodeID, f.UserID, f.Reason))
				}
				if outcome.Revoked > 0 || !allBackedOff(outcome) {
					s.logger.Error("subscription sweeper: lapse revoke incomplete; will retry",
						"subscription", shortID(rec.ID),
						"revoked", outcome.Revoked, "failed", summary)
				}
				continue
			}
			s.logger.Info("subscription sweeper: expired subscription revoked node accounts",
				"subscription", shortID(rec.ID), "revoked", outcome.Revoked)
		}
	}

	// (b) 标记过期。放在收回之后，避免上面那轮漏掉任何一行。
	if _, err := s.db.Exec(ctx, `UPDATE subscription_tokens SET enabled=FALSE, active_ips='[]'::jsonb WHERE deleted_at IS NULL AND enabled=TRUE AND expires_at IS NOT NULL AND expires_at <= $1 AND COALESCE(node_accounts,'{}'::jsonb)='{}'::jsonb`, now); err != nil {
		s.logger.Warn("subscription sweeper: lapse failed", "error", err)
	}
	// 2. Purge: auto_delete rows past expiry + grace are removed for good.
	//
	// Load the full rows here, not just the ids, and purge each from the record
	// we already hold. Two reasons, and the second one cost a real orphan:
	//
	//   - A bulk DELETE would never call revocation at all, orphaning the
	//     buyer's node-side accounts — alive on the node, under a password the
	//     buyer already holds. That is the leak this feature exists to close.
	//   - Selecting ids and then re-reading each one inside the purge reopens a
	//     window: if the row becomes invisible to loadSubscriptionByID between
	//     the two reads (deleted_at set concurrently by a second sweep or a
	//     read-time purge), the purge find nothing, revokes nothing, and its
	//     bare DELETE still "succeeds" while removing no row. The account then
	//     outlives its subscription silently. Holding the record closes that.
	//
	// The volume is tiny (expired trials) and revocations fan out concurrently,
	// so loading full rows costs nothing worth trading correctness for.
	// 2026-11-06：清理这一步出错只跳过它自己，不能 return —— 原来任何读取错误都直接返回，
	// 排在后面的第 3 步（退款 / 停用待办的补做）这一轮也不跑；清理查询持续出错时退款回收整个停摆，
	// 而买家账号在节点上一直能用。
	s.purgeExpiredAutoDelete(ctx, now)
	// 3. 退款/停用时没在节点上完成的动作，按退避补做。
	s.drainPendingNodeActions(ctx)
}

// purgeExpiredAutoDelete 是 sweeper 的第 2 步（过了宽限期的 auto_delete 订阅物理删除）。
func (s *Server) purgeExpiredAutoDelete(ctx context.Context, now time.Time) {
	rows, err := s.db.Query(ctx,
		`SELECT `+subscriptionColumns+` FROM subscription_tokens
		 WHERE deleted_at IS NULL AND auto_delete=TRUE
		   AND expires_at IS NOT NULL AND expires_at <= $1`,
		now.Add(-subscriptionExpiryGrace))
	if err != nil {
		s.logger.Warn("subscription sweeper: purge select failed", "error", err)
		return
	}
	var expired []*subscriptionRecord
	for rows.Next() {
		rec, err := scanSubscription(rows)
		if err != nil {
			rows.Close()
			s.logger.Warn("subscription sweeper: purge scan failed", "error", err)
			return
		}
		expired = append(expired, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.Warn("subscription sweeper: purge rows failed", "error", err)
		return
	}
	// 只统计真的删掉的：节点回收没完成的会保留，原来也算进 count，
	// 日志每 2 分钟报一次「purged 1」，实际什么都没删。
	purged := 0
	for _, rec := range expired {
		if s.purgeSubscriptionOutcome(ctx, rec) == nil {
			purged++
		}
	}
	if purged > 0 {
		s.logger.Info("subscription sweeper: purged", "count", purged, "pending", len(expired)-purged)
	}
}

// StartSubscriptionSweeper runs the expiry sweep on an interval until ctx ends.
func (s *Server) StartSubscriptionSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	runOnce := func() {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		s.sweepExpiredSubscriptions(rctx)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// One pass at boot so a restart applies lapses immediately.
		runOnce()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Operator-facing management
// ---------------------------------------------------------------------------

// GET /api/subscriptions — the operator's list of every subscription.
func (s *Server) listSubscriptionsHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	records, err := s.listSubscriptions(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	now := time.Now()

	// 一次性把全部订阅的用量读出来（2026-10-02 起）。
	//
	// 批量读而不是逐条查：这是"列表页"接口，N+1 查询会随订阅数线性增长。
	// 用量由采集器定期落库（见 collector/usage.go），这里只读投影。
	usageBySub := s.loadSubscriptionUsage(r.Context(), records)

	// 真实在线 IP（节点侧 ip_tracker）。读不到不该让整页失败 ——
	// 它是补充信息，而用量/到期这些主字段已经拿到了。
	onlineIPsBySub := map[string][]map[string]any{}
	onlineIPsReadable := true
	if ips, err := s.loadSubscriptionOnlineIPs(r.Context(), records); err != nil {
		s.logger.Warn("subscription-center: 读取在线 IP 失败", "error", err)
		onlineIPsReadable = false
	} else {
		onlineIPsBySub = ips
	}

	currentOnline, currentOnlineErr := s.loadCurrentOnlineIPs(r.Context(), records)
	if currentOnlineErr != nil {
		s.logger.Warn("subscription-center: current online IP read failed", "error", currentOnlineErr)
	}

	// 分类 id → 名称。一次查完：每条订阅都要显示分类名，
	// 逐条查会变成 N+1；而且分类是低频变动的小表，全量读最省事。
	categoryNameByID := map[string]string{}
	if cats, err := s.listCategories(r.Context()); err == nil {
		for _, c := range cats {
			categoryNameByID[c.ID] = c.Name
		}
	}

	items := make([]map[string]any, 0, len(records))
	for i := range records {
		rec := &records[i]
		// 在线 IP 明细。
		//
		// 管理员要求"列表里直接列 IP" —— 所以在列表接口就把明细带出去，
		// 而不是让前端为每条订阅再发一次请求。
		// 这里用已解析过的（parseActiveIPs 会剔除滑出窗口的条目），
		// 与 active_ip_count 同源，不会出现"个数 3 但列了 5 个 IP"。
		liveIPs := parseActiveIPs(rec.rawIPs, now)
		ipList := make([]map[string]any, 0, len(liveIPs))
		for _, e := range liveIPs {
			ipList = append(ipList, map[string]any{
				"ip":      e.IP,
				"seen_at": e.SeenAt,
			})
		}

		current := currentOnline[rec.ID]
		if current == nil {
			current = &currentOnlineSummary{IPs: []map[string]any{}, Errors: []string{}, Expected: len(rec.nodeAccounts)}
		}
		if currentOnlineErr != nil {
			current.IPs = []map[string]any{}
			current.Hy2Nodes = 0
			current.RealityNodes = 0
			current.Errors = append(current.Errors, "当前在线IP读取失败")
		}
		use := usageBySub[rec.ID]
		// ⚠️ 必须给空数组而不是 nil slice：Go 的 nil slice 序列化成
		// JSON `null`，前端 `item.online_ips.length` 会直接抛异常 ——
		// 而且是"没有在线 IP"这种最常见的正常状态会触发。
		onlineIPs := onlineIPsBySub[rec.ID]
		if onlineIPs == nil {
			onlineIPs = []map[string]any{}
		}
		items = append(items, map[string]any{
			"id":              rec.ID,
			"token":           rec.Token,
			"label":           rec.Label,
			"enabled":         rec.Enabled && !rec.expired(now),
			"expired":         rec.expired(now),
			"expires_at":      epochOrZero(rec.ExpiresAt),
			"expires_at_text": formatExpiryText(rec.ExpiresAt),
			"days_left":       rec.daysLeft(now),
			"ip_limit":        rec.IPLimit,
			"active_ip_count": len(ipList),
			"active_ips":      ipList,
			// 真实在线 IP：节点侧鉴权时记录的 ip_tracker，180 秒窗。
			// 与上面 active_ips（拉取过订阅链接，24h 窗）语义不同，两者都给。
			// 空数组 = 这一刻真没人连；读不到时也是空数组，靠 online_ips_readable 区分。
			"online_ips":      onlineIPs,
			"online_ip_count": len(onlineIPs),
			// 读表失败时为 false —— 前端据此显示"未知"而不是"0 个在线"。
			// 这正是本项目反复要避开的坑：把"没采到"显示成"确实是 0"。
			"online_ips_readable":           onlineIPsReadable && usageBySub[rec.ID].CurrentHealthy,
			"hysteria_online_devices":       usageBySub[rec.ID].HysteriaOnlineDevices,
			"online_count_nodes":            usageBySub[rec.ID].OnlineCountNodes,
			"online_count_scope":            "hysteria_core_instances_only",
			"online_ip_scope":               "recent_authentication",
			"current_online_ips":            current.IPs,
			"current_online_ips_readable":   current.Hy2Nodes > 0 || current.RealityNodes > 0,
			"current_online_ips_complete":   current.Expected > 0 && current.Hy2Nodes == current.Expected && current.RealityNodes == current.Expected,
			"current_online_hy2_nodes":      current.Hy2Nodes,
			"current_online_reality_nodes":  current.RealityNodes,
			"current_online_expected_nodes": current.Expected,
			"current_online_errors":         current.Errors,
			"current_online_sampled_at":     current.SampledAt,
			// 分类圈定：这条订阅只含哪个分类的节点。NULL = 不圈定（全部分类）。
			//
			// ⚠️ 必须把 category_id **和** category_name 都给出：
			// 前端要显示「AI 专用」而不是一串 uuid；只给 id 的话
			// 前端还得再拉一次分类列表去对照。
			"category_id":      rec.CategoryID,
			"node_ids":         rec.NodeIDs,
			"category_name":    categoryNameOf(categoryNameByID, rec.CategoryID),
			"auto_delete":      rec.AutoDelete,
			"order_id":         rec.OrderID,
			"user_email":       rec.UserEmail,
			"note":             rec.Note,
			"created_at":       rec.CreatedAt,
			"last_accessed_at": rec.LastAccessedAt,
			"access_count":     rec.AccessCount,
			"delivery_url":     subscriptionBaseURL(r) + "/user/" + rec.Token,
			"subscription_url": subLink(subscriptionBaseURL(r), rec.Token, ""),
			// 用量（按节点聚合成订阅级）。
			// used_bytes 为 null 表示**从未采到过**，与 0 不同 ——
			// 前端必须能区分"用量确实是 0"与"没采到"。
			"used_bytes":              use.UsedBytes,
			"limit_bytes":             use.LimitBytes,
			"usage_ok":                use.OK,
			"usage_current_ok":        use.CurrentHealthy,
			"usage_history_gaps":      use.HistoricalGaps,
			"usage_reason":            use.Reason,
			"usage_sampled_at":        use.SampledAt,
			"usage_scope":             "since_measurement_started",
			"usage_started_at":        use.StartedAt,
			"usage_started_at_latest": use.StartedAtLatest,
			"usage_expected_nodes":    use.ExpectedNodes,
			"usage_measured_nodes":    use.MeasuredNodes,
			// 两段的拆分值。used_bytes 已经是它们的和，
			// 这两个字段只为让前端能写清"含10-07 前历史 XXX"，
			// 而不是让用户对着一个来路不明的总数猜。
			"usage_legacy_bytes":  use.LegacyBytes,
			"usage_metered_bytes": use.MeasuredBytes,
		})
	}
	secret, err := s.storeSecret(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}

	// 订阅覆盖的节点数。
	//
	// 为什么补这个字段：前端原来有一张「覆盖节点」指标卡，值恒为 `—`
	// （因为这里不返回节点数），脚注却写着"每条订阅都包含全部可用节点" ——
	// 占着黄金位置却零信息量。
	//
	// 语义要说准：订阅**包含全部在役节点**（订阅是按节点实时生成的，
	// 见 subscription.go），所以这不是"某条订阅的节点数"，
	// 而是"当前所有订阅共享的节点池大小"。两条计数给出去：
	//   node_count       —— 在役节点数（就是订阅实际覆盖的范围）
	//   node_in_store    —— 其中进了商城、会推给买家的（in_store = true）
	// 只有前者会让运维误以为"有 5 个节点可卖"，而实际可能只有 3 个上架。
	var nodeCount, nodeInStore int
	fleetErr := ""
	if err := s.db.QueryRow(r.Context(), `
SELECT count(*)::int,
       count(*) FILTER (WHERE n.in_store)::int
FROM hysteria_nodes n
JOIN servers s ON s.id = n.server_id
WHERE n.deleted_at IS NULL AND s.deleted_at IS NULL`).Scan(&nodeCount, &nodeInStore); err != nil {
		// 读不到不该让整页失败：订阅列表本身已经拿到了。
		// 2026-11-06：但要告诉前端「没读到」—— 原来回 0，节点池卡片显示「0 个节点」，像是全部节点都没了。
		s.logger.Warn("subscription-center: 读取节点数失败", "error", err)
		fleetErr = "节点数读取失败"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"subscriptions": items,
		"fleet": map[string]any{
			"node_count":    nodeCount,
			"node_in_store": nodeInStore,
			"error":         fleetErr,
		},
		"store": map[string]any{
			"enabled":            secret != "",
			"key_hint":           maskSecret(secret),
			"create_endpoint":    "POST /api/store/orders",
			"renew_endpoint":     "POST /api/store/renew",
			"revoke_endpoint":    "POST /api/store/revoke",
			"query_endpoint":     "GET /api/store/orders?order_id=…",
			"auth_header":        "X-Store-Key: <对接密钥>",
			"ip_window_hours":    int(subscriptionIPWindow.Hours()),
			"expiry_grace_hours": int(subscriptionExpiryGrace.Hours()),
		},
	})
}

// POST /api/subscriptions — the operator mints a subscription by hand.
func (s *Server) createSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Label        string `json:"label"`
		DurationDays int    `json:"duration_days"`
		ExpiresAt    int64  `json:"expires_at"`
		IPLimit      int    `json:"ip_limit"`
		AutoDelete   *bool  `json:"auto_delete"`
		UserEmail    string `json:"user_email"`
		Note         string `json:"note"`
		// 分类圈定。空 = 不圈定（全部分类）。
		CategoryID string `json:"category_id"`
		// 直接圈定节点（多选）。非空时忽略 category_id。
		NodeIDs []string `json:"node_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}

	// 分类必须存在才允许挂上 —— 否则订阅会指向一个不存在的分类，
	// 生成时解析范围失败、按空集合处理，客户直接拿到 0 个节点。
	var categoryID *string
	if cid := strings.TrimSpace(input.CategoryID); cid != "" {
		ok, err := s.categoryExists(r.Context(), cid)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "指定的分类不存在"})
			return
		}
		categoryID = &cid
	}
	nodeIDs, err := s.validateSubscriptionNodeIDs(r.Context(), input.NodeIDs)
	if err != nil {
		var lookup nodeIDsLookupError
		if errors.As(err, &lookup) {
			s.databaseError(w, r, lookup.err)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(nodeIDs) > 0 {
		categoryID = nil
	}
	if input.DurationDays < 0 || input.DurationDays > 3650 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 需在 0-3650 之间"})
		return
	}
	if input.ExpiresAt < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires_at 不合法"})
		return
	}
	if input.IPLimit < 0 || input.IPLimit > 1000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_limit 需在 0-1000 之间"})
		return
	}
	var expiresAt *time.Time
	if input.ExpiresAt > 0 {
		t := time.Unix(input.ExpiresAt, 0)
		expiresAt = &t
	} else if input.DurationDays > 0 {
		t := time.Now().AddDate(0, 0, input.DurationDays)
		expiresAt = &t
	}
	label := strings.TrimSpace(input.Label)
	if label == "" {
		label = "新建订阅"
	}
	if utf8.RuneCountInString(label) > 64 { // 按字符数：22 个汉字就有 66 字节
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "标签最长 64 个字符"})
		return
	}
	autoDelete := true
	if input.AutoDelete != nil {
		autoDelete = *input.AutoDelete
	}
	note := truncateRunes(strings.TrimSpace(input.Note), 256)
	token, err := newSubscriptionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成订阅令牌失败"})
		return
	}
	var createdBy any
	if claims.Subject != "" {
		createdBy = claims.Subject
	}
	row := s.db.QueryRow(r.Context(), `
INSERT INTO subscription_tokens (token, label, enabled, expires_at, ip_limit, auto_delete, created_by, user_email, note, category_id, node_ids)
VALUES ($1, $2, TRUE, $3, $4, $5, $6, NULLIF($7,''), $8, $9::uuid, NULLIF($10::uuid[], '{}'))
RETURNING `+subscriptionColumns,
		token, label, expiresAt, input.IPLimit, autoDelete, createdBy, strings.TrimSpace(input.UserEmail), note, categoryID, nodeIDs)
	rec, err := scanSubscription(row)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_subscription", "subscription_token", rec.ID,
		map[string]any{"label": label, "days": input.DurationDays, "ip_limit": input.IPLimit,
			"category_id": input.CategoryID, "node_ids": nodeIDs})

	// 开户（2026-10-04 修的真 BUG）。
	//
	// 此前这里**只插了一行 DB 记录就返回**，完全没在节点上开户 ——
	// 而商城下单路径（store.go:691）是**会**调 provisionAndPersist 的。
	// 两条路径的不对称造成一个很隐蔽的故障：
	//
	//   手工开的订阅，客户导入链接后连不上 ——
	//   因为节点侧压根没有这个账号（订阅链接本身是有效的，
	//   它只是把所有节点拼在一起，节点缺账号就认证失败）。
	//
	// 实测复现（2026-10-04 端到端演练）：手工建订阅后
	// subscription_tokens.node_accounts 为空（0 个节点），
	// 而对账（reconcile）能刷出 4 个 —— 说明"拉取订阅"这条链是通的，
	// 只是开户这一步从来没被触发。
	//
	// ⚠️ 必须放在**写响应之前**：provisionAndPersist 会更新 rec.nodeAccounts，
	// 而响应里要如实报告开成了几个节点 —— 否则运维看到一个成功的响应，
	// 却不知道客户其实连不上。
	s.provisionAndPersist(r.Context(), rec)

	// ⚠️ 2026-10-30：缺口必须按**这条订阅真正能看到的节点范围**算，不能拿全站节点数比。
	//
	// 原来拿 len(rec.nodeAccounts) 与"全站可开户节点数"比较。分类订阅
	// （category_id 圈定）与按节点圈定（node_ids）的订阅天生只在一部分节点上
	// 开户：全站 8 台、这条订阅只圈 2 台且都开成了，也会被判成"有缺口"，
	// 于是对着一次完全成功的交付回一段"这些节点上的客户端会认证失败"。
	// 反过来，可开户节点数读库失败时 provisionableNodeCount 返回 0，
	// 真实缺口会被静默跳过 —— 两种错法方向相反，都是判据选错了数据源。
	report, reportErr := scopedProvisionReport(r.Context(), s, rec)
	payload := map[string]any{
		"id":                rec.ID,
		"token":             rec.Token,
		"label":             rec.Label,
		"expires_at":        epochOrZero(rec.ExpiresAt),
		"expires_at_text":   formatExpiryText(rec.ExpiresAt),
		"ip_limit":          rec.IPLimit,
		"category_id":       rec.CategoryID,
		"node_ids":          rec.NodeIDs,
		"provisioned_nodes": report.Provisioned,
		"expected_nodes":    report.Expected,
		"excluded":          report.Gaps,
		"delivery_url":      subscriptionBaseURL(r) + "/user/" + rec.Token,
		"subscription_url":  subLink(subscriptionBaseURL(r), rec.Token, ""),
	}
	if reportErr != nil {
		// 「不知道」也必须说出来：此时既不能报"全部开好"，也不能报"全部失败"。
		payload["provision_status"] = "unverified"
		payload["provision_note"] = "读取节点范围失败，无法判断每个节点是否已开户。请打开「账号对账」复查后再交付。"
	} else if len(report.Gaps) > 0 {
		payload["provision_status"] = "partial"
	} else {
		payload["provision_status"] = "complete"
	}
	writeJSON(w, http.StatusOK, payload)
}

// PATCH /api/subscriptions/{id} — edit label, expiry, device limit or state.
func (s *Server) updateSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	rec, err := s.loadSubscriptionByID(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	var input struct {
		Label        *string `json:"label"`
		Enabled      *bool   `json:"enabled"`
		DurationDays *int    `json:"duration_days"`
		ExpiresAt    *int64  `json:"expires_at"`
		IPLimit      *int    `json:"ip_limit"`
		AutoDelete   *bool   `json:"auto_delete"`
		UserEmail    *string `json:"user_email"`
		Note         *string `json:"note"`
		ResetIPs     bool    `json:"reset_ips"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	lockedReq, releaseLock, ok := s.lockSubscriptionRequest(w, r, rec)
	if !ok {
		return
	}
	defer releaseLock()
	r = lockedReq
	label := rec.Label
	if input.Label != nil {
		label = strings.TrimSpace(*input.Label)
		if label == "" {
			label = "订阅"
		}
		if utf8.RuneCountInString(label) > 64 { // 按字符数：22 个汉字就有 66 字节
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "标签最长 64 个字符"})
			return
		}
	}
	enabled := rec.Enabled
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	ipLimit := rec.IPLimit
	if input.IPLimit != nil {
		if *input.IPLimit < 0 || *input.IPLimit > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_limit 需在 0-1000 之间"})
			return
		}
		ipLimit = *input.IPLimit
	}
	autoDelete := rec.AutoDelete
	if input.AutoDelete != nil {
		autoDelete = *input.AutoDelete
	}
	email := rec.UserEmail
	if input.UserEmail != nil {
		email = strings.TrimSpace(*input.UserEmail)
	}
	note := rec.Note
	if input.Note != nil {
		note = truncateRunes(strings.TrimSpace(*input.Note), 256)
	}
	expiresAt := rec.ExpiresAt
	switch {
	case input.ExpiresAt != nil:
		if *input.ExpiresAt <= 0 {
			expiresAt = nil // explicit 0 clears the expiry → perpetual
		} else {
			t := time.Unix(*input.ExpiresAt, 0)
			expiresAt = &t
		}
	case input.DurationDays != nil && *input.DurationDays > 0:
		if *input.DurationDays > 3650 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duration_days 需在 1-3650 之间"})
			return
		}
		base := time.Now()
		if expiresAt != nil && expiresAt.After(base) {
			base = *expiresAt
		}
		t := base.AddDate(0, 0, *input.DurationDays)
		expiresAt = &t
	}
	resetIPs := input.ResetIPs || ipLimit != rec.IPLimit
	tag, err := s.db.Exec(r.Context(), `
UPDATE subscription_tokens
SET label=$1, enabled=$2, expires_at=$3, ip_limit=$4, auto_delete=$5,
    user_email=NULLIF($6,''), note=$7,
    active_ips=CASE WHEN $8 THEN '[]'::jsonb ELSE active_ips END
WHERE id=$9::uuid AND deleted_at IS NULL`,
		label, enabled, expiresAt, ipLimit, autoDelete, email, note, resetIPs, rec.ID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在或刚被删除"})
		return
	}
	s.audit(r.Context(), r, "update_subscription_record", "subscription_token", rec.ID,
		map[string]any{"enabled": enabled, "ip_limit": ipLimit, "label": label})

	// 「停用/启用订阅」联动节点账号状态（2026-10-03）。
	//
	// 只在 enabled **真的发生变化**时才动节点。理由：这个接口也用于改标签、
	// 改设备上限等日常编辑，每次都去逐个节点发请求既慢又无意义，
	// 还会白白消耗节点的写配额。
	//
	// 为什么必须联动：客户端每次连接都要过节点的鉴权回调重新认证，
	// 而节点账号的 status 与订阅的 enabled 原来是两套独立的状态。
	// 结果是"面板显示已停用、客户照常能用"——实测四个节点全部仍连通。
	var stateMsg string
	var stateOutcome subscriptionStateOutcome
	// notice 是「保存成功但操作者必须知道」的提示（2026-11-12）。编辑弹窗只在联动失败时才展示 node_state_message，
	// 成功时只说「已保存」，所以这类提示单独给一个字段；同时也拼进 stateMsg，启用/停用按钮那条路径照常能看到。
	var notice string
	// 2026-11-04：按「值真的变了」判断，不按「字段带了没有」。编辑弹窗每次保存都带着原样的 ip_limit：
	// 只改个标签也会逐节点 GET + update，一台节点不通就报「部分节点启用失败」并排一个后台同步 ——
	// 而这次编辑本来就不该碰节点。
	enabledChanged := enabled != rec.Enabled
	expiryChanged := !sameExpiry(expiresAt, rec.ExpiresAt)
	ipLimitChanged := ipLimit != rec.IPLimit
	if enabledChanged || expiryChanged || ipLimitChanged {
		// 改动前是否生效（下面判断「重新生效」要用，必须在覆盖 rec 之前取）。
		wasActive := rec.Enabled && !rec.expired(time.Now())
		rec.Enabled, rec.ExpiresAt, rec.IPLimit = enabled, expiresAt, ipLimit
		if expiryChanged || ipLimitChanged {
			stateOutcome = s.syncSubscriptionNodeAttributes(r.Context(), rec)
		} else {
			stateOutcome = s.applySubscriptionNodeStatus(r.Context(), rec, enabled && !rec.expired(time.Now()))
		}
		active := enabled && !rec.expired(time.Now())
		// 文案按节点实际被置成的状态写：到期订阅点「启用」时节点账号仍是停用，
		// 用 enabled 会报「已在 N 个节点上启用」而实际是停用（2026-11-03）。
		stateMsg = describeSubscriptionStateOutcome(stateOutcome, active)
		syncQueued := false
		if active {
			// 重新启用或续期：之前排队的停用/回收必须作废。
			s.cancelPendingNodeAction(r.Context(), rec.ID)
			// 2026-11-12：订阅重新生效（到期后续期、停用后启用），或生效但一个节点账号都没有时，排一次后台同步。
			//
			// 到期清扫会删掉节点账号、清空映射。映射为空时上面的节点联动没有任何对象可切，结果是「全部成功」，
			// 原来于是既不开户也不排同步：界面报「已启用」，节点上却没有这个买家的账号，要等他的客户端重新拉订阅
			// 或整点兜底补开户（最长约 1 小时）才恢复。商城续费与手工创建都是当场开户，只有这条路径没有。
			// 同步逐节点核对：账号不存在就按确定性口令创建，已存在就只推到期与网络数，不换密码。
			// 用不带取消的 ctx：登记只是一次写库，不能因为浏览器先断开就丢掉。
			if !wasActive || len(rec.nodeAccounts) == 0 {
				if err := s.queueSubscriptionSync(context.WithoutCancel(r.Context()), rec.ID); err != nil {
					s.logger.Warn("subscription: 重新生效后登记后台同步失败", "subscription", shortID(rec.ID), "error", err)
					notice = "后台同步登记失败，请稍后在订阅中心点「账号对账」"
				} else {
					syncQueued = true
					notice = "已登记后台同步：会核对各节点账号并补开缺少的（约半分钟）"
				}
			}
		} else if !enabled && expiryChanged && !rec.expired(time.Now()) {
			// 2026-11-12：续期了但订阅仍是停用（到期清扫或运营停用过）。原来只回「已保存」，没有任何提示，
			// 运营以为续上了、买家却连不上。不在这里自动启用：库里分不清是清扫置的停用还是运营手工停的。
			notice = "订阅仍处于停用状态，需要点「启用」后买家才能使用"
		}
		if notice != "" {
			stateMsg += "。" + notice
		}
		if !stateOutcome.allOK() && stateOutcome.onlyGone() {
			// 2026-11-06：失败的只有「映射还挂着、节点记录已删除」的残留。如实说出来，但不排后台重试 ——
			// 那台机器已经不在了，重试按指数退避永远跑下去，而订阅永远显示「部分失败」。
			stateMsg += "。其中的节点记录已删除（映射是残留，可在节点页清理），不需要后台重试"
		} else if !stateOutcome.allOK() {
			s.logger.Warn("subscription: 节点状态联动不完整",
				"subscription", rec.ID, "enabled", enabled, "失败节点", stateOutcome.Failed)
			if !active {
				if err := s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionDisable, false, "后台停用时部分节点未确认"); err != nil {
					stateMsg += "。后台自动重试登记失败，请稍后重新停用一次"
				} else {
					stateMsg += "。已登记后台自动重试，节点恢复后会自动停用"
				}
			} else {
				// 2026-11-04：仍生效时（续期、改设备数、重新启用）也要排后台同步。原来只记日志：
				// 每小时补开户只补「缺账号」，不会补「到期时间没推过去」—— 那台节点上的账号仍按旧日期到期，
				// 买家到时候就连不上，没有任何自动恢复。同步任务会逐节点推到期与设备数并核对，失败带退避重试。
				// 用不带取消的 ctx：走到这里往往正因为客户端先断开了（节点多、超时），
				// 用已取消的 ctx 登记重试会直接失败 —— 而这是唯一会补推到期时间的地方。
				if syncQueued {
					// 上面「重新生效」已经登记过同一条同步，文案也已经说明，不重复登记。
				} else if err := s.queueSubscriptionSync(context.WithoutCancel(r.Context()), rec.ID); err != nil {
					stateMsg += "。后台同步登记失败，请稍后在订阅中心点「账号对账」"
				} else {
					stateMsg += "。已登记后台同步，节点恢复后会自动补上"
				}
			}
		}
	}

	updated, err := s.loadSubscriptionByID(r.Context(), rec.ID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              updated.ID,
		"token":           updated.Token,
		"label":           updated.Label,
		"enabled":         updated.Enabled && !updated.expired(now),
		"expires_at":      epochOrZero(updated.ExpiresAt),
		"expires_at_text": formatExpiryText(updated.ExpiresAt),
		"ip_limit":        updated.IPLimit,
		"active_ip_count": updated.ActiveIPCount,
		"auto_delete":     updated.AutoDelete,
		"note":            updated.Note,
		"user_email":      updated.UserEmail,
		// 联动结果必须回给前端。**有节点没切成功时一定要说出来** ——
		// 只回"已停用"而实际有节点没停掉，就是谎报（铁律①）。
		"node_state_message": stateMsg,
		"node_state_ok":      stateMsg == "" || stateOutcome.allOK(),
		"node_state_failed":  stateOutcome.Failed,
		"notice":             notice,
	})
}

// sameExpiry 比较两个可空的到期时间（秒级）：都为空（永久）或指向同一秒才算相同。
func sameExpiry(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Unix() == b.Unix()
}

// DELETE /api/subscriptions/{id} — remove a subscription outright.
//
// This is the operator deleting a subscription by hand from the panel, and it
// must revoke like every other reclaim path. A bare DELETE here removes the row
// that holds the node-side account ids, so the buyer's account outlives its
// subscription as an orphan — reachable with a password the buyer still has.
// That is precisely the leak this change set exists to close, and the operator
// button was the last path still doing it.
func (s *Server) deleteSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	rec, err := s.loadSubscriptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	// Revoke from the record we already hold, then delete. Same ordering as
	// purgeSubscription: an orphaned row is inert, an orphaned account is a leak.
	if err := s.purgeSubscriptionOutcome(r.Context(), rec); err != nil {
		// 2026-11-06：还没开始回收就失败（拿不到锁、读不到订阅）时如实回锁 / 数据库错误，不登记待办。
		// 原来照样登记「回收后删除」并回「后台会补删」：待办执行时看到订阅仍生效就直接作废 ——
		// 订阅照常可用、没被删，运营却以为后台在补删；错误原文里还带着数据库连接信息。
		var notStarted purgeNotStarted
		if errors.As(err, &notStarted) {
			if errors.Is(notStarted.err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
				return
			}
			if errors.Is(notStarted.err, errRenewedDuringPurge) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "订阅刚刚被续费，已重新生效，未删除；确需删除请刷新后再操作"})
				return
			}
			var dbErr subscriptionLockDBError
			if errors.Is(notStarted.err, errSubscriptionBusy) || errors.Is(notStarted.err, errSubscriptionLockSlots) || errors.As(notStarted.err, &dbErr) {
				s.writeSubscriptionLockError(w, r, notStarted.err)
				return
			}
			s.databaseError(w, r, notStarted.err)
			return
		}
		var delErr purgeDeleteDBError
		if errors.As(err, &delErr) {
			s.databaseError(w, r, delErr.err)
			return
		}
		// 2026-11-03：回收不完整时行已被置为停用，而 sweeper 只扫 enabled 或
		// auto_delete+已到期 的行 —— 不登记待办就没有任何路径会再去补删，
		// 那台节点上的账号永久可用，而错误文案却说「节点恢复后会自动补删」。
		queueErr := s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionRevoke, true, "手动删除订阅时部分节点回收失败")
		auditCtx, stopAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		s.audit(auditCtx, r, "delete_subscription_incomplete", "subscription_token", id, map[string]any{"error": err.Error(), "queued": queueErr == nil})
		stopAudit()
		msg := err.Error()
		if queueErr != nil {
			msg += "；后台重试登记失败，请重新删除一次"
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "pending": queueErr == nil, "error": msg})
		return
	}
	auditCtx, stopAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	s.audit(auditCtx, r, "delete_subscription", "subscription_token", id, nil)
	stopAudit()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rotateSubscriptionTokenByID rotates one live row without changing its terms
// or creation time. Both management endpoints use this same scoped write.
func (s *Server) rotateSubscriptionTokenByID(ctx context.Context, id string) (string, error) {
	token, err := newSubscriptionToken()
	if err != nil {
		return "", err
	}
	tag, err := s.db.Exec(ctx, `UPDATE subscription_tokens SET token=$1, last_accessed_at=NULL, access_count=0, active_ips='[]'::jsonb WHERE id=$2::uuid AND deleted_at IS NULL`, token, id)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", pgx.ErrNoRows
	}
	return token, nil
}

// POST /api/subscriptions/{id}/rotate — new token, same terms.
func (s *Server) rotateSubscriptionHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "换新订阅链接需要 X-Confirm-Dangerous: yes"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	token, err := s.rotateSubscriptionTokenByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "订阅不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "rotate_subscription_record", "subscription_token", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":            token,
		"subscription_url": subLink(subscriptionBaseURL(r), token, ""),
		"delivery_url":     subscriptionBaseURL(r) + "/user/" + token,
	})
}

// POST /api/subscriptions/store-key — mint or rotate the shop's shared secret.
func (s *Server) rotateStoreKeyHandler(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "重置对接密钥需要 X-Confirm-Dangerous: yes"})
		return
	}
	token, err := newSubscriptionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成密钥失败"})
		return
	}
	key := "shop_" + token
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '外部商城对接密钥')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, storeSecretKey, key); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "rotate_store_key", "system_setting", storeSecretKey, nil)
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "auth_header": "X-Store-Key: " + key})
}

// maskSecret shows only enough of a secret to recognise it, never enough to use
// it. An empty secret stays empty so the UI can distinguish "not configured".
func maskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	// 短密钥（<=16 字节）全部隐藏，避免前后截取把全部明文直接暴露出来。
	if len(secret) <= 16 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:6] + strings.Repeat("*", 8) + secret[len(secret)-4:]
}

// revokeNodeFromAllSubscriptions 在节点被删除时触发（下架只回收买家，见 revokeNodeFromBuyerSubscriptions）：
// 遍历所有已售出订阅（含该节点账号者），在该节点上物理删除买家账号 (/users/delete)，
// 并将该 nodeID 从各个买家的 subscription_tokens.node_accounts 中完全剔除。
//
// override 非 nil 时用它替代 loadPortalTarget 的查询结果。删除节点的调用方必须传
// 快照：删除动作本身已经把 deleted_at 置上、且按安全设计抹掉了 api_url/api_key，
// 而 loadPortalTarget 的 SQL 带 AND deleted_at IS NULL —— 删除之后再查必然得到
// errPortalNodeNotFound，于是 /users/delete 一个都发不出去，「删除节点」这条路
// 就变成了「账面回收、账号仍在节点上继续可用」。
//
// 返回值语义（这是刻意的）：只有当**每个**买家的账号都确认从节点侧删掉时，
// 才从 node_accounts 里剔除该节点映射。任何一个失败都保留映射并把 user_id 报给
// 调用方，让它重试或告警。凭据本身已从库里抹掉时无法重试，调用方必须把失败
// 显式回给操作者，绝不能记成成功。
func (s *Server) revokeNodeFromAllSubscriptions(ctx context.Context, nodeID string, override *portalTarget) revokeOutcome {
	return s.revokeNodeFromSubscriptions(ctx, nodeID, override, nil, false)
}

// revokeNodeFromBuyerSubscriptions 节点移出商城（下架）时用：只回收买家订阅（有 order_id）上的账号。
//
// 2026-11-04：原来下架也走 revokeNodeFromAllSubscriptions，把手工 / 自用订阅在这台节点上的账号一并删掉。
// 而「未上架」只意味着不卖给买家 —— 手工订阅照样可以用（applySubscriptionAccounts 与
// filterProvisionCandidates 都只对买家要求已上架），于是自用客户端立刻断开，
// 直到每小时的补开户再把账号开回来。
func (s *Server) revokeNodeFromBuyerSubscriptions(ctx context.Context, nodeID string) revokeOutcome {
	return s.revokeNodeFromSubscriptions(ctx, nodeID, nil, nil, true)
}

// revokeNodeFromSubscriptions 同上，但 onlyCategories 非空时只回收「按这些分类圈定、
// 且没有按节点直接圈定」的订阅上的账号（节点被移出分类时用）。
func (s *Server) revokeNodeFromSubscriptions(ctx context.Context, nodeID string, override *portalTarget, onlyCategories []string, buyersOnly bool) revokeOutcome {
	outcome := revokeOutcome{}
	rows, err := s.db.Query(ctx, `
		SELECT id, node_accounts
		FROM subscription_tokens
		WHERE deleted_at IS NULL AND node_accounts IS NOT NULL AND node_accounts ? $1
		  AND ($2::uuid[] IS NULL OR (category_id = ANY($2::uuid[]) AND COALESCE(cardinality(node_ids), 0) = 0))
		  AND (NOT $3::boolean OR COALESCE(order_id, '') <> '')
	`, nodeID, onlyCategories, buyersOnly)
	if err != nil {
		s.logger.Error("revokeNodeFromStore: 查询订阅失败", "node_id", nodeID, "error", err)
		outcome.Err = err
		return outcome
	}
	defer rows.Close()

	type subEntry struct {
		id     string
		userID string
	}
	var toRevoke []subEntry

	for rows.Next() {
		var id string
		var rawAccounts []byte
		if err := rows.Scan(&id, &rawAccounts); err != nil {
			rows.Close()
			outcome.Err = fmt.Errorf("读取订阅账号映射失败: %w", err)
			return outcome
		}
		accMap := parseNodeAccounts(rawAccounts)
		if acc, ok := accMap[nodeID]; ok && acc.UserID != "" {
			toRevoke = append(toRevoke, subEntry{id: id, userID: acc.UserID})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		outcome.Err = fmt.Errorf("读取订阅账号映射中断: %w", err)
		return outcome
	}

	if len(toRevoke) == 0 {
		return outcome
	}

	target, targetErr := override, error(nil)
	if target == nil {
		target, targetErr = s.loadPortalTarget(ctx, nodeID)
	}
	if targetErr != nil || target == nil {
		// 节点凭据不可用时立即汇总返回，避免对所有受影响订阅白跑 N+1 次加锁和查库。
		reason := targetErrText(targetErr)
		s.logger.Error("revokeNodeFromStore: 无法取得节点 API 凭据，账号未从节点侧删除",
			"node_id", nodeID, "affected", len(toRevoke), "error", targetErr)
		failed := make([]revokeFailure, 0, len(toRevoke))
		for _, entry := range toRevoke {
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: reason})
		}
		outcome.Failed = failed
		return outcome
	}

	var failed []revokeFailure
	for _, entry := range toRevoke {
		lockedCtx, release, lockErr := s.lockSubscription(ctx, entry.id)
		if lockErr != nil {
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: "订阅正在操作，回收未执行: " + lockErr.Error()})
			continue
		}
		var currentUserID string
		if err := s.db.QueryRow(lockedCtx, `SELECT COALESCE(node_accounts->$1->>'user_id','') FROM subscription_tokens WHERE id=$2::uuid AND deleted_at IS NULL`, nodeID, entry.id).Scan(&currentUserID); err != nil {
			release()
			if errors.Is(err, pgx.ErrNoRows) {
				// 订阅在取锁前已被删除，无需重复回收亦不应记作失败。
				continue
			}
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: "读取订阅账号映射失败，回收未执行"})
			continue
		} else if currentUserID == "" {
			release()
			continue
		} else if currentUserID != entry.userID {
			release()
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: "订阅账号映射已变化，回收未执行"})
			continue
		}
		callCtx, cancel := context.WithTimeout(lockedCtx, 10*time.Second)
		_, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodPost, "/users/delete", map[string]any{
			"user_id": entry.userID,
		})
		cancel()
		if err != nil && !isUserNotFound(err) {
			release()
			s.logger.Error("revokeNodeFromStore: 节点物理删除用户失败，保留映射以便重试",
				"node", target.NodeName, "user_id", entry.userID, "error", err)
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: err.Error()})
			continue
		}
		// 节点上已不存在该账号时，回收目标同样达成；按幂等成功继续清除本地映射。
		// 使用脱离请求取消的短超时写库，防止节点侧已删而调用方断开导致映射残留。
		persistCtx, stopPersist := context.WithTimeout(context.WithoutCancel(lockedCtx), 5*time.Second)
		_, persistErr := s.db.Exec(persistCtx, `UPDATE subscription_tokens SET node_accounts=node_accounts - $1 WHERE id=$2::uuid`, nodeID, entry.id)
		stopPersist()
		if persistErr != nil {
			release()
			s.logger.Error("revokeNodeFromStore: 更新订阅映射失败", "sub_id", entry.id, "error", persistErr)
			failed = append(failed, revokeFailure{UserID: entry.userID, SubID: entry.id, Reason: "写回映射失败: " + persistErr.Error()})
			continue
		}
		release()
		outcome.Revoked++
	}

	outcome.Failed = failed
	if len(failed) == 0 {
		s.logger.Info("revokeNodeFromStore: 已成功从买家订阅中移出该节点并注销账号",
			"node_id", nodeID, "revoked_count", outcome.Revoked)
	} else {
		s.logger.Error("revokeNodeFromStore: 部分账号未真正注销，映射已保留",
			"node_id", nodeID, "revoked_count", outcome.Revoked, "failed_count", len(failed))
	}
	return outcome
}

// revokeFailure 记录一个没能从节点侧删掉的买家账号。
type revokeFailure struct {
	UserID string
	SubID  string
	Reason string
}

// revokeOutcome 是回收操作对调用方的诚实汇报。
type revokeOutcome struct {
	Revoked int
	Failed  []revokeFailure
	Err     error
}

// Failed 汇总成一句可以直接展示给操作者的文案。
func (o revokeOutcome) FailedSummary() string {
	if len(o.Failed) == 0 {
		return ""
	}
	parts := make([]string, 0, len(o.Failed))
	for _, f := range o.Failed {
		parts = append(parts, fmt.Sprintf("%s(%s)", f.UserID, f.Reason))
	}
	return strings.Join(parts, "; ")
}

// OK 表示「可以告诉操作者：买家账号已经不在节点上了」。
func (o revokeOutcome) OK() bool {
	return o.Err == nil && len(o.Failed) == 0
}

func targetErrText(err error) string {
	if err == nil {
		return "节点 API 凭据不可用"
	}
	return err.Error()
}

// shopOrderIDPattern 是商城（external-store）节点订单号；控制台侧的 order_id
// 带 "OPS-" 前缀（见商城 node_service._ops_create_subscription）。
var shopOrderIDPattern = regexp.MustCompile(`^OPS-(prdord_[A-Za-z0-9_-]{8,128})$`)

// storeRenewBaseURL 从环境变量读取外部商城续费基础地址，未配置时返回空串。
func storeRenewBaseURL() string {
	raw := strings.TrimSpace(os.Getenv("STORE_RENEW_BASE_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	return raw
}

// subscriptionRenewURL 返回订阅页「前往续费」地址；未配置商城基础地址时返回空串。
func subscriptionRenewURL(rec *subscriptionRecord, shop string) string {
	shop = strings.TrimSpace(shop)
	if shop == "" {
		return ""
	}
	if rec == nil {
		return shop
	}
	m := shopOrderIDPattern.FindStringSubmatch(rec.OrderID)
	if m == nil || rec.Token == "" {
		return shop
	}
	return shop + "?renew=" + url.QueryEscape(m[1]) + "&sub=" + url.QueryEscape(rec.Token)
}

// validateSubscriptionNodeIDs 校验手工开通时多选的节点：去重、必须是存在且未删除的节点。
// 有任何一个不存在就整体拒绝 —— 宁可报错也不悄悄少发。
// nodeIDsLookupError：校验节点范围时读库失败（不是输入问题）。
type nodeIDsLookupError struct{ err error }

func (e nodeIDsLookupError) Error() string { return e.err.Error() }
func (e nodeIDsLookupError) Unwrap() error { return e.err }

func (s *Server) validateSubscriptionNodeIDs(ctx context.Context, raw []string) ([]string, error) {
	seen := map[string]bool{}
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		// 2026-11-04：先规范成小写再去重。原来按原样去重：同一节点的大小写两种写法都会进列表，
		// 而库里 count(*) 只数出一台 ⇒ 报「选中的节点里有已删除或不存在的」，请求被误拒。
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" || seen[id] {
			continue
		}
		if !uuidPattern.MatchString(id) {
			return nil, errors.New("节点 id 不合法")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ids, nil
	}
	if len(ids) > 500 {
		return nil, errors.New("一次最多选 500 个节点")
	}
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hysteria_nodes WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`, ids).Scan(&n); err != nil {
		// 2026-11-06：数据库错误单独包一层，调用方据此回 500 —— 原来与输入错误一样回 400，
		// 而且把 SQLSTATE / 连接信息原文放进了响应。
		return nil, nodeIDsLookupError{err}
	}
	if n != len(ids) {
		return nil, errors.New("选中的节点里有已删除或不存在的，请刷新后重选")
	}
	return ids, nil
}

// publicRevokeFailures 是回给商城的回收失败明细（2026-11-06）。
//
// Reason 里可能是面板自己的数据库错误原文（读退避状态、摘映射失败、取订阅锁）：SQLSTATE、表名、
// 连接地址都会出现在商城收到的响应里。这类原因换成一句固定文案，原文只留在审计与日志；
// 节点侧的报错（门户返回的消息、连接超时）照原样给，商城据此判断是哪台节点的问题。
func publicRevokeFailures(in []subscriptionRevokeFailure) []subscriptionRevokeFailure {
	out := make([]subscriptionRevokeFailure, len(in))
	for i, f := range in {
		out[i] = f
		if revokeReasonIsInternal(f.Reason) {
			out[i].Reason = "面板内部错误，已登记后台重试"
		}
	}
	return out
}

func revokeReasonIsInternal(reason string) bool {
	lower := strings.ToLower(reason)
	for _, marker := range []string{"sqlstate", "failed to connect", "database", "pgx", "读取回收退避状态失败", "conn closed", "订阅锁暂时不可用"} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}
