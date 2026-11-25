package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// devicelimit.go —— 跨节点统一设备（网络）上限（2026-10-30）。
//
// 节点自己的「同时在线网络」检查是各节点各算各的：同一订阅在东京用 3 个网络、
// 在香港再用 3 个……12 台节点理论上能同时用 36 个。分享就是靠这个漏洞。
//
// 这里把采集器写进 subscription_live_connections 的各节点在线 IP 合并去重，
// 按「先上线的先占名额」排序：前 ip_limit 个放行，其余是超额。
//
// 模式（system_settings.device_limit_mode）：
//   observe（默认）—— 只记录「会被拦截的」到审计日志，不拦截任何人；
//   enforce        —— 把超额 IP 下发到该订阅开了户的每台节点（节点门户 users/ip-deny，
//                     hysteria2-installer 4ca9ab6 起），10 分钟有效、超额期间每 4 分钟续期，
//                     回到名额内立即下发空列表解除；老门户没有该接口 ⇒ 跳过并记日志；
//   off            —— 完全不统计。
//
// 名额顺序（谁先上线）落在 subscription_ip_admissions：面板重启后沿用，
// 不会因为重启把已经在用的客户当成「新来的」挤掉。

const (
	deviceLimitModeKey      = "device_limit_mode"
	deviceLimitInterval     = 30 * time.Second
	deviceLimitOnlineWindow = 180 * time.Second // 与节点 IP_TIMEOUT_SECONDS 一致
	deviceLimitFreshness    = 90 * time.Second  // 采集结果多旧以内才算数
	deviceLimitRecordEvery  = time.Hour         // 同一订阅同一组超额 IP 多久记一次
	deviceLimitDenyTTL      = 600               // 下发到节点的拦截有效期（秒）
	deviceLimitRefresh      = 4 * time.Minute   // 超额期间多久续期一次
)

// deviceLimitRetryUnblock 标记「解除只送达了部分节点」：下一轮还要再发一次空列表。
// 它不是合法的 IP 列表，deviceLimitNeedsPush 看到它与空列表不同，必然重发。
const deviceLimitRetryUnblock = "retry-unblock"

type deviceLimitPush struct {
	ips string
	at  time.Time
}

type deviceLimitState struct {
	mu        sync.Mutex
	loaded    bool
	firstSeen map[string]map[string]time.Time
	lastSeen  map[string]map[string]time.Time
	recorded  map[string]time.Time
	pushed    map[string]deviceLimitPush
	blocked   map[string]int
	warned    map[string]time.Time
	// incomplete 记上一轮哪些订阅缺节点数据，只在状态变化时打日志
	//（原来每 30 秒每条订阅一条 WARN，日志轮转只能留 1–2 天）。
	incomplete map[string]bool
}

var deviceLimits = deviceLimitState{
	firstSeen:  map[string]map[string]time.Time{},
	lastSeen:   map[string]map[string]time.Time{},
	recorded:   map[string]time.Time{},
	pushed:     map[string]deviceLimitPush{},
	blocked:    map[string]int{},
	warned:     map[string]time.Time{},
	incomplete: map[string]bool{},
}

type deviceLimitOnline struct {
	IP       string
	LastSeen time.Time
	Nodes    []string
}

// deviceNetworkKey 把 IP 归到一个「网络」：IPv4 按 /24，IPv6 按 /64。
//
// 2026-11-06：设备上限原来按**精确 IP** 占名额。手机在 4G / Wi-Fi 之间切换、或重连 PPPoE 拿到新地址时，
// 180 秒窗口里会同时留下两个 IP —— ip_limit=1 的客户「较新的那个 IP」被判超额并被推 ip-deny，
// 只有一台设备也连不上。商店自己的拒绝文案写的是「同时在线的**网络数**已达上限」，语义本来就是网络。
// 取 /24 而不是 /16：同一运营商大区里有大量互不相干的用户，/16 会把限制放得太松。
func deviceNetworkKey(ip string) string {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return ip
	}
	if v4 := p.To4(); v4 != nil {
		return net.IP(v4.Mask(net.CIDRMask(24, 32))).String() + "/24"
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// deviceLimitVerdict 按「网络」分配名额，同一网络里的所有 IP 一起放行或一起拦。
// 返回的 allowed / extra 仍是具体 IP 列表（调用方要按 IP 下发 ip-deny）。
func deviceLimitVerdict(online []deviceLimitOnline, firstSeen map[string]time.Time, limit int) (allowed, extra []string) {
	type group struct {
		key   string
		first time.Time
		ips   []string
	}
	groups := make(map[string]*group, len(online))
	for _, entry := range online {
		key := deviceNetworkKey(entry.IP)
		g, ok := groups[key]
		if !ok {
			g = &group{key: key}
			groups[key] = g
		}
		seen := firstSeen[entry.IP]
		if g.first.IsZero() || (!seen.IsZero() && seen.Before(g.first)) {
			g.first = seen
		}
		g.ips = append(g.ips, entry.IP)
	}
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.ips)
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].first.Equal(ordered[j].first) {
			return ordered[i].first.Before(ordered[j].first)
		}
		return ordered[i].key < ordered[j].key
	})
	for i, g := range ordered {
		if limit <= 0 || i < limit {
			allowed = append(allowed, g.ips...)
		} else {
			extra = append(extra, g.ips...)
		}
	}
	return allowed, extra
}

func deviceLimitNeedsPush(prev deviceLimitPush, had bool, ips string, now time.Time) bool {
	if !had {
		return ips != ""
	}
	if prev.ips != ips {
		return true
	}
	return ips != "" && now.Sub(prev.at) >= deviceLimitRefresh
}

// deviceLimitUnion 合并上一轮已下发的拦截列表（逗号分隔）与本轮的超额 IP，去重排序。
func deviceLimitUnion(prev string, extra []string) []string {
	set := map[string]struct{}{}
	for _, ip := range extra {
		set[ip] = struct{}{}
	}
	for _, ip := range strings.Split(prev, ",") {
		if ip != "" && ip != deviceLimitRetryUnblock {
			set[ip] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func (s *Server) deviceLimitMode(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, deviceLimitModeKey).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "observe", nil
	}
	if err != nil {
		return "", err
	}
	switch mode := strings.TrimSpace(v); mode {
	case "observe", "enforce", "off":
		return mode, nil
	default:
		return "", errors.New("invalid device limit mode")
	}
}

func deviceLimitBlockedCount(subID string) int {
	deviceLimits.mu.Lock()
	defer deviceLimits.mu.Unlock()
	return deviceLimits.blocked[subID]
}

func (s *Server) StartDeviceLimitWatch(ctx context.Context) {
	go func() {
		if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS subscription_ip_admissions (
			subscription_id uuid NOT NULL, ip text NOT NULL, first_seen timestamptz NOT NULL,
			PRIMARY KEY (subscription_id, ip))`); err != nil {
			s.logger.Warn("device limit: admissions table unavailable, ordering resets on restart", "error", err)
		}
		ticker := time.NewTicker(deviceLimitInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
				mode, err := s.deviceLimitMode(runCtx)
				if err != nil {
					s.logger.Warn("device limit: mode load failed; preserving current enforcement", "error", err)
					cancel()
					continue
				}
				if mode == "off" {
					s.clearDeviceLimitEnforcement(runCtx)
					cancel()
					continue
				}
				s.checkDeviceLimits(runCtx, mode)
				cancel()
			}
		}
	}()
}

func (s *Server) clearDeviceLimitEnforcement(ctx context.Context) {
	deviceLimits.mu.Lock()
	jobs := make([]string, 0, len(deviceLimits.pushed))
	for subID, previous := range deviceLimits.pushed {
		if previous.ips != "" {
			jobs = append(jobs, subID)
		}
	}
	deviceLimits.mu.Unlock()

	for _, subID := range jobs {
		// 关闭模式要求每台节点都真的解除；只到一部分就保留记录，下一轮继续清。
		if _, complete := s.pushDeviceLimitDeny(ctx, subID, nil); complete {
			deviceLimits.mu.Lock()
			delete(deviceLimits.pushed, subID)
			delete(deviceLimits.blocked, subID)
			deviceLimits.mu.Unlock()
		}
	}
}

// queryDeviceLimitAdmissions 在锁外查询持久化名额记录，避免数据库延迟阻塞交付页并发读取。
func (s *Server) queryDeviceLimitAdmissions(ctx context.Context) (map[string]map[string]time.Time, map[string]map[string]time.Time, error) {
	rows, err := s.db.Query(ctx, `SELECT subscription_id::text, ip, first_seen FROM subscription_ip_admissions`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	now := time.Now()
	firstSeen := map[string]map[string]time.Time{}
	lastSeen := map[string]map[string]time.Time{}
	for rows.Next() {
		var sub, ip string
		var first time.Time
		if err := rows.Scan(&sub, &ip, &first); err != nil {
			return nil, nil, err
		}
		if firstSeen[sub] == nil {
			firstSeen[sub], lastSeen[sub] = map[string]time.Time{}, map[string]time.Time{}
		}
		firstSeen[sub][ip] = first
		lastSeen[sub][ip] = now
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return firstSeen, lastSeen, nil
}

func (s *Server) loadDeviceLimitAdmissions(ctx context.Context) error {
	firstSeen, lastSeen, err := s.queryDeviceLimitAdmissions(ctx)
	if err != nil {
		return err
	}
	deviceLimits.firstSeen = firstSeen
	deviceLimits.lastSeen = lastSeen
	return nil
}

func (s *Server) checkDeviceLimits(ctx context.Context, mode string) {
	now := time.Now()
	rows, err := s.db.Query(ctx, `
SELECT t.id::text, t.label, t.ip_limit, n.name, COALESCE(c.ips, '[]'::jsonb),
       expected.node_id::text, c.node_id IS NOT NULL
  FROM subscription_tokens t
 CROSS JOIN LATERAL jsonb_object_keys(COALESCE(t.node_accounts, '{}'::jsonb)) AS expected(node_id)
  JOIN hysteria_nodes n ON n.id = expected.node_id::uuid AND n.deleted_at IS NULL
  LEFT JOIN subscription_live_connections c
    ON c.subscription_id = t.id AND c.node_id = n.id
   AND c.collected_at > now() - make_interval(secs => $1)
   AND c.ok
 WHERE t.deleted_at IS NULL AND t.enabled AND t.ip_limit > 0
   -- 2026-11-03：已到期的订阅不再参与设备上限（节点侧已按到期拒绝认证；
   -- 原来照样算超额、照样往节点推拦截名单，白白占用节点 API 配额）。
   AND (t.expires_at IS NULL OR t.expires_at > now())`, deviceLimitFreshness.Seconds())
	if err != nil {
		s.logger.Warn("device limit: query failed", "error", err)
		return
	}
	type subInfo struct {
		label    string
		limit    int
		nodes    map[string]struct{}
		online   map[string]*deviceLimitOnline
		complete bool
	}
	subs := map[string]*subInfo{}
	for rows.Next() {
		var subID, label, nodeName, nodeID string
		var limit int
		var raw []byte
		var observed bool
		if err := rows.Scan(&subID, &label, &limit, &nodeName, &raw, &nodeID, &observed); err != nil {
			rows.Close()
			s.logger.Warn("device limit: query row scan failed; skipping this round", "error", err)
			return
		}
		var ips []struct {
			IP       string `json:"ip"`
			LastSeen int64  `json:"last_seen"`
		}
		if err := json.Unmarshal(raw, &ips); err != nil {
			rows.Close()
			s.logger.Warn("device limit: online IP payload is invalid; skipping this round", "error", err)
			return
		}
		info := subs[subID]
		if info == nil {
			info = &subInfo{label: label, limit: limit, nodes: map[string]struct{}{}, online: map[string]*deviceLimitOnline{}, complete: true}
			subs[subID] = info
		}
		info.nodes[nodeID] = struct{}{}
		if !observed {
			info.complete = false
		}
		for _, entry := range ips {
			ip := strings.TrimSpace(entry.IP)
			seen := time.Unix(entry.LastSeen, 0)
			if ip == "" || now.Sub(seen) > deviceLimitOnlineWindow {
				continue
			}
			cur := info.online[ip]
			if cur == nil {
				cur = &deviceLimitOnline{IP: ip, LastSeen: seen}
				info.online[ip] = cur
			}
			if seen.After(cur.LastSeen) {
				cur.LastSeen = seen
			}
			cur.Nodes = append(cur.Nodes, nodeName)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("device limit: query iteration failed; skipping this round", "error", err)
		return
	}
	rows.Close()
	// 2026-11-02 修：任一节点缺数据时原来整条订阅跳过，生产上 19 条限额订阅常有
	// 7 条处于「永不检查」状态（节点按源端口配对连接日志，端口跳跃后长期判不一致）。
	// 缺数据只会让在线 IP 少算、不会多算，所以已确认的 IP 超额仍然成立：照常判定
	// 并下发拦截；但不能凭不完整的数据判「已回到名额内」而解除，也不能释放名额
	// （那些 IP 可能正在缺数据的节点上在线）。
	partial := map[string]bool{}
	for subID, info := range subs {
		if !info.complete {
			partial[subID] = true
		}
	}
	deviceLimits.mu.Lock()
	for subID := range partial {
		if !deviceLimits.incomplete[subID] {
			s.logger.Warn("device limit: incomplete node telemetry; enforcing confirmed IPs only", "subscription", shortID(subID))
		}
	}
	for subID := range deviceLimits.incomplete {
		if _, still := subs[subID]; still && !partial[subID] {
			s.logger.Info("device limit: node telemetry complete again", "subscription", shortID(subID))
		}
	}
	deviceLimits.incomplete = partial
	deviceLimits.mu.Unlock()

	var jobs []pushJob
	var newAdmissions, released [][2]string

	// 若尚未加载持久化名额，先在互斥锁外查库再回填，避免数据库慢查询阻塞交付页读取当前拦截数。
	deviceLimits.mu.Lock()
	needLoad := !deviceLimits.loaded
	deviceLimits.mu.Unlock()
	var loadedFirst, loadedLast map[string]map[string]time.Time
	if needLoad {
		var loadErr error
		loadedFirst, loadedLast, loadErr = s.queryDeviceLimitAdmissions(ctx)
		if loadErr != nil {
			s.logger.Warn("device limit: admission load failed; skipping this round", "error", loadErr)
			return
		}
	}

	deviceLimits.mu.Lock()
	if !deviceLimits.loaded {
		deviceLimits.firstSeen = loadedFirst
		deviceLimits.lastSeen = loadedLast
		deviceLimits.loaded = true
	}
	for subID, info := range subs {
		first, last := deviceLimits.firstSeen[subID], deviceLimits.lastSeen[subID]
		if first == nil {
			first, last = map[string]time.Time{}, map[string]time.Time{}
			deviceLimits.firstSeen[subID], deviceLimits.lastSeen[subID] = first, last
		}
		for ip, entry := range info.online {
			if _, ok := first[ip]; !ok {
				first[ip] = now
				newAdmissions = append(newAdmissions, [2]string{subID, ip})
			}
			if entry.LastSeen.After(last[ip]) {
				last[ip] = entry.LastSeen
			}
		}
	}
	for subID, last := range deviceLimits.lastSeen {
		if partial[subID] {
			continue
		}
		for ip, seen := range last {
			if now.Sub(seen) > deviceLimitOnlineWindow {
				delete(last, ip)
				delete(deviceLimits.firstSeen[subID], ip)
				released = append(released, [2]string{subID, ip})
			}
		}
		if len(last) == 0 {
			delete(deviceLimits.lastSeen, subID)
			delete(deviceLimits.firstSeen, subID)
		}
	}
	for key, at := range deviceLimits.recorded {
		if now.Sub(at) > deviceLimitRecordEvery {
			delete(deviceLimits.recorded, key)
		}
	}
	// 定期淘汰过期的旧版门户告警抑制记录，防止已删除节点的键长期滞留内存。
	for nodeID, at := range deviceLimits.warned {
		if now.Sub(at) > 2*time.Hour {
			delete(deviceLimits.warned, nodeID)
		}
	}
	type auditRow struct {
		subID   string
		details []byte
		extra   []string
		limit   int
		online  int
	}
	var audits []auditRow
	current := map[string][]string{}
	for subID, info := range subs {
		online := make([]deviceLimitOnline, 0, len(info.online))
		for _, entry := range info.online {
			online = append(online, *entry)
		}
		allowed, extra := deviceLimitVerdict(online, deviceLimits.firstSeen[subID], info.limit)
		if len(extra) == 0 {
			continue
		}
		current[subID] = extra
		key := subID + "|" + strings.Join(extra, ",")
		if _, done := deviceLimits.recorded[key]; done {
			continue
		}
		deviceLimits.recorded[key] = now
		where := map[string][]string{}
		for _, entry := range online {
			where[entry.IP] = entry.Nodes
		}
		details, _ := json.Marshal(map[string]any{
			"mode": mode, "label": info.label, "limit": info.limit,
			"online": len(online), "allowed": allowed, "would_block": extra, "nodes": where,
		})
		audits = append(audits, auditRow{subID: subID, details: details, extra: extra, limit: info.limit, online: len(online)})
	}

	for subID, prev := range deviceLimits.pushed {
		_, listed := subs[subID]
		// 2026-11-03：订阅停用、删除或把上限改成 0 后就不在查询结果里了，
		// 原来 observed=false ⇒ 不解除，被拦的 IP 要一直等 600 秒 TTL，交付页还显示拦截数。
		// 不在结果里 = 不再受上限约束，按「已观测且未超额」处理；
		// 只有「在结果里但这一轮数据不完整」才保留现状。
		observed := !listed || !partial[subID]
		_, overLimit := current[subID]
		if mode != "enforce" || (observed && !overLimit) {
			if prev.ips != "" {
				jobs = append(jobs, pushJob{subID: subID, ips: nil})
			}
		}
	}
	if mode == "enforce" {
		for subID, extra := range current {
			sorted := append([]string(nil), extra...)
			sort.Strings(sorted)
			joined := strings.Join(sorted, ",")
			prev, had := deviceLimits.pushed[subID]
			if partial[subID] && had {
				// 不完整的一轮只增不减：保留上一轮已拦截、本轮没看到的 IP。
				sorted = deviceLimitUnion(prev.ips, sorted)
				joined = strings.Join(sorted, ",")
			}
			if deviceLimitNeedsPush(prev, had, joined, now) {
				jobs = append(jobs, pushJob{subID: subID, ips: sorted})
			}
		}
	}
	deviceLimits.mu.Unlock()

	for _, a := range newAdmissions {
		if _, err := s.db.Exec(ctx, `INSERT INTO subscription_ip_admissions (subscription_id, ip, first_seen)
			VALUES ($1::uuid, $2, $3) ON CONFLICT DO NOTHING`, a[0], a[1], now); err != nil {
			deviceLimits.mu.Lock()
			delete(deviceLimits.firstSeen[a[0]], a[1])
			delete(deviceLimits.lastSeen[a[0]], a[1])
			deviceLimits.mu.Unlock()
			s.logger.Warn("device limit: admission save failed; will retry", "subscription", shortID(a[0]), "error", err)
		}
	}
	for _, r := range released {
		if _, err := s.db.Exec(ctx, `DELETE FROM subscription_ip_admissions WHERE subscription_id=$1::uuid AND ip=$2`, r[0], r[1]); err != nil {
			deviceLimits.mu.Lock()
			if deviceLimits.firstSeen[r[0]] == nil {
				deviceLimits.firstSeen[r[0]], deviceLimits.lastSeen[r[0]] = map[string]time.Time{}, map[string]time.Time{}
			}
			deviceLimits.firstSeen[r[0]][r[1]] = now.Add(-deviceLimitOnlineWindow)
			deviceLimits.lastSeen[r[0]][r[1]] = now.Add(-deviceLimitOnlineWindow)
			deviceLimits.mu.Unlock()
			s.logger.Warn("device limit: admission release failed; will retry", "subscription", shortID(r[0]), "error", err)
		}
	}
	action := "device_limit_exceeded"
	if mode == "enforce" {
		action = "device_limit_enforced"
	}
	for _, a := range audits {
		if _, err := s.db.Exec(ctx, `INSERT INTO audit_logs (user_id, action, resource_type, resource_id, ip_address, details)
			VALUES (NULL, $1, 'subscription_token', $2::uuid, NULL, $3::jsonb)`, action, a.subID, string(a.details)); err != nil {
			s.logger.Warn("device limit: audit insert failed", "error", err)
		}
		s.logger.Info("device limit exceeded", "mode", mode, "subscription", shortID(a.subID),
			"limit", a.limit, "online", a.online, "over", strings.Join(a.extra, ","))
	}
	// 2026-11-04：并发下发（上限与开户一致）。整轮共用 25 秒预算，原来逐条串行、每台节点最多等 8 秒：
	// 一台不可达的节点挂着四条限额订阅就要 32 秒，排在后面的任务（包括解除拦截）全部超时、不记账，
	// map 顺序随机，同一条解除可能连续多轮都轮不到，用户回到名额内后仍被拦到 TTL 过期。
	s.runDeviceLimitPushes(ctx, jobs, now)
}

// runDeviceLimitPushes 并发执行一轮下发（并发上限 provisioningConcurrency）。
func (s *Server) runDeviceLimitPushes(ctx context.Context, jobs []pushJob, now time.Time) {
	sem := make(chan struct{}, provisioningConcurrency)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(job pushJob) {
			defer wg.Done()
			defer func() { <-sem }()
			s.applyDeviceLimitPush(ctx, job, now)
		}(job)
	}
	wg.Wait()
}

type pushJob struct {
	subID string
	ips   []string
}

// applyDeviceLimitPush 下发一条拦截列表并按结果记账。
func (s *Server) applyDeviceLimitPush(ctx context.Context, job pushJob, now time.Time) {
	{
		applied, complete := s.pushDeviceLimitDeny(ctx, job.subID, job.ips)
		// 2026-11-03：原来只要有一台节点失败就整体不记账。可是可达的节点已经拦下了这些 IP：
		// 不记 pushed，用户回到名额内时解除循环（只遍历 pushed）就不会发空列表，
		// 那些节点上要一直拦到 600 秒 TTL 过期；同时整批每 30 秒重推一遍，页面上拦截数显示 0。
		// 现在：至少一台接受就记账；不完整时把时间戳留旧，下一轮会续推补上失败的节点。
		if !applied {
			return
		}
		at := now
		if !complete {
			at = time.Time{}
		}
		deviceLimits.mu.Lock()
		if len(job.ips) == 0 && complete {
			delete(deviceLimits.pushed, job.subID)
			delete(deviceLimits.blocked, job.subID)
		} else if len(job.ips) == 0 {
			// 解除只到了一部分节点：保留一条「已拦截但列表为空」的记录，下一轮再发一次空列表。
			deviceLimits.pushed[job.subID] = deviceLimitPush{ips: deviceLimitRetryUnblock, at: at}
		} else {
			deviceLimits.pushed[job.subID] = deviceLimitPush{ips: strings.Join(job.ips, ","), at: at}
			deviceLimits.blocked[job.subID] = len(job.ips)
		}
		deviceLimits.mu.Unlock()
	}
}

// pushDeviceLimitDeny 把拦截列表下发到订阅开户的每台节点。
// applied：至少一台节点接受了（或无事可做）；complete：所有需要下发的节点都接受了。
func (s *Server) pushDeviceLimitDeny(ctx context.Context, subID string, ips []string) (applied, complete bool) {
	rec, err := s.loadSubscriptionByID(ctx, subID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, true
	}
	if err != nil {
		s.logger.Warn("device limit: subscription load failed; will retry", "subscription", shortID(subID), "error", err)
		return false, false
	}
	if ips == nil {
		ips = []string{}
	}
	// 2026-11-06：并发下发，且节点请求不再共享整轮预算。
	// 原来逐台串行、每台 8 秒，而 runCtx 只有 25 秒：12 台里 3 台黑洞就吃满预算，
	// 后面的**健康节点**立刻 context deadline exceeded 被记成「push to node failed」 ⇒ complete=false ⇒
	// 每 30 秒重推一次、坏节点被猛敲，解除拦截（retry-unblock）永远凑不齐，
	// 已回到名额内的客户在某些节点上一直拦到 TTL。
	// 并发度 3：与其它节点写路径的姿势一致（节点侧 portal 是单线程）。
	type pushOutcome struct {
		nodeID, nodeName, err string
		accepted, attempted   bool
		skipped               bool
	}
	outcomes := make([]pushOutcome, 0, len(rec.nodeAccounts))
	mu := sync.Mutex{}
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for nodeID, account := range rec.nodeAccounts {
		wg.Add(1)
		go func(nodeID string, account subscriptionNodeAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// 每台节点自己的预算，脱离整轮 ctx：一轮里再慢也不该把别的节点判成失败。
			callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			defer cancel()
			out := pushOutcome{nodeID: nodeID}
			target, err := s.loadPortalTarget(callCtx, nodeID)
			if errors.Is(err, errPortalNodeNotFound) {
				// 映射里残留的已删除节点：那台机器已经不在，不能拿它当「下发失败」一直重试。
				out.skipped = true
				out.nodeName = nodeID
			} else if err != nil {
				out.attempted = true
				out.err = err.Error()
				out.nodeName = nodeID
				s.logger.Warn("device limit: portal credentials unavailable; will retry", "node_id", shortID(nodeID), "subscription", shortID(subID), "error", err)
			} else {
				out.attempted = true
				out.nodeName = target.NodeName
				if _, err := portalCall(callCtx, target.BaseURL, target.APIKey, http.MethodPost, "/users/ip-deny",
					map[string]any{"user_id": account.UserID, "ips": ips, "ttl": deviceLimitDenyTTL}); err == nil {
					out.accepted = true
				} else {
					out.err = err.Error()
				}
			}
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
		}(nodeID, account)
	}
	wg.Wait()

	ok := true
	accepted, attempted := 0, 0
	for _, out := range outcomes {
		if out.skipped {
			continue
		}
		if out.attempted {
			attempted++
		}
		if out.accepted {
			accepted++
			continue
		}
		msg := out.err
		if strings.Contains(msg, "404") || strings.Contains(strings.ToLower(msg), "not found") {
			deviceLimits.mu.Lock()
			if last, seen := deviceLimits.warned[out.nodeID]; !seen || time.Since(last) > time.Hour {
				deviceLimits.warned[out.nodeID] = time.Now()
				s.logger.Warn("device limit: node portal too old for ip-deny, skipped", "node", out.nodeName)
			}
			deviceLimits.mu.Unlock()
			continue
		}
		ok = false
		s.logger.Warn("device limit: push to node failed", "node", out.nodeName, "subscription", shortID(subID), "error", msg)
	}
	if attempted == 0 {
		return true, true
	}
	// 2026-11-04：有要拦的 IP 时，只有真的被某台节点接受才算「已下发」。原来所有节点都因门户过旧
	// 被跳过时仍返回 applied=true：调用方记下拦截数，交付页告诉买家「多出的 N 个网络已被暂时拦截」，
	// 而实际上没有任何节点拦了。
	if len(ips) > 0 && accepted == 0 {
		return false, false
	}
	return ok || accepted > 0, ok
}
