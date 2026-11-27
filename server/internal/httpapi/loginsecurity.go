package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// 面板登录安全（2026-10-16）：
//   - 连续失败封禁：同一 IP 15 分钟内失败 5 次 → 封禁 30 分钟（比原有的
//     「5 分钟 10 次」限流更严，且封禁期内连正确密码也拒绝），并推送 Telegram。
//   - 新 IP 登录提醒：账号第一次从某个 IP 登录成功时推送 Telegram。
//   - 最近登录：账号安全页可以看到自己的登录来源。
// 封禁状态只在内存里：面板重启即解除 —— 这是可以接受的，攻击者没法控制面板重启。

const (
	loginFailWindow      = 15 * time.Minute
	loginFailMax         = 5
	loginBanFor          = 30 * time.Minute
	maxLoginGuardEntries = 16384
)

type loginGuard struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	banned map[string]time.Time
	// lastSweep 上次全表清理时刻。
	//
	// ⚠️ 2026-10-30：这两个 map 原来只做「单个 IP 内部」的裁剪，**键永不删除**。
	// 而登录是公开端点：2026-10-29 那轮爆破用了约 1250 个 IP，日常还有持续扫描。
	// 每一个见过的源 IP 都会在 fails 里永久占一个键（含一个时间戳切片），
	// 被封禁过又不再回来的 IP 在 banned 里同样永久滞留 —— 面板跑得越久、被扫得越多，
	// 内存只增不减，且没有任何迹象。
	// 现在每 5 分钟（在一次失败/检查时顺带）清掉窗口外与已解封的条目。
	lastSweep time.Time
}

// sweepLocked 清掉过期条目；调用方必须已持有 g.mu。
func (g *loginGuard) sweepLocked(now time.Time) {
	if !g.lastSweep.IsZero() && now.Sub(g.lastSweep) < 5*time.Minute {
		return
	}
	g.lastSweep = now
	for ip, stamps := range g.fails {
		alive := stamps[:0]
		for _, t := range stamps {
			if now.Sub(t) < loginFailWindow {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			delete(g.fails, ip)
			continue
		}
		g.fails[ip] = alive
	}
	for ip, until := range g.banned {
		if now.After(until) {
			delete(g.banned, ip)
		}
	}
}

var loginGuardState = &loginGuard{fails: map[string][]time.Time{}, banned: map[string]time.Time{}}

// loginKey 是登录限流与失败封禁的计数键（2026-11-06）。IPv4 按单个地址；IPv6 按所在 /64 ——
// 一台主机 / 一个家庭宽带通常分到整个 /64，按单个地址计时攻击者每次换一个地址，限流与封禁都形同虚设
// （Turnstile 被应急关闭时就完全没有防护）。显示与审计仍用原始地址。
func loginKey(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// changePasswordGuardKey 是改密接口「当前密码」猜错的计数键（与登录封禁共用阈值）。
func changePasswordGuardKey(userID string) string { return "pwchg:" + userID }

// bannedUntil 返回该 IP 的封禁截止时间（未封禁返回零值）。
func (g *loginGuard) bannedUntil(ip string, now time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	until, ok := g.banned[ip]
	if !ok {
		return time.Time{}
	}
	if now.After(until) {
		delete(g.banned, ip)
		return time.Time{}
	}
	return until
}

// fail 记一次失败；达到阈值时封禁并返回 true（只在刚封禁的那一次返回 true）。
func (g *loginGuard) fail(ip string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	recent := g.fails[ip][:0]
	for _, t := range g.fails[ip] {
		if now.Sub(t) < loginFailWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	g.fails[ip] = recent
	if len(recent) >= loginFailMax {
		g.banned[ip] = now.Add(loginBanFor)
		delete(g.fails, ip)
		g.evictBannedLocked(now, ip)
		return true
	}
	// 在分布式洪水时强制限制 map 体积，防止内存无限增长（OOM）。
	if len(g.fails) > maxLoginGuardEntries {
		for k, v := range g.fails {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > loginFailWindow {
				delete(g.fails, k)
			}
		}
	}
	if len(g.fails) > maxLoginGuardEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, v := range g.fails {
			if k == ip {
				continue
			}
			last := time.Time{}
			if len(v) > 0 {
				last = v[len(v)-1]
			}
			if oldestKey == "" || last.Before(oldestAt) {
				oldestKey, oldestAt = k, last
			}
		}
		if oldestKey != "" {
			delete(g.fails, oldestKey)
		}
	}
	g.evictBannedLocked(now, ip)
	return false
}

// evictBannedLocked 在封禁表超出硬上限时先清过期项，仍超限则淘汰最早解封的旧条目，防止分布式爆破耗尽内存。
func (g *loginGuard) evictBannedLocked(now time.Time, keepIP string) {
	if len(g.banned) <= maxLoginGuardEntries {
		return
	}
	for k, until := range g.banned {
		if now.After(until) {
			delete(g.banned, k)
		}
	}
	for len(g.banned) > maxLoginGuardEntries {
		var earliestKey string
		var earliestUntil time.Time
		for k, until := range g.banned {
			if k == keepIP {
				continue
			}
			if earliestKey == "" || until.Before(earliestUntil) {
				earliestKey, earliestUntil = k, until
			}
		}
		if earliestKey == "" {
			break
		}
		delete(g.banned, earliestKey)
	}
}

func (g *loginGuard) success(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

// telegramNotifyAsync 尽力发一条 Telegram（没配就什么也不做）。
func (s *Server) telegramNotifyAsync(text string) {
	go s.telegramNotify(text)
}

// telegramNotify 同步发送并报告是否送达（未绑定 Chat 也算未送达）。
//
// 2026-11-03：按 Telegram 4096 字符上限分段发送。原来整段一次 sendMessage，
// 13 台节点的解锁变化、IPv6 封禁列表这类长消息会被 Telegram 400 拒收，只留一条 Warn 日志。
func (s *Server) telegramNotify(text string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chat := s.telegramChatID(ctx)
	if chat == "" {
		return false
	}
	if err := s.sendTelegramLong(ctx, chat, text); err != nil {
		s.logger.Warn("telegram notify failed", "error", err)
		return false
	}
	return true
}

// sendTelegramLong 把超长文本按上限切段逐条发送；任一段失败即返回错误。
func (s *Server) sendTelegramLong(ctx context.Context, chat, text string) error {
	for _, chunk := range splitTelegramMessage(text, telegramChunkLimit) {
		if err := s.sendTelegram(ctx, chat, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) telegramNotifyWithBanAsync(text, ip string) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil && s.logger != nil {
				s.logger.Error("telegram notify with ban panic", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		chat := s.telegramChatID(ctx)
		token := s.telegramToken()
		if chat == "" || token == "" {
			return
		}
		payload := map[string]any{
			"chat_id":                  chat,
			"text":                     text,
			"disable_web_page_preview": true,
		}
		if net.ParseIP(strings.TrimSpace(ip)) != nil {
			payload["reply_markup"] = map[string]any{
				"inline_keyboard": [][]map[string]string{{{
					"text":          "🚫 立即永久封禁此IP (" + ip + ")",
					"callback_data": "tg:banip:" + ip,
				}}},
			}
		}
		if err := telegramCall(ctx, token, "sendMessage", payload, nil); err != nil && s.logger != nil {
			s.logger.Warn("telegram notify failed", "error", err)
		}
	}()
}

func (s *Server) onLoginFailure(ip, email string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return
	}
	// 受信任的反代地址不计入封禁，防止代理链未透传客户端地址时误封全体用户。
	if s.isTrustedProxyAddress(ip) {
		if s.logger != nil {
			s.logger.Warn("login: failure attributed to a trusted proxy address; not banning (check X-Forwarded-For on the reverse proxy)", "ip", ip)
		}
		return
	}
	if loginGuardState.fail(loginKey(ip), time.Now()) {
		if s.logger != nil {
			s.logger.Warn("login: ip banned after repeated failures", "ip", ip, "key", loginKey(ip))
		}
		s.telegramNotifyWithBanAsync(fmt.Sprintf("🚫 面板登录：%s 在 %d 分钟内连续失败 %d 次，已封禁 %d 分钟。最后尝试的账号：%s",
			ip, int(loginFailWindow.Minutes()), loginFailMax, int(loginBanFor.Minutes()), firstNonEmpty(email, "（空）")), ip)
	}
}

// onLoginSuccess 记录登录来源；新 IP 时推送提醒。
func (s *Server) onLoginSuccess(ctx context.Context, userID, email, ip, userAgent string) {
	loginGuardState.success(loginKey(ip))
	if ip == "" {
		return
	}
	userAgent = truncateRunes(strings.TrimSpace(userAgent), 300)
	var inserted bool
	err := s.db.QueryRow(ctx, `
INSERT INTO login_history (user_id, ip, user_agent) VALUES ($1::uuid, $2::inet, $3)
ON CONFLICT (user_id, ip) DO UPDATE SET last_seen_at = now(), user_agent = EXCLUDED.user_agent,
    login_count = login_history.login_count + 1
RETURNING (xmax = 0)`, userID, ip, userAgent).Scan(&inserted)
	if err != nil {
		s.logger.Warn("login history insert failed", "error", err)
		return
	}
	if !inserted {
		return
	}
	// 该账号的第一条记录不提醒（首次登录 / 功能刚上线），之后每个新 IP 都提醒。
	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM login_history WHERE user_id=$1::uuid`, userID).Scan(&total); err != nil {
		s.logger.Warn("login history count failed", "user_id", userID, "error", err)
		// 记录已写入；无法确认这是首个来源时保守发送提醒，避免登录安全事件静默。
		total = 2
	}
	if total <= 1 {
		return
	}
	loc := ""
	if s.geo != nil {
		gctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if info, err := s.geo.Lookup(gctx, ip); err == nil {
			loc = strings.TrimSpace(info.CountryName() + " " + info.Region + " " + info.Provider)
		}
		cancel()
	}
	s.telegramNotifyAsync(fmt.Sprintf("🔐 面板新 IP 登录\n账号：%s\nIP：%s %s\n设备：%s\n如果不是你本人，请立即修改密码。",
		email, ip, loc, truncateRunes(userAgent, 80)))
}

// GET /api/auth/logins —— 当前账号最近的登录来源。
func (s *Server) myLogins(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT host(ip), user_agent, first_seen_at, last_seen_at, login_count FROM login_history
WHERE user_id = $1::uuid ORDER BY last_seen_at DESC LIMIT 20`, claims.Subject)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	type item struct {
		IP        string    `json:"ip"`
		UserAgent string    `json:"user_agent"`
		FirstSeen time.Time `json:"first_seen_at"`
		LastSeen  time.Time `json:"last_seen_at"`
		Count     int       `json:"login_count"`
		Current   bool      `json:"current"`
	}
	current := s.requestClientIP(r)
	out := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.IP, &it.UserAgent, &it.FirstSeen, &it.LastSeen, &it.Count); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		it.Current = it.IP == current
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "ban_policy": fmt.Sprintf("同一 IP %d 分钟内失败 %d 次封禁 %d 分钟",
		int(loginFailWindow.Minutes()), loginFailMax, int(loginBanFor.Minutes()))})
}

// isTrustedProxyAddress 判断 ip 是否落在受信任的反代网段里。
func (s *Server) isTrustedProxyAddress(raw string) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return false
	}
	for _, network := range s.trustedProxyCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
