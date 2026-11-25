package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodeops/server/internal/ippurity"
)

// Telegram 告警与控制机器人。控制入口复用面板 API，私聊身份与写入确认见 telegram_control.go。

const telegramChatSettingKey = "telegram_chat_id"

var telegramChatPattern = regexp.MustCompile(`^-?\d{3,20}$`)

var telegramHTTP = &http.Client{Timeout: 70 * time.Second}

func (s *Server) telegramChatID(ctx context.Context) string {
	// db 为 nil 时不能直接 QueryRow —— 那会 panic，
	// 而告警推送是在独立 goroutine 里跑的，panic 会连带打断推送。
	if s.db == nil {
		return ""
	}
	v, _ := s.telegramChatIDChecked(ctx)
	return v
}

// telegramChatIDChecked 与 telegramChatID 相同，但把「读库失败」与「没有绑定」分开（2026-11-06）。
// 绑定状态决定要不要回「你的 Chat ID 是…」、设置页显示「已绑定 / 未绑定」：数据库抖一下就把
// 已绑定说成未绑定，运维会以为配置丢了、重新绑定。鉴权类调用仍用 telegramChatID（读不到 = 拒绝，方向安全）。
func (s *Server) telegramChatIDChecked(ctx context.Context) (string, error) {
	if s.db == nil {
		return "", nil
	}
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, telegramChatSettingKey).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func telegramCall(ctx context.Context, token, method string, payload any, out any) error {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return errors.New("构造 Telegram 请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := telegramHTTP.Do(req)
	if err != nil {
		// 不带 err 原文：URL 里有 bot token。
		return &telegramRetryableError{msg: "连接 Telegram 失败（网络错误或超时）"}
	}
	defer resp.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return &telegramRetryableError{msg: fmt.Sprintf("Telegram 暂不可用（%d）", resp.StatusCode)}
		}
		return fmt.Errorf("Telegram 返回 %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return &telegramRetryableError{msg: fmt.Sprintf("Telegram 暂不可用（%d）", resp.StatusCode)}
	}
	if !env.OK || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			return &telegramRetryableError{msg: "Telegram 限流：" + env.Description,
				after: time.Duration(env.Parameters.RetryAfter) * time.Second}
		}
		return fmt.Errorf("Telegram 拒绝：%s", env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// telegramRetryableError 标记「值得重试一次」的失败：网络错误 / 超时，或 429 限流。
// after 是 429 响应里 parameters.retry_after 给的等待时间（没有则为 0）。
type telegramRetryableError struct {
	msg   string
	after time.Duration
}

func (e *telegramRetryableError) Error() string { return e.msg }

// telegramRetryBackoff：可重试失败后默认等多久再试；telegramRetryMaxWait 是 retry_after 的上限
// （告警推送总共只有 15 秒预算，等不起 Telegram 要求的几分钟，那就放弃这一条）。
var (
	telegramRetryBackoff = 2 * time.Second
	telegramRetryMaxWait = 10 * time.Second
)

// telegramRetryWait 判断 err 能不能重试、要等多久。
func telegramRetryWait(err error) (time.Duration, bool) {
	var re *telegramRetryableError
	if !errors.As(err, &re) {
		return 0, false
	}
	wait := telegramRetryBackoff
	if re.after > 0 {
		wait = re.after
	}
	if wait > telegramRetryMaxWait {
		return 0, false
	}
	return wait, true
}

// sendWithOneRetry 发一次，遇到网络错误 / 429 时退避后**只重试一次**。
// 每次尝试单独限时，为退避与第二次请求留出预算。
func sendWithOneRetry(ctx context.Context, send func(context.Context) error) error {
	attempt := func() error {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return send(attemptCtx)
	}
	err := attempt()
	wait, ok := telegramRetryWait(err)
	if err == nil || !ok {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(wait):
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return attempt()
}

func (s *Server) sendTelegram(ctx context.Context, chatID, text string) error {
	token := s.purityKeys.Get(ippurity.KeyTelegram)
	if token == "" || chatID == "" {
		return errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	return sendWithOneRetry(ctx, func(attemptCtx context.Context) error {
		return telegramCall(attemptCtx, token, "sendMessage", map[string]any{
			"chat_id": chatID, "text": text, "disable_web_page_preview": true,
		}, nil)
	})
}

// notifyTelegram 由 notifyAlert 调用（已经过节流）。
func (s *Server) notifyTelegram(ev alertNotification) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	chat := s.telegramChatID(ctx)
	if chat == "" || s.purityKeys.Get(ippurity.KeyTelegram) == "" {
		return false
	}
	icon := map[string]string{"triggered": "🔴", "reopened": "🔴", "repeated": "🔴", "resolved": "✅", "acknowledged": "🟡"}[ev.Event]
	if ev.Severity == "warning" && icon == "🔴" {
		icon = "🟠"
	}
	text := strings.TrimSpace(icon + " " + ev.Text)
	// 处置按钮（2026-10-24 加）：故障类告警附带「确认告警 / 下线节点 /
	// 下线主机」按钮，让运维在 Telegram 里就能直接处理，不必回面板。
	// 按钮动作最终落到现有面板 API，走统一的两阶段确认与审计。
	rows := s.alertActionRows(ctx, chat, ev)
	if len(rows) > 0 {
		if err := s.sendTelegramRich(ctx, chat, text, rows); err != nil {
			s.logger.Warn("telegram notify failed", "error", err)
			return false
		}
		return true
	}
	if err := s.sendTelegram(ctx, chat, text); err != nil {
		s.logger.Warn("telegram notify failed", "error", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------- 机器人

// telegramBotStatus 记录机器人最近的运行情况，给设置页显示 —— 之前轮询失败、
// 回复失败都被静默吞掉，用户只看到「机器人不回话」却无从排查。
var telegramBotStatus = struct {
	sync.Mutex
	LastPollOK   time.Time
	LastError    string
	LastErrorAt  time.Time
	LastFromChat string
	LastFromAt   time.Time
	LastReplyErr string
	BotUsername  string
}{}

func noteTelegramError(msg string) {
	telegramBotStatus.Lock()
	telegramBotStatus.LastError, telegramBotStatus.LastErrorAt = msg, time.Now()
	telegramBotStatus.Unlock()
}

func (s *Server) StartTelegramBot(ctx context.Context) {
	queue := make(chan func(), 16)
	light := make(chan struct{}, 4)
	run := func(fn func()) {
		defer func() {
			if recover() != nil {
				noteTelegramError("消息处理异常，请重新打开菜单；未确认操作不会自动重试")
			}
		}()
		fn()
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case fn := <-queue:
				run(fn)
			}
		}
	}()
	// onBusy 在队列满、这条更新被放弃时调用（2026-11-04）。原来只记一条状态：用户什么都收不到，
	// 按钮回调连 answerCallbackQuery 都没发，转圈一直不停；而 offset 已经越过它，不会再收到第二次。
	dispatch := func(fn func(), quick bool, onBusy func()) {
		if quick {
			select {
			case light <- struct{}{}:
				go func() { defer func() { <-light }(); run(fn) }()
				return
			default:
			}
		}
		select {
		case queue <- fn:
		default:
			noteTelegramError("机器人任务队列繁忙，请稍后重试")
			if onBusy != nil {
				go run(onBusy)
			}
		}
	}
	busyNotice := "⏳ 机器人正在处理前面的操作，这一条没有执行，请稍后重新发送 / 点击。"

	go func() {
		var offset int64
		lastToken := ""
		menuToken := ""
		// pollFails 是 getUpdates 的连续失败次数（2026-10-24 审计 P2-3）。
		// 只用于决定日志级别：1 次是网络抖动，连续 10 次以上才是真断了。
		pollFails := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			token := s.purityKeys.Get(ippurity.KeyTelegram)
			if token == "" {
				sleepCtx(ctx, 30*time.Second)
				continue
			}
			if token != lastToken {
				offset, lastToken = 0, token
				var me struct {
					Username string `json:"username"`
				}
				meCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				if err := telegramCall(meCtx, token, "getMe", map[string]any{}, &me); err == nil {
					telegramBotStatus.Lock()
					telegramBotStatus.BotUsername = me.Username
					telegramBotStatus.Unlock()
					s.logger.Info("telegram bot online", "bot", me.Username)
				} else {
					s.logger.Warn("telegram getMe failed", "error", err)
					noteTelegramError("验证 Token 失败：" + err.Error())
				}
				cancel()
			}
			if token != menuToken {
				menuCtx, done := context.WithTimeout(ctx, 15*time.Second)
				if err := s.telegramInstallMenu(menuCtx, token); err == nil {
					menuToken = token
				} else {
					noteTelegramError("安装菜单失败：" + err.Error())
				}
				done()
			}
			var updates []struct {
				UpdateID int64 `json:"update_id"`
				Message  *struct {
					Text string `json:"text"`
					Date int64  `json:"date"`
					From struct {
						ID    int64 `json:"id"`
						IsBot bool  `json:"is_bot"`
					} `json:"from"`
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
				Callback *struct {
					ID   string `json:"id"`
					Data string `json:"data"`
					From struct {
						ID int64 `json:"id"`
					} `json:"from"`
					Message *struct {
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
					} `json:"message"`
				} `json:"callback_query"`
			}

			pollCtx, cancel := context.WithTimeout(ctx, 65*time.Second)
			err := telegramCall(pollCtx, token, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "callback_query"}}, &updates)
			cancel()
			if err != nil {
				// 2026-10-24 审计 P2-3：连续失败要能看出「偶发」还是「断了」。
				//
				// 原来每次失败都是一条一模一样的 WARN。实测东京→Telegram 偶有
				// 网络抖动，两天里 2 次，每次下一轮就自己好了 —— 这属于正常。
				// 但如果某天 API 域名被墙、或 token 被吊销，日志长得完全一样，
				// 运维看不出「这次是真的坏了，机器人已经不收消息 6 小时」。
				//
				// 现在记连续次数：第 1 次 INFO（偶发无需处理），之后每 10 次
				// 提一条 ERROR（持续失败必须人工介入）。判据落在日志级别上，
				// 是因为运维实际是按级别过滤的 —— 只有这样才真的看得见。
				pollFails++
				fields := []any{"error", err, "consecutive_failures", pollFails,
					"机器人已停止收消息约", (time.Duration(pollFails) * 20 * time.Second).String()}
				switch {
				case pollFails == 1:
					s.logger.Info("telegram getUpdates failed", fields...)
				case pollFails%10 == 0:
					s.logger.Error("telegram getUpdates still failing", fields...)
				default:
					s.logger.Warn("telegram getUpdates failed", fields...)
				}
				noteTelegramError("接收消息失败：" + err.Error())
				sleepCtx(ctx, 20*time.Second)
				continue
			}
			if pollFails > 0 {
				s.logger.Info("telegram getUpdates recovered", "failures", pollFails)
				pollFails = 0
			}
			telegramBotStatus.Lock()
			telegramBotStatus.LastPollOK = time.Now()
			telegramBotStatus.Unlock()
			// 2026-11-04：处理之前先向 Telegram 确认这一批。官方语义：「用比某条 update_id 更大的 offset
			// 调一次 getUpdates，该条即确认」。offset 原来只在内存里、要等下一次长轮询才确认，这期间进程重启
			// （部署）或换 Token 再换回来，Telegram 会把整批重发 —— 向导回答、封禁确认等写操作被执行第二次。
			// limit 最小为 1，这次确认调用可能顺带取回新的更新：一并放进本批处理，不能丢。
			if n := len(updates); n > 0 {
				ackCtx, ackCancel := context.WithTimeout(ctx, 10*time.Second)
				more := updates[:0:0]
				if err := telegramCall(ackCtx, token, "getUpdates", map[string]any{"offset": updates[n-1].UpdateID + 1, "timeout": 0, "allowed_updates": []string{"message", "callback_query"}}, &more); err != nil {
					s.logger.Warn("telegram update ack failed; batch may be redelivered after restart", "error", err)
				} else {
					updates = append(updates, more...)
				}
				ackCancel()
			}
			for _, u := range updates {
				offset = u.UpdateID + 1
				if u.Callback != nil {
					if u.Callback.Message != nil {
						cb := u.Callback
						dispatch(func() {
							s.telegramCallback(ctx, cb.ID, strconv.FormatInt(cb.Message.Chat.ID, 10), strconv.FormatInt(cb.From.ID, 10), cb.Data)
						}, false, func() {
							_ = telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": busyNotice, "show_alert": true}, nil)
						})
					}
					continue
				}

				if u.Message == nil || u.Message.From.IsBot {
					continue
				}
				chatID := strconv.FormatInt(u.Message.Chat.ID, 10)
				telegramBotStatus.Lock()
				telegramBotStatus.LastFromChat, telegramBotStatus.LastFromAt = chatID, time.Now()
				telegramBotStatus.Unlock()
				s.logger.Info("telegram message received", "chat", chatID, "command", tgCommand(u.Message.Text))
				text := strings.TrimSpace(u.Message.Text)
				sender := strconv.FormatInt(u.Message.From.ID, 10)
				cmd := tgCommand(text)
				staleWrite := u.Message.Date > 0 && time.Now().Unix()-u.Message.Date > 300 && cmd == "/api" && !isTelegramReadOnlyAPI(text)
				quick := cmd == "/menu" || cmd == "/start" || cmd == "/hide" || cmd == "/cancel" || cmd == "/status" || text == "🙈 收起菜单" || text == "🏠 主菜单"
				dispatch(func() {
					if staleWrite {
						s.sendTelegram(ctx, chatID, "这条操作消息已过期，请重新从菜单发起。")
						return
					}
					if telegramPrivateOwner(s.telegramChatID(ctx), chatID, sender) && s.telegramControlMessage(ctx, chatID, sender, text) {
						return
					}
					s.handleTelegramMessage(ctx, chatID, text)
				}, quick, func() {
					if telegramPrivateOwner(s.telegramChatID(ctx), chatID, sender) {
						_ = s.sendTelegram(ctx, chatID, busyNotice)
					}
				})

			}
		}
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (s *Server) handleTelegramMessage(ctx context.Context, chatID, text string) {
	cmd := tgCommand(text)
	cmd = strings.SplitN(cmd, "@", 2)[0]
	configured, chatErr := s.telegramChatIDChecked(ctx)
	if chatErr != nil {
		// 读不到绑定：不知道发消息的是不是本人，什么都不回（尤其不能回「你的 Chat ID 是…」）。
		s.logger.Warn("telegram: 读取绑定的 Chat ID 失败，本条消息不处理", "error", chatErr)
		return
	}
	if configured == "" {
		// 未配置 Chat ID 时，任何消息都回复 Chat ID（不限 /start：有人会直接点 START 或随便发一句）。
		if err := s.sendTelegram(ctx, chatID, "你的 Chat ID 是："+chatID+"\n把它填到 Teyir 控制台「系统设置 → Telegram」里，之后告警会发到这里。"); err != nil {
			s.logger.Warn("telegram reply failed", "chat", chatID, "error", err)
			telegramBotStatus.Lock()
			telegramBotStatus.LastReplyErr = err.Error()
			telegramBotStatus.Unlock()
		}
		return
	}
	if chatID != configured {
		return // 只服务配置的聊天，其他人一律不理
	}
	var reply string
	switch cmd {
	case "/status", "/start":
		reply = s.telegramStatus(ctx)
	case "/nodes":
		reply = s.telegramNodes(ctx)
	case "/alerts":
		reply = s.telegramAlerts(ctx)
	default:
		reply = "可用命令：\n/status 总览\n/nodes 节点状态\n/alerts 未处理告警"
	}
	// /nodes 在节点多时会超过 Telegram 单条 4096 字符的上限，整条被拒、用户什么都收不到。
	for _, chunk := range splitTelegramMessage(reply, telegramChunkLimit) {
		if err := s.sendTelegram(ctx, chatID, chunk); err != nil {
			s.logger.Warn("telegram reply failed", "error", err)
			telegramBotStatus.Lock()
			telegramBotStatus.LastReplyErr = err.Error()
			telegramBotStatus.Unlock()
			return
		}
	}
}

// telegramChunkLimit：单条消息的字符上限（Telegram 是 4096，留余量）。
const telegramChunkLimit = 4000

// splitTelegramMessage 按行切成每段不超过 limit 个字符（按 rune 计）的若干条。
// 只在行边界切，免得把一个节点名劈成两半；单行本身超长时才硬切。
func splitTelegramMessage(text string, limit int) []string {
	if limit <= 0 {
		limit = telegramChunkLimit
	}
	if len([]rune(text)) <= limit {
		return []string{text}
	}
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.TrimRight(string(cur), "\n"))
			cur = cur[:0]
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		r := []rune(line)
		if len(cur)+len(r) > limit {
			flush()
		}
		for len(r) > limit {
			out = append(out, string(r[:limit]))
			r = r[limit:]
		}
		cur = append(cur, r...)
	}
	flush()
	return out
}

func (s *Server) telegramStatus(ctx context.Context) string {
	var total, online, servers, alerts int
	// 查询失败时如实说「暂不可用」，不报 0 —— 「未处理告警 0 条」会让人以为一切正常。
	const unavailable = "暂不可用"
	nodesLine, serversLine, alertsLine := unavailable, unavailable, unavailable
	if s.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='online') FROM hysteria_nodes WHERE deleted_at IS NULL`).Scan(&total, &online) == nil {
		nodesLine = fmt.Sprintf("%d/%d", online, total)
	}
	if s.db.QueryRow(ctx, `SELECT count(*) FROM servers WHERE deleted_at IS NULL`).Scan(&servers) == nil {
		serversLine = fmt.Sprintf("%d 台", servers)
	}
	if s.db.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE status='open'`).Scan(&alerts) == nil {
		alertsLine = fmt.Sprintf("%d 条", alerts)
	}
	hidden := 0
	if s.autoHideEnabled(ctx) {
		hidden = len(s.unhealthyNodes(ctx))
	}
	msg := fmt.Sprintf("📊 Teyir 总览\n节点在线 %s\n主机 %s\n未处理告警 %s", nodesLine, serversLine, alertsLine)
	if hidden > 0 {
		msg += fmt.Sprintf("\n已自动从订阅下线 %d 个故障节点", hidden)
	}
	return msg
}

func (s *Server) telegramNodes(ctx context.Context) string {
	rows, err := s.db.Query(ctx, `SELECT name, status FROM hysteria_nodes WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return "读取节点失败"
	}
	defer rows.Close()
	hidden := map[string]hiddenNode{}
	if s.autoHideEnabled(ctx) {
		for _, h := range s.unhealthyNodes(ctx) {
			hidden[h.Name] = h
		}
	}
	var b strings.Builder
	b.WriteString("🖥 节点状态\n")
	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			s.logger.Warn("telegram: 读取节点状态失败", "error", err)
			return "节点状态暂不可用"
		}
		icon := map[string]string{"online": "🟢", "warning": "🟠", "offline": "🔴"}[status]
		if icon == "" {
			icon = "⚪"
		}
		fmt.Fprintf(&b, "%s %s", icon, name)
		if h, ok := hidden[name]; ok {
			fmt.Fprintf(&b, "（已下线：%s）", h.Reason)
		}
		b.WriteString("\n")
	}
	if rows.Err() != nil {
		return "节点状态暂不可用"
	}
	return b.String()
}

func (s *Server) telegramAlerts(ctx context.Context) string {
	rows, err := s.db.Query(ctx, `
SELECT a.type, a.severity, COALESCE(sv.name,''), COALESCE(sv.host,''), COALESCE(nd.name,''), COALESCE(nd.listen_port,0)
FROM alerts a LEFT JOIN servers sv ON sv.id = a.server_id LEFT JOIN hysteria_nodes nd ON nd.id = a.node_id
WHERE a.status = 'open' ORDER BY a.severity = 'critical' DESC, a.last_seen_at DESC LIMIT 15`)
	if err != nil {
		return "读取告警失败"
	}
	defer rows.Close()
	var b strings.Builder
	n := 0
	for rows.Next() {
		var kind, sev, sname, host, nname string
		var port int
		if err := rows.Scan(&kind, &sev, &sname, &host, &nname, &port); err != nil {
			s.logger.Warn("telegram: 读取告警列表失败", "error", err)
			return "告警状态暂不可用"
		}
		icon := "🟠"
		if sev == "critical" {
			icon = "🔴"
		}
		fmt.Fprintf(&b, "%s %s\n", icon, describeAlert(kind, sname, host, nname, port))
		n++
	}
	if rows.Err() != nil {
		return "告警状态暂不可用"
	}
	if n == 0 {
		return "✅ 没有未处理的告警"
	}
	return "🚨 未处理告警\n" + b.String()
}

// ---------------------------------------------------------------- 设置接口

// GET /api/settings/telegram
func (s *Server) getTelegramSettings(w http.ResponseWriter, r *http.Request) {
	telegramBotStatus.Lock()
	st := map[string]any{
		"bot_username":   telegramBotStatus.BotUsername,
		"last_poll_ok":   timeOrNil(telegramBotStatus.LastPollOK),
		"last_error":     telegramBotStatus.LastError,
		"last_error_at":  timeOrNil(telegramBotStatus.LastErrorAt),
		"last_from_chat": telegramBotStatus.LastFromChat,
		"last_from_at":   timeOrNil(telegramBotStatus.LastFromAt),
		"last_reply_err": telegramBotStatus.LastReplyErr,
	}
	telegramBotStatus.Unlock()
	chatID, chatErr := s.telegramChatIDChecked(r.Context())
	if chatErr != nil {
		s.databaseError(w, r, chatErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_configured":     s.purityKeys.Get(ippurity.KeyTelegram) != "",
		"control_private_only": true,
		"control_routes":       len(s.telegramControl.Routes),
		"menu_commands":        telegramCommands(),
		"chat_id":              chatID,
		"bot":                  st,
	})
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// PUT /api/settings/telegram  {"chat_id": "123456"}
func (s *Server) setTelegramSettings(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		ChatID string `json:"chat_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	in.ChatID = strings.TrimSpace(in.ChatID)
	if in.ChatID != "" && !telegramChatPattern.MatchString(in.ChatID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Chat ID 是一串数字（群组以 - 开头）"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, 'Telegram 告警与机器人的 Chat ID')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, telegramChatSettingKey, in.ChatID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "set_telegram_chat", "system_setting", telegramChatSettingKey, map[string]any{"chat_id": in.ChatID})
	s.getTelegramSettings(w, r)
}

// POST /api/settings/telegram/test
func (s *Server) testTelegram(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// 2026-11-06：没填 Chat ID / Token 时直接说缺什么。原来照样去调 Telegram，回一句看不懂的 502（chat not found 之类）。
	if s.telegramChatID(ctx) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "还没有保存 Chat ID：先给机器人发 /start 拿到 Chat ID 并保存"})
		return
	}
	if s.purityKeys == nil || s.purityKeys.Get(ippurity.KeyTelegram) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "还没有配置 Bot Token：先在上一步保存机器人 Token"})
		return
	}
	if err := s.sendTelegram(ctx, s.telegramChatID(ctx), "✅ Teyir 控制台测试消息：告警会发到这里。发送 /help 查看可用命令。"); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	chat := s.telegramChatID(ctx)
	if telegramPrivateOwner(chat, chat, chat) {
		if err := s.telegramMenu(ctx, chat); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "测试消息已发送，但菜单发送失败：" + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
