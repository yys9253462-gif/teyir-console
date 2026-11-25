package httpapi

// 告警处置按钮（telegram_alert.go）的回归测试。
//
// 这一组测试的重点**不是**「按钮能不能生成」，而是**边界**：
// 按钮直接触达「删除节点 / 删除主机」这种不可逆操作，
// 一个越界的按钮就是生产事故。所以重点断言在：
//
//  1. 按钮动作必须落到**现有面板 API**（不另写一套下线逻辑）
//  2. 危险动作必须走**两阶段确认**，不能一点即删
//  3. callback_data **不得携带目标 ID**（防伪造操作别的对象）
//  4. 登记动作**不得跨 chat 使用**（防他人拿到 nonce 后操作）
//  5. 过期动作**必须失效**（防对着已经变化的世界操作）

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 1. 按钮生成

// TestAlertActionsOnlyForFaultEvents 断言只有故障类事件才带处置按钮。
//
// 恢复类通知（resolved）没有可处置的对象 —— 给它一个「下线此节点」
// 按钮，就是在故障已经修好之后还留着一个能把它再弄坏的入口。
func TestAlertActionsOnlyForFaultEvents(t *testing.T) {
	t.Parallel()

	s := &Server{}
	for _, ev := range []alertNotification{
		{Event: "resolved", AlertID: "a", NodeID: "n"},
		{Event: "acknowledged", AlertID: "a", NodeID: "n"},
	} {
		if rows := s.alertActionRows(context.Background(), "chat-1", ev); rows != nil {
			t.Errorf("事件 %q 不该带处置按钮，实际带出 %d 行", ev.Event, len(rows))
		}
	}

	// 故障类必须带出按钮。
	for _, ev := range []alertNotification{
		{Event: "triggered", AlertID: "a", NodeID: "n"},
		{Event: "reopened", AlertID: "a", NodeID: "n"},
		{Event: "repeated", AlertID: "a", NodeID: "n"},
	} {
		if rows := s.alertActionRows(context.Background(), "chat-1", ev); len(rows) == 0 {
			t.Errorf("事件 %q 应带处置按钮，实际没有", ev.Event)
		}
	}
}

// TestAlertActionsRequireAlertID 断言缺 alert_id 时不给按钮。
//
// 没有告警 ID 就构造不出确认告警的路径（/api/alerts/{id}/ack）——
// 与其发一个点了没反应的按钮，不如不发。
func TestAlertActionsRequireAlertID(t *testing.T) {
	t.Parallel()

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{Event: "triggered", NodeID: "n"})
	if rows != nil {
		t.Error("缺 alert_id 时不应生成按钮")
	}
}

// ---------------------------------------------------------------- 2. 动作落点

// TestAlertActionsTargetExistingAPIs 断言按钮动作用的是**现有面板 API**。
//
// 这是本组测试最重要的一条：按钮绝不能自造一套下线逻辑。
// 自造的后果是「面板删节点」与「TG 删节点」两套语义必然分叉 ——
// 一边加了回收凭据的逻辑，另一边没加，TG 一键下线就会留下
// 「钱退了、货还在」的孤儿账号。
//
// 断言方式：动作的 Method+Path 必须能在 server.go 的 panelRoute
// 注册表里找到对应路由。
func TestAlertActionsTargetExistingAPIs(t *testing.T) {
	t.Parallel()

	s := &Server{}
	// 节点级告警：确认告警 + 下线节点 + 永久下线节点。
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	if len(rows) == 0 {
		t.Fatal("没生成按钮")
	}

	registered := panelRoutePatterns(t)
	for _, nonce := range collectNonces(rows) {
		action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
		if !ok {
			t.Fatalf("nonce %s 查不到登记的动作", nonce)
		}
		// 具体路径（带真实 UUID）归一化成注册表里的模式（带 {id}）。
		// 去掉查询参数（?decommissioned=1 仍属于 DELETE /api/nodes/{id}）。
		base := action.Method + " " + normalizeActionPath(action.Path)
		if !registered[base] {
			t.Errorf("按钮动作 %q 不是已注册的面板路由 —— "+
				"处置按钮必须复用现有 API，不能另写一套下线逻辑", base)
		}
	}
}

// normalizeActionPath 把具体路径里的 UUID 换成 {id}，
// 并去掉查询参数 —— 与 server.go 里 panelRoute 的注册形态对齐。
func normalizeActionPath(path string) string {
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if telegramUUID.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	return "/" + strings.Join(parts, "/")
}

// TestAlertPermanentDecommissionUsesExistingParameter 断言「永久下线」
// 复用 deleteNode **已有的** decommissioned 参数，而不是新造开关。
func TestAlertPermanentDecommissionUsesExistingParameter(t *testing.T) {
	t.Parallel()

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	found := false
	for _, nonce := range collectNonces(rows) {
		action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
		if !ok {
			continue
		}
		if action.Method == "DELETE" && strings.Contains(action.Path, "decommissioned=1") {
			found = true
		}
	}
	if !found {
		t.Error("没找到带 decommissioned=1 的删除节点动作 —— " +
			"「永久下线」必须复用 deleteNode 已有参数，而不是新造一个开关")
	}

	// 反向：该参数必须在 deleteNode 里真的被识别，否则就是个哑参数。
	code := stripGoComments(readSourceFile(t, "manage.go"))
	if !strings.Contains(code, `decommissioned`) {
		t.Error("manage.go 里找不到 decommissioned —— 参数名对不上，按钮会静默失效")
	}
}

// ---------------------------------------------------------------- 3. 危险动作必须确认

// TestAlertDangerousActionsNeedConfirmation 断言删除类动作必须走确认。
//
// telegramNeedsConfirm 对非 GET 一律要求确认。这里钉死的是：
// **所有按钮动作里，只要会改状态，就必须过确认卡**。
// 若哪天有人把删除动作改成 GET 绕过确认，这条会报红。
func TestAlertDangerousActionsNeedConfirmation(t *testing.T) {
	t.Parallel()

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	for _, nonce := range collectNonces(rows) {
		action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
		if !ok {
			continue
		}
		if !telegramNeedsConfirm(action) {
			t.Errorf("动作 %s %s 不需要确认 —— "+
				"按钮是危险操作的入口，必须先出确认卡", action.Method, action.Path)
		}
	}
}

// TestAlertCallbackDoesNotExecuteDirectly 断言点击按钮**不直接执行**。
//
// 这条用源码断言：处置按钮的回调必须走 telegramRun（生成确认卡），
// 而不是绕过它。绕过 = 一点即删。
func TestAlertCallbackDoesNotExecuteDirectly(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "telegram_alert.go"))
	if !strings.Contains(code, "s.telegramRun(ctx, chat, sender, action, false)") {
		t.Error("处置按钮回调必须调用 telegramRun 生成确认卡（confirmed=false）—— " +
			"直接执行等于一点即删")
	}
	// 不能出现 confirmed=true 的直接执行
	if strings.Contains(code, "telegramRun(ctx, chat, sender, action, true)") {
		t.Error("处置按钮回调不得以 confirmed=true 直接执行 —— " +
			"那样会跳过确认卡")
	}
}

// ---------------------------------------------------------------- 4. 防伪造

// TestAlertCallbackDataCarriesNoTargetID 断言 callback_data 不含目标 ID。
//
// Telegram 的 callback_data 会原样回传给服务端，**客户端可以伪造**。
// 若 callback_data 里直接带 node_id，别人（哪怕是拿到聊天记录的人）
// 就能构造出「删除任意节点」的按钮。目标 ID 必须只存在服务端登记表里。
func TestAlertCallbackDataCarriesNoTargetID(t *testing.T) {
	t.Parallel()

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	for _, row := range rows {
		for _, btn := range row {
			data := btn["callback_data"]
			if data == "" {
				continue // URL 按钮
			}
			if !strings.HasPrefix(data, "tg:al:") {
				t.Fatalf("callback_data %q 的前缀不对", data)
			}
			nonce := strings.TrimPrefix(data, "tg:al:")
			if strings.Contains(nonce, "11111111") || strings.Contains(nonce, "22222222") {
				t.Errorf("callback_data %q 里含目标 ID —— "+
					"目标 ID 只能存在服务端登记表里，否则可被伪造", data)
			}
			// nonce 必须是随机十六进制，不是可猜的序号。
			if len(nonce) != 16 {
				t.Errorf("nonce %q 长度应为 16（8 字节随机），实际 %d", nonce, len(nonce))
			}
			for _, c := range nonce {
				if !strings.ContainsRune("0123456789abcdef", c) {
					t.Errorf("nonce %q 含非十六进制字符 —— 必须是随机串，不能是可猜的序号", nonce)
					break
				}
			}
		}
	}
}

// TestAlertActionCannotCrossChat 断言登记的动作不能跨 chat 使用。
//
// 场景：A 的机器人消息里有个 nonce，被转发/截图到 B 的会话；
// B 点下去不能生效 —— 否则 B 能操作 A 的资源。
func TestAlertActionCannotCrossChat(t *testing.T) {
	t.Parallel()

	st := &telegramControlState{}
	nonce := st.registerAlertAction("chat-A", telegramAction{Method: "POST", Path: "/api/alerts/x/ack"})
	if nonce == "" {
		t.Fatal("登记失败")
	}
	if _, ok := st.lookupAlertAction(nonce, "chat-B"); ok {
		t.Error("另一个 chat 拿到了动作 —— 必须拒绝跨 chat 使用")
	}
	if _, ok := st.lookupAlertAction(nonce, "chat-A"); !ok {
		t.Error("登记方本人应能取到动作")
	}
}

// TestAlertActionUnknownNonceRejected 断言未知 nonce 被拒。
func TestAlertActionUnknownNonceRejected(t *testing.T) {
	t.Parallel()

	st := &telegramControlState{}
	if _, ok := st.lookupAlertAction("deadbeefdeadbeef", "chat-A"); ok {
		t.Error("不存在的 nonce 不该取到动作")
	}
}

// ---------------------------------------------------------------- 5. 过期

// TestAlertActionExpires 断言过期动作失效。
//
// 对着一个已经变化的世界执行旧动作（比如机器已经恢复、
// 节点已经被别人删了），是危险操作里最典型的一种误伤。
func TestAlertActionExpires(t *testing.T) {
	t.Parallel()

	st := &telegramControlState{}
	nonce := st.registerAlertAction("chat-A", telegramAction{Method: "DELETE", Path: "/api/nodes/x"})
	if nonce == "" {
		t.Fatal("登记失败")
	}
	// 手工把过期时间拨到过去（不改 TTL 常量，避免拖慢测试）。
	st.Lock()
	v := st.Alerts[nonce]
	v.Until = time.Now().Add(-time.Minute)
	st.Alerts[nonce] = v
	st.Unlock()

	if _, ok := st.lookupAlertAction(nonce, "chat-A"); ok {
		t.Error("过期动作不该还能取到 —— 必须拒绝对着变化的世界执行旧动作")
	}
}

// TestAlertActionTTLIsBounded 断言有效期有上界。
//
// 登记表只增不减会漏内存；但 TTL 太长则旧动作长期有效。
func TestAlertActionTTLIsBounded(t *testing.T) {
	t.Parallel()

	if telegramAlertActionTTL <= 0 || telegramAlertActionTTL > 30*time.Minute {
		t.Errorf("处置动作有效期 %v 不合理（应在 0~30 分钟之间）", telegramAlertActionTTL)
	}
}

// ---------------------------------------------------------------- 6. 接线

// TestAlertButtonsAreWiredIntoNotification 断言按钮真的挂进了告警推送。
//
// 只写了 alertActionRows 却没接到 notifyTelegram，等于没做。
// 这正是「永远绿的测试等于没有测试」的典型反面教材。
func TestAlertButtonsAreWiredIntoNotification(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "telegram.go"))
	if !strings.Contains(code, "s.alertActionRows(ctx, chat, ev)") {
		t.Error("告警推送没有调用 alertActionRows —— 按钮生成了但没发出去")
	}
	if !strings.Contains(code, "s.sendTelegramRich(ctx, chat, text, rows)") {
		t.Error("告警推送没有用带按钮的发送方式 —— 按钮随消息发出去了吗")
	}
}

// TestAlertCallbackIsDispatched 断言回调分发接上了 tg:al: 前缀。
func TestAlertCallbackIsDispatched(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "telegram_control.go"))
	if !strings.Contains(code, `strings.HasPrefix(data, "tg:al:")`) {
		t.Error("telegramCallback 没有处理 tg:al: 前缀 —— 按钮点了没反应")
	}
	if !strings.Contains(code, "s.telegramAlertCallback(ctx, chat, sender, data)") {
		t.Error("tg:al: 分支没有调用 telegramAlertCallback")
	}
}

// TestAlertButtonsIncludePanelLink 断言面板 URL 按钮是 URL 按钮。
//
// URL 按钮由 Telegram 客户端直接打开，不经过服务端 —— 里面**不能**
// 带任何需要服务端处理的信息。
func TestAlertButtonsIncludePanelLink(t *testing.T) {
	t.Setenv("PANEL_PUBLIC_URL", "https://panel.example.com")

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	found := false
	for _, row := range rows {
		for _, btn := range row {
			if btn["callback_data"] == "" && btn["url"] == "https://panel.example.com" {
				found = true
			}
		}
	}
	if !found {
		t.Error("配置 PANEL_PUBLIC_URL 后按钮里应包含面板入口（URL 按钮）")
	}

	// 未配置外部地址时不生成无效硬编码外链按钮
	t.Setenv("PANEL_PUBLIC_URL", "")
	t.Setenv("SUBSCRIPTION_PUBLIC_BASE_URL", "")
	rowsNoURL := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	for _, row := range rowsNoURL {
		for _, btn := range row {
			if btn["url"] != "" {
				t.Errorf("未配置 PANEL_PUBLIC_URL 时不应生成外部 URL 按钮，却得到 %q", btn["url"])
			}
		}
	}
}

// TestAlertButtonGenerationIsLogged 断言按钮生成有观测日志。
//
// 「按钮没出现」是极难查的问题：面板这边一切正常（生成了、送出去了），
// Telegram 那边却看不到。有这行日志才能区分
// 「按钮压根没生成」与「生成了但没送达」。
//
// 同时钉死：日志里**不得出现目标 ID**（那是半敏感信息，
// 不该无谓地散落在日志文件里）。
func TestAlertButtonGenerationIsLogged(t *testing.T) {
	t.Parallel()

	code := stripGoComments(readSourceFile(t, "telegram_alert.go"))
	if !strings.Contains(code, "告警附带处置按钮") {
		t.Error("按钮生成没有记日志 —— 「按钮没出现」将无从排查")
	}
	// 日志里只能出现按钮文字与回调前缀，不能出现 ev.NodeID / ev.ServerID。
	for _, forbidden := range []string{`"node_id", ev.NodeID`, `"server_id", ev.ServerID`, `"alert_id", ev.AlertID`} {
		if strings.Contains(code, forbidden) {
			t.Errorf("按钮日志里出现了目标 ID（%s）—— "+
				"日志只需记录按钮文字与回调前缀", forbidden)
		}
	}
}

// TestAlertActionsPassTelegramRouteGate 断言按钮动作能通过
// telegramRun 的路由白名单校验（telegramAllowed）。
//
// 这是**运行时**校验，不是源码断言：telegramAllowed 会拿具体路径
// （带真实 UUID）去 mux 里反查模式，只有反查得到的模式**恰好等于**
// 注册时的 "METHOD /api/xxx/{id}" 字面量才放行。
//
// 如果 Go 的 ServeMux 反查出来的是别的形态（比如返回 "/api/nodes/{id}"
// 而注册时写的是 "DELETE /api/nodes/{id}"），按钮就会被静默拒绝 ——
// 点了没反应，而且日志里只有一句「该路径不是已登记的面板管理接口」。
//
// 这里用真实的 ServeMux 复刻 server.go 的注册方式，
// 把这条链路完整跑一遍。
func TestAlertActionsPassTelegramRouteGate(t *testing.T) {
	t.Parallel()

	// 复刻 server.go 的注册方式：方法与路径写在同一个 pattern 里。
	mux := http.NewServeMux()
	registered := []string{
		"POST /api/alerts/{id}/ack",
		"DELETE /api/nodes/{id}",
		"DELETE /api/servers/{id}",
	}
	for _, spec := range registered {
		mux.HandleFunc(spec, func(w http.ResponseWriter, r *http.Request) {})
	}

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	if len(rows) == 0 {
		t.Fatal("没生成按钮")
	}

	for _, nonce := range collectNonces(rows) {
		action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
		if !ok {
			t.Fatalf("nonce %s 查不到动作", nonce)
		}
		// telegramAllowed 的判定：反查模式 → 与注册字面量比对。
		r, err := http.NewRequest(action.Method, action.Path, nil)
		if err != nil {
			t.Fatalf("构造请求失败：%v", err)
		}
		_, pattern := mux.Handler(r)
		found := false
		for _, spec := range registered {
			if pattern == spec {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("动作 %s %s 反查到的模式是 %q，不在注册字面量里 —— "+
				"按钮会被 telegramAllowed 静默拒绝（点了没反应）",
				action.Method, action.Path, pattern)
		}
	}
}

// TestAlertRouteGateIsNotShadowedByOtherRoutes 断言反查不会被别的路由抢走。
//
// Go 的 ServeMux 在同一前缀下有多条模式时，反查结果取决于
// 「哪条最具体」。若 /api/alerts/{id}/ack 被 /api/alerts/{id}/events
// 或 /api/alerts/webhook 抢走（前者路径段数不同、后者字面量更具体），
// telegramAllowed 就会判成「不是已登记的接口」而拒绝 ——
// 按钮点了没反应，日志里只有一句「该路径不是已登记的面板管理接口」。
//
// ⚠️ 这里**必须从 server.go 真实解析注册表**，不能硬编码副本 ——
// 硬编码副本与生产实际注册的两份清单一旦分叉，
// 测试全绿而生产静默失效。所以本测试同时具备两个作用：
//   1. 用真实注册表跑一遍反查，验证按钮动作能过门；
//   2. 反查结果与注册字面量**逐一比对**，任何不一致都会报红。
func TestAlertRouteGateIsNotShadowedByOtherRoutes(t *testing.T) {
	t.Parallel()

	// 从 server.go 真实解析全部注册（形如 "POST /api/alerts/{id}/ack"）。
	specs := panelRoutePatterns(t)

	mux := http.NewServeMux()
	for spec := range specs {
		spec := spec
		mux.HandleFunc(spec, func(w http.ResponseWriter, r *http.Request) {})
	}

	s := &Server{}
	rows := s.alertActionRows(context.Background(), "chat-1", alertNotification{
		Event: "triggered", AlertID: "11111111-1111-1111-1111-111111111111",
		NodeID: "22222222-2222-2222-2222-222222222222",
	})
	if len(rows) == 0 {
		t.Fatal("没生成按钮")
	}

	for _, nonce := range collectNonces(rows) {
		action, ok := s.telegramControl.lookupAlertAction(nonce, "chat-1")
		if !ok {
			t.Fatalf("nonce %s 查不到动作", nonce)
		}
		r, err := http.NewRequest(action.Method, action.Path, nil)
		if err != nil {
			t.Fatalf("构造请求失败：%v", err)
		}
		_, pattern := mux.Handler(r)
		if !specs[pattern] {
			t.Errorf("动作 %s %s 反查到的模式是 %q，不在 server.go 的注册表里 —— "+
				"按钮会被 telegramAllowed 静默拒绝（点了没反应）",
				action.Method, action.Path, pattern)
		}
	}
}

// ---------------------------------------------------------------- 辅助

// collectNonces 从按钮行里取出全部 callback_data 的 nonce 部分。
func collectNonces(rows [][]map[string]string) []string {
	var out []string
	for _, row := range rows {
		for _, btn := range row {
			data := btn["callback_data"]
			if strings.HasPrefix(data, "tg:al:") {
				out = append(out, strings.TrimPrefix(data, "tg:al:"))
			}
		}
	}
	return out
}

// panelRoutePatterns 从 server.go 里抠出全部已注册的面板路由
// （形如 "DELETE /api/nodes/{id}"）。
//
// 解析方式：取 panelRoute( 之后的**第一个带引号的字符串**。
// 不能按右括号截断 —— 那会截到嵌套调用
// （s.authenticated(s.requireAdmin(s.deleteNode))）里去，拿到半截路径。
func panelRoutePatterns(t *testing.T) map[string]bool {
	t.Helper()
	code := stripGoComments(readSourceFile(t, "server.go"))
	out := map[string]bool{}
	for _, line := range strings.Split(code, "\n") {
		i := strings.Index(line, "panelRoute(")
		if i < 0 {
			continue
		}
		rest := line[i+len("panelRoute("):]
		q1 := strings.Index(rest, `"`)
		if q1 < 0 {
			continue
		}
		rest = rest[q1+1:]
		q2 := strings.Index(rest, `"`)
		if q2 < 0 {
			continue
		}
		spec := rest[:q2]
		if spec == "" {
			continue
		}
		out[spec] = true
	}
	if len(out) == 0 {
		t.Fatal("没解析出任何面板路由 —— 解析方式失效了")
	}
	return out
}

// 群组里的处置按钮回调只认管理员私聊，因此群组仅在配置了外部地址时返回「打开面板」按钮。
func TestAlertActionRowsGroupChatGetsOnlyPanelLink(t *testing.T) {
	t.Setenv("PANEL_PUBLIC_URL", "https://panel.example.com")
	s := &Server{}
	rows := s.alertActionRows(context.Background(), "-1001234", alertNotification{Event: "triggered", AlertID: "a", NodeID: "n"})
	if len(rows) != 1 || rows[0][0]["url"] != "https://panel.example.com" || rows[0][0]["callback_data"] != "" {
		t.Fatalf("群组聊天应仅返回面板链接按钮，实际得到 %v", rows)
	}
	t.Setenv("PANEL_PUBLIC_URL", "")
	t.Setenv("SUBSCRIPTION_PUBLIC_BASE_URL", "")
	if empty := s.alertActionRows(context.Background(), "-1001234", alertNotification{Event: "triggered", AlertID: "a", NodeID: "n"}); len(empty) != 0 {
		t.Fatalf("未配置面板地址时群组不应返回无效外链按钮，实际得到 %v", empty)
	}
}
