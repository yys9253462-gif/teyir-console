package httpapi

// alertnotify.go — 把告警推出去（2026-10-04 加）。
//
// 为什么需要：告警系统本身**早已完整** —— 条件判定、去重、重现标记、
// 事件时间线、自动 resolve 都在 manage.go 里跑得好好的。
// 唯独缺「发出去」这一步：告警只在面板里躺着，运维必须主动登录才看得到。
// 「节点半夜挂了」这种最需要知道的事，恰恰是没人登录的时候发生的。
//
// 设计取舍（每条都是刻意选的）
// ---------------------------
//  1. **通用 Webhook，不做各家 IM 的专用适配器**。
//     钉钉 / 企业微信 / 飞书 / Telegram Bot 的 incoming webhook 都是
//     「POST 一个 JSON」，差别只在字段名。与其写四个适配器（每个都要跟着
//     对方的字段变更维护），不如发一份**信息完整、字段稳定的 JSON**，
//     并在配置里允许自定义请求体模板 —— 真要对接某家时改模板即可。
//     这是「一次适配全部」与「零维护」之间的平衡点。
//
//  2. **异步发送**。告警的产生路径是采集器 30 秒一轮的同步循环；
//     如果在这里等 webhook 响应，对方慢一下就拖垮整轮采集。
//
//  3. **失败只记日志，绝不影响告警本身**。
//     通知是增强能力，不是关键路径。webhook 打不通不该让告警丢进数据库这一步失败。
//
//  4. **节流**。采集器每 30 秒重算一次条件，同一条告警会持续存在。
//     不节流的话「节点离线」会在几小时内推几千条。
//     这里按 (告警类型 + 目标) 做时间窗去重 —— 与数据库层的 status 去重互补：
//     数据库保证「一条告警只有一行」，这里保证「一条告警不会反复刷屏」。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"nodeops/server/internal/portaltls"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// alertWebhookKey 是 system_settings 里的配置键。
	alertWebhookKey = "alert_webhook_url"
	// alertWebhookTimeout 单次投递上限。3 秒是刻意的短 ——
	// 投递失败也只是少一条通知，不值得为它挂住 30 秒。
	alertWebhookTimeout = 3 * time.Second
	// alertNotifyCooldown 同一目标同一类型的最短重发间隔。
	// 1 小时：足以覆盖「节点半夜挂了」这类场景，又不会刷屏。
	alertNotifyCooldown = time.Hour
	// alertNewTargetQuiet 新主机 / 新节点的静默期：安装、采集刚起步时状态必然抖动。
	alertNewTargetQuiet = 30 * time.Minute
)

// alertNotifyGrace 触发后等多久再推送（期间恢复就不推）。是变量以便测试缩短。
var alertNotifyGrace = 10 * time.Minute

// alertNotifyGraceByType 是**按告警类型**的静默期（2026-10-24 加）。
//
// 为什么要有它：原来所有类型一律 10 分钟。这个值对「端口抖动」是合理的
// （节点端口不通可能是探测抖动，10 分钟内恢复就不该打扰人），但对
// **整机失联**太长 —— SSH 完全连不上基本只有两种结局：机器宕了，或已退租。
// 机器不会在 3 分钟内自己恢复，而 10 分钟的沉默意味着：故障发生了、
// 面板知道、但运维完全不知情（2026-10-23 美国-阿什本实测：19:33 失联，
// 直到 23:19 巡检时才发现主机已宕机近 4 小时）。
//
// 分级原则：**故障越"确定不可自愈"，门槛越短**。
//   - server_offline：SSH 全断 —— 不会自愈，3 分钟
//   - server_stale  ：主机可达但采集停滞 —— 采集器可能卡住，5 分钟
//   - node_offline / node_blocked：端口不通 / 疑似被墙 —— 可能抖动，维持 10 分钟
//   - 其余类型：一律走默认 10 分钟
var alertNotifyGraceByType = map[string]time.Duration{
	"server_offline": 3 * time.Minute,
	"server_stale":   5 * time.Minute,
}

// alertGraceFor 按告警类型取静默期；未登记的类型用默认值。
func alertGraceFor(alertType string) time.Duration {
	if d, ok := alertNotifyGraceByType[alertType]; ok {
		return d
	}
	return alertNotifyGrace
}

// alertEscalateAfter 是「持续未送达」的升级推送门槛（2026-10-24 加）。
//
// 解决的问题（告警黑洞）：一条 open 告警在**状态首次出现**的那一刻只推送一次。
// 若那一刻推送失败、或被 1 小时冷却吞掉、或恰好落在静默期内被放弃 ——
// 此后这条告警会一直 open，而 reconcile 每轮只 UPDATE last_seen_at，
// **不再产生任何新事件、也不再重试推送**。于是：面板知道故障、数据库里有记录、
// 运维却永远不会收到 —— 一条持续 7 小时的宕机全程静默。
//
// 修法：持续超过该门槛仍未推送成功（notified_at IS NULL）的告警，
// 重新触发一次推送。门槛取 15 分钟：既能等到首次推送自然失败后的重试窗口，
// 又不会与 1 小时冷却打架（冷却按 类型|目标|事件 走，重试是同一事件，
// 仍受冷却保护，不会刷屏）。
const alertEscalateAfter = 15 * time.Minute

// alertResolveGrace 是「条件已消失，但先别急着 resolved」的观察窗口
// （2026-10-24 审计 P0-2）。
//
// 原来条件一消失就立刻 resolved，下一次 reconcile 只要条件又出现，
// 就是一条新的 alert + 一条 reopened 事件。一次 30 秒的 SSH 抖动
// 恰好横跨两次 reconcile，就会造出「开→关→开」三条事件。
//
// 实测后果：alert_events 里 reopened 382 次而 triggered 只有 64 次。
// 现在给自己一个 90 秒（≈3 个采集周期）的观察期：期间条件恢复就不动它
// （连同 first_seen_at 一起保留，时间线仍是连续的），真正消失了才 resolve。
//
// 为什么不用更长：resolve 通知的及时性对运维有用，拖太久会让人以为
// 故障还没好。90 秒足够滤掉单次抖动，又不影响真实恢复的体感。
const alertResolveGrace = 90 * time.Second

// alertMergeWindow 是同一条告警「短时间反复开合」的归并窗口（2026-10-24 审计 P0-2）。
//
// 与 alertResolveGrace 的分工：resolveGrace 拦住的是「还没来得及 resolve
// 就又回来了」；本条拦住的是「已经 resolve 了，一分钟后又开一条新的」。
// 后者在库里留下的是真实的多行 alert + 多条 reopened 事件，前端看到的
// 是同一台机器反复出现、反复消失 —— 而实际上机器一直没恢复。
//
// 判据：同一 (类型 + 主机 + 节点) 在窗口内刚 resolve 过，就**复用那一行**、
// 把它重新置回 open，事件记 reopened 但不新建告警。
// 这样「告警条数」与「真实故障数」才对得上，看板上不再是一屏重复项。
//
// 取 10 分钟与 alertNotifyGrace 默认值一致：短于它，说明这条故障
// 在通知静默期里就回来了，运维不该收到第二封「新故障」邮件。
const alertMergeWindow = 10 * time.Minute

// alertNotifier 持有节流状态。
//
// 放在 Server 结构体上而不是包级变量：包级变量会让测试之间互相污染
// （一个用例发了告警，另一个用例就被节流掉，表现为"偶发失败"）。
type alertNotifier struct {
	mu   sync.Mutex
	last map[string]time.Time
	sent map[string]bool // 已推送过「告警」的 alertID，用来决定要不要推「恢复」

	// lookup / persist 是 alerts.notified_at 的读写（2026-10-23 加，见 049 迁移）。
	// 内存里的 sent 重启就丢，落库后重启前推送过的告警恢复时仍能推「恢复」。
	// 为 nil 时走 Server.db；测试注入桩函数，免得依赖数据库。
	lookup  func(ctx context.Context, alertID string) (bool, error)
	persist func(ctx context.Context, alertID string) error
}

func newAlertNotifier() *alertNotifier {
	return &alertNotifier{last: map[string]time.Time{}, sent: map[string]bool{}}
}

func (n *alertNotifier) markSent(alertID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent[alertID] = true
}

// takeSent 报告这条告警是否推送过（只看内存）；clear=true（恢复）时顺便清掉记录。
func (n *alertNotifier) takeSent(alertID string, clear bool) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	ok := n.sent[alertID]
	if clear {
		delete(n.sent, alertID)
	}
	return ok
}

// allow 判断这个 key 现在能不能发。
func (n *alertNotifier) allow(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.last) > 256 {
		for k, ts := range n.last {
			if now.Sub(ts) >= alertNotifyCooldown {
				delete(n.last, k)
			}
		}
	}
	if t, ok := n.last[key]; ok && now.Sub(t) < alertNotifyCooldown {
		return false
	}
	n.last[key] = now
	return true
}

// admit 是节流判定：「恢复」永不节流 —— 它只会在对应「告警」确实推送过之后才走到这里
// （见 wasAnnounced），本身就不会刷屏；而按 1 小时冷却节流会把同一目标一小时内的
// 第二次恢复吞掉，运维会以为节点还没好。其余事件按 类型 + 目标 + 事件 做冷却。
func (n *alertNotifier) admit(ev alertNotification, now time.Time) bool {
	if ev.Event == "resolved" {
		return true
	}
	return n.allow(ev.AlertType+"|"+ev.Target+"|"+ev.Event, now)
}

// wasAnnounced 判断这条告警的「告警」是否推送过：先查内存，查不到再回落查
// alerts.notified_at（重启后内存为空）。查库失败只记日志、按「没推送过」处理 ——
// 宁可少一条恢复，也不在迁移未上线等情况下把每条恢复都推出去。
func (s *Server) wasAnnounced(ctx context.Context, alertID string, clear bool) bool {
	n := s.alertNotifier
	if n.takeSent(alertID, clear) {
		return true
	}
	lookup := n.lookup
	if lookup == nil {
		if s.db == nil {
			return false
		}
		lookup = func(ctx context.Context, id string) (bool, error) {
			var ok bool
			err := s.db.QueryRow(ctx, `SELECT notified_at IS NOT NULL FROM alerts WHERE id=$1::uuid`, id).Scan(&ok)
			return ok, err
		}
	}
	ok, err := lookup(ctx, alertID)
	if err != nil {
		s.logger.Warn("alert notify: 查 notified_at 失败，按未推送处理", "alert_id", alertID, "error", err)
		return false
	}
	return ok
}

// admitTrigger 对「告警 / 再次出现」做节流判定；只有**真正放行**才记为已推送
// （内存 + notified_at）。原来先记已推送再判节流：被冷却吞掉的那次也算「推送过」，
// 于是运维从没收到「告警」却会收到一条莫名其妙的「恢复」。
// 2026-11-04：只做节流，不再记「已推送」。原来在投递前就 markSent ——
// 投递失败或投递时发现类型已静音（deliverAlert 返回 false），内存里仍记着「推过」，
// 告警恢复时就会单独推一条「✅ 已恢复」，而对应的故障从没告诉过运维。markSent 挪进 markDelivered。
func (s *Server) admitTrigger(ctx context.Context, alertID string, ev alertNotification) bool {
	return s.alertNotifier.admit(ev, time.Now())
}

// markDelivered 在至少一个渠道确认收到后才写 alerts.notified_at。
//
// 2026-11-03：原来 admitTrigger 在投递前就写 notified_at。Telegram / webhook 当时不可用，
// 投递失败只记日志，而 notified_at 已经非空 —— 「持续未送达就升级重推」只看
// notified_at IS NULL，于是永远不会重推，正是它要防的那种全程静默。
//
// 2026-11-04：落库用独立的短超时，不沿用调用方的 ctx。调用方的 5 秒预算在投递**之前**就开始计时，
// 而投递（Telegram 有 15 秒预算）常常就把它耗光：消息已经送到，notified_at 却因 deadline 写不进去，
// 15 分钟后「持续未送达 · 升级告警」把运维已经收到的告警再推一遍。
func (s *Server) markDelivered(ctx context.Context, alertID string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ctx = pctx
	s.alertNotifier.markSent(alertID)
	persist := s.alertNotifier.persist
	if persist == nil {
		if s.db == nil {
			return
		}
		persist = func(ctx context.Context, id string) error {
			_, err := s.db.Exec(ctx, `UPDATE alerts SET notified_at=now() WHERE id=$1::uuid`, id)
			return err
		}
	}
	if err := persist(ctx, alertID); err != nil {
		// 落库失败不影响本次推送，只是重启后这条的「恢复」可能发不出去。
		s.logger.Warn("alert notify: 写 notified_at 失败", "alert_id", alertID, "error", err)
	}
}

// alertNotification 是发出去的载荷。
//
// 字段刻意用**全称而非缩写**：这份 JSON 会被人直接粘到 IM 的模板里，
// 也可能被别的脚本消费，可读性比省几个字节重要。
type alertNotification struct {
	Event      string `json:"event"`       // triggered / reopened / resolved / acknowledged
	AlertType  string `json:"alert_type"`  // node_offline / server_warning …
	Severity   string `json:"severity"`    // warning / critical
	Target     string `json:"target"`      // 节点名或主机名
	Detail     string `json:"detail"`      // 人话描述
	OccurredAt string `json:"occurred_at"` // RFC3339
	PanelURL   string `json:"panel_url,omitempty"`
	// Text 是给 IM 直接显示的整句，省得对方还要拼字段。
	Text string `json:"text"`

	// 下面三个 ID 是 2026-10-24 加的：Telegram 告警要带**处置按钮**，
	// 按钮必须能定位到具体对象，否则点了没法执行。
	//
	// 为什么放在载荷里而不是回调时再查：按钮的 callback_data 只有 64 字节，
	// 塞不下「告警 + 节点 + 主机」三个 UUID；只能带一个主 ID，
	// 其余在推送时查好一并带上。空串表示该对象不存在（例如主机级告警没有节点）。
	AlertID  string `json:"alert_id,omitempty"`
	NodeID   string `json:"node_id,omitempty"`
	ServerID string `json:"server_id,omitempty"`
}

// loadAlertWebhook 读配置。空串表示「没配」，此时静默跳过。
func (s *Server) loadAlertWebhook(ctx context.Context) string {
	var v string
	if err := s.db.QueryRow(ctx,
		`SELECT value FROM system_settings WHERE key=$1`, alertWebhookKey).Scan(&v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

const alertMutedTypesKey = "alert_muted_types"

// alertTypeMuted 仅静默外发；采集、面板告警与事件时间线仍正常记录。
func (s *Server) alertTypeMuted(parent context.Context, kind string) bool {
	if s.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	var raw string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, alertMutedTypesKey).Scan(&raw); err != nil {
		return false
	}
	for _, value := range strings.Split(raw, ",") {
		if strings.TrimSpace(value) == kind {
			return true
		}
	}
	return false
}

// notifyAlert 把一条告警事件推出去。
//
// ⚠️ 必须在**独立 goroutine** 里被调用（或调它的人自己开 goroutine）——
// 它内部会做网络请求。这里用 context.Background() 而不是传入的 ctx：
// 传入的 ctx 属于那次采集循环，循环一结束就被 cancel，
// 会导致"告警刚发出就被掐断"这种极难排查的现象。
func (s *Server) notifyAlert(ev alertNotification) bool {
	if s.alertNotifier == nil {
		return false
	}
	// 节流见 admit：「恢复」不节流，其余按 类型 + 目标 + 事件 冷却。
	if !s.alertNotifier.admit(ev, time.Now()) {
		return false
	}
	s.deliverAlert(ev)
	return true
}

// deliverAlert 不做节流，直接投递（调用方已经过 admit / admitTrigger）。
// 返回是否至少有一个渠道（Telegram 或 webhook）确认收到。
func (s *Server) deliverAlert(ev alertNotification) bool {
	// 再查一次，覆盖已进入静默等待期、随后才被关闭的通知。
	if s.alertTypeMuted(context.Background(), ev.AlertType) {
		return false
	}
	// Telegram 与 Webhook 并行、互不依赖（只配了其中一个也能收到）。
	telegramOK := make(chan bool, 1)
	go func() { telegramOK <- s.notifyTelegram(ev) }()
	webhookOK := s.deliverWebhook(ev)
	return <-telegramOK || webhookOK
}

func (s *Server) deliverWebhook(ev alertNotification) bool {
	// 读配置也要用 background ctx —— 理由同上。
	ctx, cancel := context.WithTimeout(context.Background(), alertWebhookTimeout)
	defer cancel()
	url := s.loadAlertWebhook(ctx)
	if url == "" {
		return false
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		s.logger.Warn("alert notify: 构造请求失败", "error", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	// 带一个显式 UA：部分 IM 网关会拦默认的 Go UA（与 resend 1010 同类问题）。
	req.Header.Set("User-Agent", "hy2-ops-console/alert-notifier")

	resp, err := alertNotifyClient.Do(req)
	if err != nil {
		s.logger.Warn("alert notify: 投递失败", "type", ev.AlertType, "error", "webhook request failed")
		return false
	}
	defer func() {
		// 排空少量响应体以复用 HTTP 长连接。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		s.logger.Warn("alert notify: 对方返回非 2xx",
			"type", ev.AlertType, "status", resp.StatusCode)
		return false
	}
	s.logger.Info("alert notify: 已投递", "type", ev.AlertType,
		"event", ev.Event, "target", ev.Target)
	return true
}

// alertNotifyClient 是投递用的 HTTP 客户端。
//
// 单独一个而不是复用 portalClient：portalClient 有 SSRF 防护
// （拒绝私有地址），而告警 webhook 恰好**经常**指向内网
// （自建的通知服务、局域网里的机器人网关）。复用会被拦掉，
// 且报错是"拒绝私有地址"，看着像配置错了。
func webhookAddrAllowed(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}
func newAlertNotifyClient() *http.Client {
	dialer := &net.Dialer{Timeout: alertWebhookTimeout}
	return &http.Client{Timeout: alertWebhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, errInvalidWebhook("no destination addresses")
			}
			for _, ip := range ips {
				if !webhookAddrAllowed(ip.IP) {
					return nil, errInvalidWebhook("unsafe webhook destination")
				}
			}
			// 全部目标地址通过校验后按 IPv4 优先依次回退拨号，避免双栈地址首个不可达时漏发告警。
			return portaltls.DialCheckedIPs(ctx, dialer, network, port, ips)
		}},
	}
}

var alertNotifyClient = newAlertNotifyClient()
var numericWebhookHost = regexp.MustCompile(`(?i)^(?:0x[0-9a-f]+|[0-9]+)(?:\.(?:0x[0-9a-f]+|[0-9]+))*$`)

// GET /api/alerts/webhook —— 读告警外发配置。
//
// ⚠️ 返回的是**脱敏后的形态**：webhook URL 里常带 token
// （钉钉/企微的 `?...access_token=xxx`、TG 的 `/bot<token>/sendMessage`），
// 而设置页是多个管理员都能看的。只回显 host 与是否已配置，
// 具体 token 一律不回传 —— 与 maskSecret 同一套原则。
func (s *Server) alertWebhookGet(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	raw := s.loadAlertWebhook(r.Context())
	if raw == "" {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "host": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true, "host": urlHost(raw)})
}

// PUT /api/alerts/webhook —— 设置或清空告警外发地址。
//
// 只接受 http(s)，且**不允许**指向本机回环 —— 防止把它当成本机的
// 任意请求发起器（SSRF 的最小防线；内网地址是允许的，
// 因为自建通知服务通常就在内网）。
func (s *Server) alertWebhookPut(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	raw := strings.TrimSpace(input.URL)

	if raw == "" {
		// 清空 = 关闭告警外发。用一个空值而不是删行，让「已配置过但关掉了」
		// 与「从没配过」在库里可区分。
		if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, '', '告警外发 Webhook 地址')
ON CONFLICT (key) DO UPDATE SET value='', updated_at=now()`, alertWebhookKey); err != nil {
			s.databaseError(w, r, err)
			return
		}
		s.audit(r.Context(), r, "set_alert_webhook", "system_setting", alertWebhookKey,
			map[string]any{"cleared": true})
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}

	if err := validateWebhookURL(raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.db.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, '告警外发 Webhook 地址')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`,
		alertWebhookKey, raw); err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 审计里**不写完整 URL** —— 它带 token。只记 host。
	s.audit(r.Context(), r, "set_alert_webhook", "system_setting", alertWebhookKey,
		map[string]any{"host": urlHost(raw)})
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "host": urlHost(raw)})
}

// validateWebhookURL 做最小必要校验。
//
// 刻意**不**复用 portalClient 的 SSRF 防护（那个拒绝一切私有地址）：
// 告警 webhook 经常就指向内网的自建通知服务。这里只挡两件事：
//   - 非 http(s) 协议（file:// 之类）
//   - 指向 127.0.0.1 / localhost —— 那是"让面板替我打本机端口"的入口
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errInvalidWebhook("地址格式不合法")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errInvalidWebhook("只支持 http:// 或 https:// 开头的地址")
	}
	if u.Host == "" {
		return errInvalidWebhook("地址缺少主机名")
	}
	h := strings.ToLower(u.Hostname())
	h = strings.TrimSuffix(h, ".")
	if u.User != nil || u.Fragment != "" {
		return errInvalidWebhook("不接受 URL 用户凭据或 fragment")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return errInvalidWebhook("不能指向本机回环地址")
	}
	if ip := net.ParseIP(h); ip != nil {
		if !webhookAddrAllowed(ip) {
			return errInvalidWebhook("不能指向回环、链路本地或组播地址")
		}
	} else if numericWebhookHost.MatchString(h) {
		return errInvalidWebhook("不接受非规范 IP 地址")
	}
	return nil
}

type errInvalidWebhook string

func (e errInvalidWebhook) Error() string { return string(e) }

// urlHost 取 host（含端口），用于脱敏回显与审计。
// 解析失败返回空串 —— 回显路径不该因为一条坏配置就报错。
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// listAlertEventsHandler 已存在（manage.go）；这里只补 webhook 配置端点。
//
// 抽成纯函数是为了可单测 —— 文案逻辑（尤其是 severity 取词与
// 恢复事件的措辞）不值得靠真机验证。
//
// 参数按 describeAlert 的真实签名传：它区分「主机名 + 节点名」，
// 因为同一台主机上可能挂多个节点，只报主机名会让人不知道是哪个节点的问题。
func buildAlertNotification(eventType, alertType, severity, serverName, host, nodeName string,
	port int, panelURL string, now time.Time) alertNotification {

	detail := describeAlert(alertType, serverName, host, nodeName, port)
	if eventType == "resolved" {
		detail = describeAlertRecovery(alertType, serverName, host, nodeName, port)
	}
	// target 取「更能定位问题的那一个」：节点类告警用节点名，
	// 主机类告警用主机名 —— 两者混用会让运维去错的地方查。
	target := serverName
	if strings.HasPrefix(alertType, "node_") && nodeName != "" {
		target = nodeName
	}

	var text string
	switch eventType {
	case "resolved":
		text = fmt.Sprintf("[恢复] %s —— %s", target, detail)
	case "reopened":
		text = fmt.Sprintf("[再次出现] %s —— %s", target, detail)
	case "acknowledged":
		text = fmt.Sprintf("[已确认] %s —— %s", target, detail)
	case "repeated":
		// 升级重推：强调「这条已经持续一段时间且之前没送达」，
		// 否则运维会以为是重复消息而忽略。措辞必须与前几种明显不同。
		text = fmt.Sprintf("[持续未送达 · 升级告警] %s —— %s（此故障已持续一段时间，此前未成功通知）", target, detail)
	default:
		sev := severity
		if sev == "" {
			sev = "warning"
		}
		text = fmt.Sprintf("[告警] %s —— %s", target, detail)
	}

	sev := severity
	if sev == "" {
		sev = "warning"
	}
	return alertNotification{
		Event:      eventType,
		AlertType:  alertType,
		Severity:   sev,
		Target:     target,
		Detail:     detail,
		OccurredAt: now.UTC().Format(time.RFC3339),
		PanelURL:   panelURL,
		Text:       text,
	}
}
