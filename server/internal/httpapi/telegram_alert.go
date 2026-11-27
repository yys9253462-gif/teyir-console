package httpapi

// telegram_alert.go —— 告警处置按钮（2026-10-24 加）。
//
// 背景：面板把告警推给 Telegram 之后，运维只能看到一句话，
// 要处置就得自己去面板点。这在「节点半夜挂了」时是很慢的一步 ——
// 而处置动作其实只有三四个：确认、下线节点、下线主机。
//
// 设计取舍
// --------
//  1. **不另写一套下线逻辑**。按钮最终落到的是**现有面板 API**
//     （POST /api/alerts/{id}/ack、DELETE /api/nodes/{id}、
//     DELETE /api/servers/{id}），走 telegramRun 的统一通道：
//     私聊本人校验 → 两阶段确认卡 → 执行时带 X-Confirm-Dangerous → 审计。
//     如果在这里自己写一份「删节点」，就会出现两套下线语义，
//     日后必然分叉（一边改了另一边没改）。
//
//  2. **callback_data 只带随机 nonce，不带目标 ID**。
//     Telegram 的 callback_data 上限 64 字节，装不下「告警+节点+主机」
//     三组 UUID。更重要的是：ID 由服务端在**推送时**查好、放进登记表，
//     点击时从登记表取 —— 客户端（哪怕是本人）无法通过伪造
//     callback_data 去操作别的节点。
//
//  3. **按钮随告警类型给**。节点级故障给节点级动作，主机级故障给
//     主机级动作 + 该主机上各节点的动作。恢复类通知不给按钮
//     （没有可处置的对象）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// telegramAlertActionTTL 是处置动作登记的有效期。
// 10 分钟：足够人看一眼告警、想一下、点按钮；太久则让登记表积压
// 过期动作（点下去会作用于一个已经变化的世界）。
const telegramAlertActionTTL = 10 * time.Minute

// telegramAlertAction 是「一条待执行的处置动作」。
//
// 与 telegramPending 的区别：telegramPending 是**确认阶段**的凭证
// （点了「确认执行」就消费掉）；这里是**按钮阶段**的登记表
// （点一下取出动作，转成确认卡）。
type telegramAlertAction struct {
	Action telegramAction
	Chat   string
	Until  time.Time
}

// registerAlertAction 登记一个处置动作，返回可放进 callback_data 的 nonce。
//
// nonce 用 16 个十六进制字符（8 字节随机）：足够不可猜，
// 又短到 callback_data 留得下余量。
func (st *telegramControlState) registerAlertAction(chat string, a telegramAction) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(buf)
	st.Lock()
	defer st.Unlock()
	if st.Alerts == nil {
		st.Alerts = map[string]telegramAlertAction{}
	}
	// 清掉过期条目，避免登记表只增不减。
	now := time.Now()
	for k, v := range st.Alerts {
		if now.After(v.Until) {
			delete(st.Alerts, k)
		}
	}
	st.Alerts[nonce] = telegramAlertAction{Action: a, Chat: chat, Until: now.Add(telegramAlertActionTTL)}
	return nonce
}

// lookupAlertAction 取回登记的动作。
//
// **不消费**：用户在确认卡上点「取消」后还能再点一次原按钮重试
// （登记本来就有 TTL，不必一次点错就作废）。
// 重复点击顶多多出几张确认卡；真正执行仍由 telegramRun 的
// 单次消费确认凭证把关。
func (st *telegramControlState) lookupAlertAction(nonce, chat string) (telegramAction, bool) {
	st.Lock()
	defer st.Unlock()
	v, ok := st.Alerts[nonce]
	if !ok {
		return telegramAction{}, false
	}
	if time.Now().After(v.Until) {
		delete(st.Alerts, nonce)
		return telegramAction{}, false
	}
	// 动作只对登记时的那个 chat 有效 —— 防止别人用自己的会话
	// 拿到 nonce 后操作。
	if v.Chat != chat {
		return telegramAction{}, false
	}
	return v.Action, true
}

// alertActionButton 是一个待登记的按钮。
type alertActionButton struct {
	Text   string
	Action telegramAction
}

// alertActionRows 为一条告警生成处置按钮。
//
// chat 显式传入而不是内部去查：调用方（notifyTelegram）已经查过 chat 了，
// 再查一遍既多余，又让这个纯逻辑函数依赖数据库、没法单独测试。
//
// 返回空切片表示这条通知不该带按钮（恢复类、缺 ID 等情况）。
func (s *Server) alertActionRows(ctx context.Context, chat string, ev alertNotification) [][]map[string]string {
	// 只有「故障态」事件才需要处置动作。
	switch ev.Event {
	case "triggered", "reopened", "repeated":
	default:
		return nil
	}
	if ev.AlertID == "" {
		return nil
	}
	// 群组 / 频道（chat ID 以 - 开头）不提供写操作回调按钮；若配置了外部面板地址则仅展示打开面板按钮。
	if !telegramPrivateOwner(chat, chat, chat) {
		if panelURL := telegramPanelURL(); panelURL != "" {
			return [][]map[string]string{{{"text": "🔗 打开面板", "url": panelURL}}}
		}
		return nil
	}

	var buttons []alertActionButton

	// 确认告警：所有故障类告警都有。
	buttons = append(buttons, alertActionButton{
		Text:   "✅ 确认告警",
		Action: telegramAction{Method: "POST", Path: "/api/alerts/" + ev.AlertID + "/ack"},
	})

	if ev.NodeID != "" {
		// 节点级告警：下线这一台节点。
		buttons = append(buttons, alertActionButton{
			Text:   "🚫 下线此节点",
			Action: telegramAction{Method: "DELETE", Path: "/api/nodes/" + ev.NodeID},
		})
		// 「永久下线」：机器已退租/已报废，节点侧不可能再连上，
		// 买家账号的物理注销**永远做不了** —— 与其把账号永远挂在
		// 「待补删」里，不如直接结案。查询参数 decommissioned=1
		// 是现有 deleteNode 已支持的语义，不是新造的开关。
		buttons = append(buttons, alertActionButton{
			Text:   "⛔ 永久下线此节点",
			Action: telegramAction{Method: "DELETE", Path: "/api/nodes/" + ev.NodeID + "?decommissioned=1"},
		})
	} else if ev.ServerID != "" {
		// 主机级告警：先给该主机上每个节点一个下线按钮
		//（主机整体下线要求其上已无在役节点，见 deleteServer）。
		buttons = append(buttons, s.serverAlertNodeButtons(ctx, ev.ServerID)...)
		buttons = append(buttons, alertActionButton{
			Text:   "🖥 下线此主机",
			Action: telegramAction{Method: "DELETE", Path: "/api/servers/" + ev.ServerID},
		})
	}

	if len(buttons) == 0 {
		return nil
	}

	// 登记动作 → 换成带 nonce 的按钮。
	rows := [][]map[string]string{}
	for _, b := range buttons {
		nonce := s.telegramControl.registerAlertAction(chat, b.Action)
		if nonce == "" {
			continue
		}
		rows = append(rows, []map[string]string{tgButton(b.Text, "tg:al:"+nonce)})
	}
	if len(rows) == 0 {
		return nil
	}
	// 面板入口按钮（仅在已配置外部地址时展示，不经过服务端，不涉及写操作）。
	if panelURL := telegramPanelURL(); panelURL != "" {
		rows = append(rows, []map[string]string{{"text": "🔗 打开面板", "url": panelURL}})
	}
	// 记一行日志：按钮生成了、随消息发出去了。
	//
	// 为什么值得记：「按钮没出现」是个很难查的问题 ——
	// 面板这边一切正常（按钮生成了、推送成功了），Telegram 那边
	// 却看不到按钮，日志里只有一句「推送失败」或干脆什么都没有。
	// 有这行日志，至少能区分「按钮压根没生成」与「生成了但没送达」。
	//
	// 只记按钮的**文字与回调前缀**，不记目标 ID，也不记任何密钥。
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		for _, btn := range row {
			text := btn["text"]
			if data := btn["callback_data"]; data != "" {
				text += "(" + strings.SplitN(data, ":", 3)[0] + ":…)"
			}
			labels = append(labels, text)
		}
	}
	// logger 为 nil 时不能调 Info —— 那会 panic，
	// 而告警推送跑在独立 goroutine 里，panic 会连带打断整条推送链路。
	if s.logger != nil {
		s.logger.Info("telegram: 告警附带处置按钮", "event", ev.Event, "type", ev.AlertType,
			"target", ev.Target, "按钮数", len(rows), "按钮", strings.Join(labels, " / "))
	}
	return rows
}

// serverAlertNodeButtons 列出主机级告警时该主机上的在役节点，
// 各给一个「下线此节点」按钮。
//
// 上限 4 个：Telegram 消息的按钮行数有限，而且一台主机上挂十几个节点时
// 「整台退租」才是主要诉求，逐个点按钮反而不是主要路径。
func (s *Server) serverAlertNodeButtons(ctx context.Context, serverID string) []alertActionButton {
	if s.db == nil {
		return nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := s.db.Query(queryCtx, `
SELECT id::text, name FROM hysteria_nodes
WHERE server_id=$1::uuid AND deleted_at IS NULL
ORDER BY name, id LIMIT 4`, serverID)
	if err != nil {
		s.logger.Warn("telegram: 查主机上的节点失败", "server_id", serverID, "error", err)
		return nil
	}
	defer rows.Close()
	var out []alertActionButton
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			s.logger.Warn("telegram: 读取主机节点列表失败", "server_id", serverID, "error", err)
			return nil
		}
		label := name
		if len([]rune(label)) > 20 {
			label = string([]rune(label)[:20]) + "…"
		}
		out = append(out, alertActionButton{
			Text:   "🚫 下线节点 " + label,
			Action: telegramAction{Method: "DELETE", Path: "/api/nodes/" + id},
		})
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("telegram: 主机节点列表读取不完整", "server_id", serverID, "error", err)
		return nil
	}
	return out
}

// sendTelegramRich 发一条带内联按钮的消息，**带一次重试**。
//
// 为什么不用 telegramRich：它直接调 telegramCall，网络抖一下这条
// 告警就永远丢了 —— 而告警推送恰恰是最不能丢的那类消息
// （丢了就是「面板知道、运维不知道」）。
// 告警推送本身已有 15 秒预算，一次退避重试（约 2 秒）完全塞得下。
func (s *Server) sendTelegramRich(ctx context.Context, chat, text string, rows [][]map[string]string) error {
	token := s.telegramToken()
	if token == "" || chat == "" {
		return errors.New("未配置 Telegram Bot Token 或 Chat ID")
	}
	payload := map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}
	if len(rows) > 0 {
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	return sendWithOneRetry(ctx, func(attemptCtx context.Context) error {
		return telegramCall(attemptCtx, token, "sendMessage", payload, nil)
	})
}

// telegramAlertCallback 处理处置按钮的点击。
//
// 关键：**不直接执行**。取出登记的动作后交给 telegramRun，
// 由它生成确认卡（两分钟有效、只执行一次），用户再点「确认执行」
// 才真正落地。危险动作（删除节点/主机）在执行时会带上
// X-Confirm-Dangerous: yes，与网页面板的护栏完全一致。
func (s *Server) telegramAlertCallback(ctx context.Context, chat, sender, data string) {
	nonce := strings.TrimPrefix(data, "tg:al:")
	action, ok := s.telegramControl.lookupAlertAction(nonce, chat)
	if !ok {
		_ = s.sendTelegram(ctx, chat,
			"⌛ 这个处置按钮已失效（超过 "+fmt.Sprint(int(telegramAlertActionTTL.Minutes()))+
				" 分钟）。请等下一条告警，或到面板里操作。")
		return
	}
	s.telegramRun(ctx, chat, sender, action, false)
}
