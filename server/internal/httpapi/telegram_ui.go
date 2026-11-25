package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"nodeops/server/internal/ippurity"
	"regexp"
	"sort"
	"strings"
)

func telegramBottomKeyboard() map[string]any {
	rows := [][]map[string]string{}
	for _, r := range [][]string{{"📊 面板总览", "🌐 节点管理"}, {"🖥 主机管理", "🎫 订阅管理"}, {"🛡 检测与报表", "🔌 3X-UI面板"}, {"🛠 任务与巡检", "🚨 告警中心"}, {"⚙️ 系统设置", "🏠 主菜单"}, {"🧩 全部功能", "🙈 收起菜单"}} {
		row := []map[string]string{}
		for _, v := range r {
			row = append(row, map[string]string{"text": v})
		}
		rows = append(rows, row)
	}
	return map[string]any{"keyboard": rows, "resize_keyboard": true, "is_persistent": true, "one_time_keyboard": false, "input_field_placeholder": "点击下方菜单选择功能"}
}
func (s *Server) telegramBottomMenu(ctx context.Context, chat string) error {
	return telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "sendMessage", map[string]any{"chat_id": chat, "text": "🏠 Teyir 控制台\n菜单在输入框下方。点击按钮选择功能，无需输入接口或 JSON。", "reply_markup": telegramBottomKeyboard()}, nil)
}
func (s *Server) telegramFriendlyText(ctx context.Context, chat, sender, text string) bool {
	switch text {
	case "🧩 全部功能":
		s.telegramAllMenu(ctx, chat)
	case "🙈 收起菜单", "收起菜单", "/hide":
		s.telegramHideMenu(ctx, chat)
	case "🏠 主菜单", "主菜单", "菜单":
		s.telegramControl.Lock()
		delete(s.telegramControl.Drafts, chat)
		s.telegramControl.Unlock()
		s.telegramBottomMenu(ctx, chat)
	case "📊 面板总览":
		s.handleTelegramMessage(ctx, chat, "/status")
	case "🌐 节点管理":
		s.telegramPick(ctx, chat, "nodes")
	case "🖥 主机管理":
		s.telegramPick(ctx, chat, "servers")
	case "🎫 订阅管理":
		s.telegramPick(ctx, chat, "subs")
	case "🛡 检测与报表":
		s.telegramRich(ctx, chat, "🛡 检测与报表\n请选择要查看的结果：", [][]map[string]string{{tgButton("🛡 IP纯净度", "ux:q:purity"), tgButton("🎬 流媒体解锁", "ux:q:unlock")}, {tgButton("💰 收益报表", "ux:q:profit"), tgButton("🧱 被墙检测", "ux:q:block")}})
	case "🔌 3X-UI面板":
		s.telegramRun(ctx, chat, sender, telegramAction{Method: "GET", Path: "/api/xui/panels"}, false)
	case "🛠 任务与巡检":
		s.telegramRich(ctx, chat, "🛠 任务与巡检", [][]map[string]string{{tgButton("运维任务", "ux:q:tasks"), tgButton("定时巡检", "ux:q:schedules")}, {tgButton("待补删账号", "ux:q:orphans")}})
	case "🚨 告警中心":
		s.handleTelegramMessage(ctx, chat, "/alerts")
	case "⚙️ 系统设置":
		rows := [][]map[string]string{{tgButton("自动下线设置", "ux:q:autohide"), tgButton("定时巡检", "ux:q:schedules")}}
		if panelURL := telegramPanelURL(); panelURL != "" {
			rows = append(rows, []map[string]string{{"text": "打开网页设置", "url": panelURL}})
		}
		s.telegramRich(ctx, chat, "⚙️ 系统设置\n选择要查看的设置；密钥、文件与实时终端请在网页工作台操作。", rows)
	default:
		return false
	}
	return true
}

var telegramUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) telegramPick(ctx context.Context, chat, kind string) {
	queries := map[string]string{"nodes": "SELECT id::text,name FROM hysteria_nodes WHERE deleted_at IS NULL ORDER BY name LIMIT 40", "servers": "SELECT id::text,name FROM servers WHERE deleted_at IS NULL ORDER BY name LIMIT 40", "subs": "SELECT id::text,label FROM subscription_tokens WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 40"}
	q, ok := queries[kind]
	if !ok {
		return
	}
	rows, err := s.db.Query(ctx, q)
	if err != nil {
		_ = s.sendTelegram(ctx, chat, "暂时无法读取列表，请稍后再试。")
		return
	}
	defer rows.Close()
	buttons := [][]map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			_ = s.sendTelegram(ctx, chat, "读取对象列表失败，请稍后重试。")
			return
		}
		if name == "" {
			name = "未命名"
		}
		buttons = append(buttons, []map[string]string{tgButton(name, "ux:p:"+kind+":"+id)})
	}
	if err := rows.Err(); err != nil {
		_ = s.sendTelegram(ctx, chat, "读取对象列表失败，请稍后重试。")
		return
	}
	text := "请选择要管理的对象："
	if len(buttons) == 0 {
		text = "暂无可管理的对象。"
	} else if len(buttons) == 40 {
		text += "\n最多显示前40项，完整列表请在网页查看。"
	}
	_ = s.telegramRich(ctx, chat, text, buttons)
}
func (s *Server) telegramFriendlyCallback(ctx context.Context, chat, sender, data string) bool {
	if !strings.HasPrefix(data, "ux:") {
		return false
	}
	p := strings.Split(data, ":")
	if len(p) < 3 {
		return true
	}
	if p[1] == "q" {
		if path := telegramQuickPaths[p[2]]; path != "" {
			s.telegramRun(ctx, chat, sender, telegramAction{Method: "GET", Path: path}, false)
		}
		return true
	}
	if len(p) != 4 || !telegramUUID.MatchString(p[3]) && p[1] == "p" {
		return true
	}
	if p[1] == "p" {
		kind, id := p[2], p[3]
		buttons := [][]map[string]string{}
		if kind == "nodes" {
			buttons = [][]map[string]string{{tgButton("📊 节点详情", "ux:n:"+id+":info"), tgButton("👥 节点用户", "ux:n:"+id+":users")}, {tgButton("🛡 IP纯净度", "ux:n:"+id+":purity"), tgButton("🎬 解锁状态", "ux:n:"+id+":unlock")}, {tgButton("🔄 更新门户", "ux:n:"+id+":upgrade"), tgButton("🔌 WARP开关", "ux:n:"+id+":warp")}, {tgButton("Reality开关", "ux:n:"+id+":reality"), tgButton("⚠️ 重启节点主机", "ux:n:"+id+":reboot")}}
		}
		if kind == "servers" {
			buttons = [][]map[string]string{{tgButton("📊 主机详情", "ux:s:"+id+":info"), tgButton("⚙️ 服务状态", "ux:s:"+id+":services")}, {tgButton("🔌 测试SSH连接", "ux:s:"+id+":ssh")}}
		}
		if kind == "subs" {
			buttons = [][]map[string]string{{tgButton("✅ 启用订阅", "ux:b:"+id+":on"), tgButton("⏸ 停用订阅", "ux:b:"+id+":off")}, {tgButton("🔧 账号对账", "ux:b:"+id+":reconcile"), tgButton("🔄 换新订阅链接", "ux:b:"+id+":rotate")}}
		}
		name := s.telegramResourceName(ctx, kind, id)
		_ = s.telegramRich(ctx, chat, "已选择："+name+"\n请选择操作；修改类操作还会要求确认。", buttons)
		return true
	}
	id, op := p[2], p[3]
	if !telegramUUID.MatchString(id) {
		return true
	}
	a := telegramAction{Method: "GET"}
	switch p[1] {
	case "n":
		reads := map[string]string{"info": "overview", "users": "users", "purity": "ip-purity", "unlock": "ip-purity"}
		if suffix := reads[op]; suffix != "" {
			a.Path = "/api/nodes/" + id + "/" + suffix
		} else {
			verbs := map[string]string{"upgrade": "upgrade_portal", "warp": "warp_toggle", "reality": "reality_toggle", "reboot": "reboot"}
			v := verbs[op]
			if v == "" {
				return true
			}
			a.Method = "POST"
			a.Path = "/api/nodes/" + id + "/portal-action"
			a.Body, _ = json.Marshal(map[string]string{"verb": v})
		}
	case "s":
		if op == "info" {
			a.Path = "/api/servers/" + id
		} else if op == "services" {
			a.Path = "/api/servers/" + id + "/services"
		} else if op == "ssh" {
			a.Method = "POST"
			a.Path = "/api/servers/" + id + "/test-ssh"
			a.Body = json.RawMessage(`{}`)
		} else {
			return true
		}
	case "b":
		a.Path = "/api/subscriptions/" + id
		if op == "on" || op == "off" {
			a.Method = "PATCH"
			a.Body, _ = json.Marshal(map[string]bool{"enabled": op == "on"})
		} else if op == "reconcile" || op == "rotate" {
			a.Method = "POST"
			a.Path += "/" + op
			a.Body = json.RawMessage(`{}`)
		} else {
			return true
		}
	default:
		return true
	}
	if p[1] == "n" && op == "unlock" {
		a.Path = "/api/unlock?node_id=" + url.QueryEscape(id)
	}
	s.telegramRun(ctx, chat, sender, a, false)
	return true
}
func (s *Server) telegramResourceName(ctx context.Context, kind, id string) string {
	q := map[string]string{"nodes": "SELECT name FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL", "servers": "SELECT name FROM servers WHERE id=$1::uuid AND deleted_at IS NULL", "subs": "SELECT label FROM subscription_tokens WHERE id=$1::uuid"}[kind]
	name := "当前对象"
	if q != "" && s.db != nil {
		// 2026-11-06：读不到名字（数据库错误、对象已删）时给出 ID 前缀，不能只写一句「当前对象」——
		// 确认卡上看不出要操作的是哪一台，正是确认这一步要防的。
		if err := s.db.QueryRow(ctx, q, id).Scan(&name); err != nil {
			name = "ID " + shortID(id) + "（名称读取失败）"
		}
	}
	return name
}

// Telegram 日常回执只展示可读信息，绝不把原始 JSON/供应商原始数据刷进聊天。
func telegramFriendlyResult(a telegramAction, status int, raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "操作已返回。完整内容请在网页查看。"
	}
	if status == 202 {
		if m, ok := v.(map[string]any); ok {
			for _, k := range []string{"job_id", "task_id", "id"} {
				if id, ok := m[k].(string); ok && id != "" {
					return "任务已受理，正在后台运行。任务号：" + id + "\n请在运维任务中查看进度；受理不代表完成。"
				}
			}
		}
		return "操作已受理，尚未完成，请稍后查看任务状态。"
	}
	if u, err := url.Parse(a.Path); err == nil && u.Path == "/api/unlock" && u.Query().Get("node_id") != "" {
		if m, ok := v.(map[string]any); ok {
			if items, ok := m["items"].([]any); ok {
				filtered := []any{}
				for _, it := range items {
					if row, ok := it.(map[string]any); ok && row["node_id"] == u.Query().Get("node_id") {
						filtered = append(filtered, it)
					}
				}
				m["items"] = filtered
			}
		}
	}
	if status >= 400 {
		if m, ok := v.(map[string]any); ok {
			if e, ok := m["error"].(string); ok {
				return "未完成：" + e
			}
		}
		return "操作未完成，请稍后重试或查看面板日志。"
	}
	labels := map[string]string{"user_id": "用户", "node_name": "节点", "server_name": "主机", "name": "名称", "label": "名称", "ip": "IP", "host": "地址", "status": "状态", "enabled": "启用", "fraud_score": "风险分", "connection_type": "线路类型", "is_dual_isp": "双ISP", "is_native": "原生IP", "country": "地区", "region": "地区", "subscribers": "订阅数", "revenue": "收入", "cost": "成本", "profit": "利润", "checked_at": "检测时间", "error": "异常", "interval": "间隔", "running": "运行中", "total": "总计", "online": "在线", "severity": "级别", "title": "内容", "type": "类型", "verb": "操作", "message": "说明", "count": "数量"}
	containers := map[string]bool{"purity": true, "native": true, "results": true, "totals": true, "revenue": true, "cost": true, "profit": true, "bot": true}
	var b strings.Builder
	var walk func(any, int)
	walk = func(x any, depth int) {
		if depth > 4 {
			return
		}
		switch m := x.(type) {
		case []any:
			for i, item := range m {
				if i == 12 {
					fmt.Fprintf(&b, "\n其余 %d 项请在面板查看。\n", len(m)-i)
					break
				}
				if i > 0 {
					b.WriteString("\n")
				}
				walk(item, depth+1)
			}
		case map[string]any:
			for _, key := range []string{"items", "nodes", "subscriptions", "servers", "tasks", "alerts", "schedules", "users", "panels", "inbounds", "services", "logins"} {
				if list, ok := m[key].([]any); ok {
					if len(list) == 0 {
						b.WriteString("暂无记录。\n")
					} else {
						walk(list, depth+1)
					}
					return
				}
			}
			keys := []string{}
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				value := m[k]
				label := labels[k]
				if value == nil {
					if k == "fraud_score" {
						b.WriteString("风险分：未检测\n")
					}
					continue
				}
				switch z := value.(type) {
				case string:
					if label != "" && z != "" {
						text := map[string]string{"online": "在线", "offline": "离线", "active": "生效中", "fulfilled": "已交付", "expired": "已到期", "pending": "待处理", "success": "成功", "failed": "失败", "open": "未处理", "residential": "住宅", "hosting": "机房", "yes": "支持", "no": "不支持", "unknown": "未知", "upgrade_portal": "更新节点门户", "warp_toggle": "切换 WARP", "reality_toggle": "切换 Reality", "reboot": "重启节点主机"}[z]
						if text == "" {
							text = z
						}
						r := []rune(text)
						if len(r) > 120 {
							r = append(r[:120], []rune("…")...)
						}
						fmt.Fprintf(&b, "%s：%s\n", label, string(r))
					}
				case bool:
					if label != "" {
						text := "否"
						if z {
							text = "是"
						}
						fmt.Fprintf(&b, "%s：%s\n", label, text)
					}
				case float64:
					if len(k) == 3 && k == strings.ToUpper(k) {
						fmt.Fprintf(&b, "%s %.2f\n", k, z)
					}
					if label != "" {
						fmt.Fprintf(&b, "%s：%g\n", label, z)
					}
				default:
					if containers[k] {
						if label != "" {
							fmt.Fprintf(&b, "%s：\n", label)
						}
						walk(z, depth+1)
					}
				}
			}
		}
	}
	walk(v, 0)
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "接口已返回，但没有可展示的结果；请在网页核对状态。"
	}
	r := []rune(out)
	if len(r) > 2400 {
		out = string(r[:2400]) + "\n内容较多，其余请在面板查看。"
	}
	return out
}

func (s *Server) telegramActionText(ctx context.Context, a telegramAction) string {
	// 2026-11-06：先拆出路径与查询串。原来直接按「/」切整个 a.Path：带查询串时第三段是「<uuid>?decommissioned=1」，
	// 过不了 UUID 校验，「对象：」整行消失；查询参数也不显示 —— 「⛔ 永久下线此节点」的确认卡只写「删除 · 节点」，
	// 比普通下线给的信息还少，而它会直接结案所有待补删账号。
	path, rawQuery, _ := strings.Cut(a.Path, "?")
	title := telegramRouteTitle(a.Method + " " + path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 2 && telegramUUID.MatchString(parts[2]) {
		kind := parts[1]
		if kind == "subscriptions" {
			kind = "subs"
		}
		if kind == "nodes" || kind == "servers" || kind == "subs" {
			title += "\n对象：" + s.telegramResourceName(ctx, kind, parts[2])
		}
	}
	if rawQuery != "" {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return title + "\n参数：" + rawQuery
		}
		if a.Method == http.MethodDelete && q.Get("decommissioned") == "1" {
			title += "\n⛔ 永久下线：机器已退租 / 报废，不再尝试注销节点上的买家账号，相关待补删记录直接结案"
		}
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			title += "\n参数 " + k + "=" + strings.Join(q[k], ",")
		}
	}
	return title
}

func telegramRemoveKeyboard() map[string]any { return map[string]any{"remove_keyboard": true} }
func (s *Server) telegramHideMenu(ctx context.Context, chat string) error {
	return telegramCall(ctx, s.purityKeys.Get(ippurity.KeyTelegram), "sendMessage", map[string]any{"chat_id": chat, "text": "菜单已收起。发送 /menu 可重新展开。", "reply_markup": telegramRemoveKeyboard()}, nil)
}

// telegramQuickPaths 是菜单按钮「ux:q:<key>」对应的只读接口。
//
// 2026-11-03：原来还有「机器人状态」(/api/settings/telegram) 与「登录记录」(/api/auth/logins)，
// 两者都在 telegramForbiddenPrefixes 里（有意禁止），按钮每次点都回「该路径不是已登记的面板管理接口」。
// 测试 TestTelegramQuickButtonsAreReachable 保证这里的每一项都真能调用。
var telegramQuickPaths = map[string]string{"purity": "/api/ip-purity", "unlock": "/api/unlock", "profit": "/api/reports/profit", "block": "/api/block-checks", "tasks": "/api/tasks", "schedules": "/api/schedules", "orphans": "/api/orphans", "autohide": "/api/nodes/auto-hide"}
