package httpapi

// 告警外发（alertnotify.go）的单元测试。
//
// 这一组盯四件事：
//   1. 节流真的生效（否则采集器每 30 秒重算会把 IM 刷爆）
//   2. 恢复事件不被触发事件压掉（否则"好了"永远看不到）
//   3. URL 校验挡住协议与非回环（但**允许**内网 —— 自建通知在内网）
//   4. 脱敏回显不泄漏 token

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNotifierCooldownSuppressesRepeats(t *testing.T) {
	t.Parallel()
	n := newAlertNotifier()
	now := time.Now()

	if !n.allow("node_offline|jp2|x", now) {
		t.Fatal("第一次应当放行")
	}
	if n.allow("node_offline|jp2|x", now.Add(time.Minute)) {
		t.Error("冷却期内重复应被抑制 —— 采集器每 30 秒重算，" +
			"不节流会把 IM 刷爆（节点离线几小时 = 上千条）")
	}
	if !n.allow("node_offline|jp2|x", now.Add(alertNotifyCooldown+time.Second)) {
		t.Error("冷却期过后应重新放行")
	}
}

func TestNotifierKeysAreIndependent(t *testing.T) {
	t.Parallel()
	n := newAlertNotifier()
	now := time.Now()

	n.allow("node_offline|jp2|triggered", now)
	// 不同目标 / 不同类型 / 不同事件都应各自独立计数
	for _, key := range []string{
		"node_offline|us1|triggered",
		"server_warning|jp2|triggered",
		"node_offline|jp2|resolved",
	} {
		if !n.allow(key, now.Add(time.Second)) {
			t.Errorf("%q 不该被别的键的冷却波及", key)
		}
	}
}

func TestResolvedNotSuppressedByTriggered(t *testing.T) {
	t.Parallel()
	n := newAlertNotifier()
	now := time.Now()

	// 同类型同目标，先触发再恢复 —— 恢复必须能发出去。
	n.allow("node_offline|jp2|triggered", now)
	if !n.allow("node_offline|jp2|resolved", now.Add(time.Second)) {
		t.Error("恢复通知不该被触发通知的冷却压掉 —— " +
			"否则运维以为还没好（节流键必须含 event）")
	}
}

func TestBuildAlertNotificationWording(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)

	// 触发
	ev := buildAlertNotification("triggered", "node_offline", "warning",
		"东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617, "", now)
	if !strings.Contains(ev.Text, "告警") {
		t.Errorf("触发文案应含「告警」：%q", ev.Text)
	}
	if !strings.Contains(ev.Text, "东京-AWS-14617") {
		t.Errorf("节点类告警的 target 应是**节点名**（不是主机名）—— "+
			"同一主机可能挂多个节点，报主机名会让人不知道是哪个：%q", ev.Text)
	}
	if ev.OccurredAt == "" || !strings.HasSuffix(ev.OccurredAt, "Z") {
		t.Errorf("时间应为 RFC3339 UTC：%q", ev.OccurredAt)
	}

	// 恢复
	ev2 := buildAlertNotification("resolved", "node_offline", "warning",
		"东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617, "", now)
	if !strings.Contains(ev2.Text, "恢复") {
		t.Errorf("恢复文案应含「恢复」：%q", ev2.Text)
	}
	if strings.Contains(ev2.Text, "未监听") || !strings.Contains(ev2.Text, "已恢复监听") {
		t.Errorf("恢复文案不能沿用故障描述：%q", ev2.Text)
	}

	// 主机类告警用主机名
	ev3 := buildAlertNotification("triggered", "server_offline", "critical",
		"东京-AWS", "203.0.113.17", "", 0, "", now)
	if !strings.Contains(ev3.Text, "东京-AWS") {
		t.Errorf("主机类告警应含主机名：%q", ev3.Text)
	}

	// severity 缺省兜底
	ev4 := buildAlertNotification("triggered", "x", "", "T", "h", "", 0, "", now)
	if ev4.Severity != "warning" {
		t.Errorf("severity 为空时应兜底为 warning，实际 %q", ev4.Severity)
	}
}

func TestValidateWebhookURL(t *testing.T) {
	t.Parallel()

	// 允许：公网与**内网**都放行 —— 自建通知服务常在内网
	for _, ok := range []string{
		"https://oapi.dingtalk.com/robot/send?access_token=abc",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc",
		"https://api.telegram.org/bot123:ABC/sendMessage",
		"http://172.19.0.1:8085/notify",
		"http://192.168.1.10:9000/hook",
	} {
		if err := validateWebhookURL(ok); err != nil {
			t.Errorf("应放行 %s：%v", ok, err)
		}
	}

	// 拒绝
	for _, bad := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x",
		"http://127.0.0.1:8080/admin",
		"http://localhost/x",
		"http://0.0.0.0/x",
		"notaurl",
		"http://",
	} {
		if err := validateWebhookURL(bad); err == nil {
			t.Errorf("应拒绝 %s", bad)
		}
	}
}

func TestURLHostStripsCredentials(t *testing.T) {
	t.Parallel()
	// 脱敏只回显 host —— 路径里的 token 绝不能出现在设置页
	got := urlHost("https://oapi.dingtalk.com/robot/send?access_token=SECRET123")
	if got != "oapi.dingtalk.com" {
		t.Errorf("应只回 host，实际 %q", got)
	}
	if strings.Contains(got, "SECRET123") {
		t.Error("token 泄漏进了回显")
	}
	// 坏 URL 不该让回显路径报错
	if urlHost(":::bad::") != "" {
		t.Error("坏 URL 应返回空串而不是抛错")
	}
}

// 通知必须异步、且用 Background ctx。
//
// 这条是源码级断言：传入的 ctx 属于采集循环，循环一结束就被 cancel，
// 会让"告警刚发出就被掐断"—— 只在采集器偶发慢时暴露，极难排查。
func TestNotifyUsesBackgroundContext(t *testing.T) {
	t.Parallel()
	// 2026-11-03：webhook 投递拆进 deliverWebhook（deliverAlert 需要汇总两个渠道的结果）。
	body := funcBlock(t, "alertnotify.go", "func (s *Server) deliverWebhook(")
	if !strings.Contains(body, "context.Background()") {
		t.Error("notifyAlert 必须用 context.Background()，不能用调用方的 ctx —— " +
			"采集循环结束会把它 cancel 掉")
	}
	if strings.Contains(body, "NewRequestWithContext(ctx,") &&
		!strings.Contains(body, "context.WithTimeout(context.Background()") {
		t.Error("请求上下文必须源自 Background")
	}
}

func TestRecordAlertEventFiresNotification(t *testing.T) {
	t.Parallel()
	body := funcBlock(t, "manage.go", "func (s *Server) recordAlertEvent(")
	if !strings.Contains(body, "go s.notifyAlertFor(") {
		t.Error("recordAlertEvent 必须在独立 goroutine 里触发通知 —— " +
			"它是所有告警事件的唯一汇聚处，挂这里才全覆盖")
	}
}

func TestDescribeAlertCoversAllProducibleTypes(t *testing.T) {
	t.Parallel()
	// 每个**会真实产生**的告警类型都必须有专门文案。
	//
	// 这条是被真机验证逼出来的：端到端测试收到的载荷里
	//   "detail": "node_unknown · zzz-alert-selftest"
	// —— 落进了 default 的兜底拼接，是机器人话。
	// 而 node_unknown 恰恰是最常见的节点故障形态。
	//
	// 判定标准：文案里不能出现「类型名 · 名字」这种兜底形态，
	// 且必须包含可指导行动的信息。
	cases := []struct {
		kind      string
		severity  string
		mustHave  []string
		serverNam string
		host      string
		nodeName  string
		port      int
	}{
		{"server_offline", "critical", []string{"不可达"}, "东京-AWS", "203.0.113.17", "", 0},
		{"server_warning", "warning", []string{"阈值"}, "东京-AWS", "203.0.113.17", "", 0},
		{"server_unknown", "warning", []string{"凭据"}, "东京-AWS", "203.0.113.17", "", 0},
		{"server_stale", "critical", []string{"采集"}, "东京-AWS", "203.0.113.17", "", 0},
		{"node_offline", "critical", []string{"未监听", "14617"}, "东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617},
		{"node_unstable", "warning", []string{"不稳定"}, "东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617},
		{"node_unknown", "warning", []string{"未知"}, "东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617},
		{"node_subscription_lingering", "warning", []string{"滞留未注销"}, "东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617},
	}
	for _, c := range cases {
		got := describeAlert(c.kind, c.serverNam, c.host, c.nodeName, c.port)
		if strings.HasPrefix(got, c.kind+" · ") {
			t.Errorf("%s 落进了 default 兜底（文案是 %q）——"+
				"每个会真实产生的告警类型都要有可指导行动的说明", c.kind, got)
		}
		for _, m := range c.mustHave {
			if !strings.Contains(got, m) {
				t.Errorf("%s 的文案应含 %q，实际 %q", c.kind, m, got)
			}
		}
	}
}

// 告警投递不能用 portalClient。
func TestNotifyClientNotSharedWithPortal(t *testing.T) {
    t.Parallel()
    if alertNotifyClient == portalClient { t.Fatal("webhook and portal clients must be independent") }
    if alertNotifyClient.Transport == nil { t.Fatal("webhook requires a guarded transport") }
    if alertNotifyClient.CheckRedirect == nil { t.Fatal("webhook redirects must be rejected") }
}

func TestRecoveryNotificationsDescribeRecoveredState(t *testing.T) {
	t.Parallel()
	kinds := []string{"server_offline", "server_warning", "server_unknown", "server_stale", "server_ssh_broken",
		"server_expiring", "server_expired", "node_blocked", "node_offline", "node_unstable",
		"node_unknown", "node_traffic_exceeded", "node_subscription_lingering"}
	for _, kind := range kinds {
		triggered := buildAlertNotification("triggered", kind, "warning",
			"东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617, "", time.Unix(0, 0))
		resolved := buildAlertNotification("resolved", kind, "warning",
			"东京-AWS", "203.0.113.17", "东京-AWS-14617", 14617, "", time.Unix(0, 0))
		if resolved.Detail == triggered.Detail {
			t.Errorf("%s 恢复与告警使用了相同描述：%q", kind, resolved.Detail)
		}
		if !strings.HasPrefix(resolved.Text, "[恢复] ") {
			t.Errorf("%s 恢复文案缺少标签：%q", kind, resolved.Text)
		}
		for _, failure := range []string{"未成功", "不可达", "未监听", "疑似被墙", "可能仍可连入"} {
			if strings.Contains(resolved.Detail, failure) {
				t.Errorf("%s 恢复描述仍含故障词 %q：%q", kind, failure, resolved.Detail)
			}
		}
	}
	got := buildAlertNotification("resolved", "node_subscription_lingering", "warning",
		"美国-阿什本", "198.51.100.52", "美国-阿什本", 443, "", time.Unix(0, 0)).Text
	want := "[恢复] 美国-阿什本 —— 节点 美国-阿什本 上已到期订阅账号已注销，不再有滞留账号"
	if got != want {
		t.Errorf("滞留账号恢复文案 = %q，期望 %q", got, want)
	}
}

// 2026-11-04：投递耗尽了调用方的 ctx 之后，notified_at 仍要写进去（否则 15 分钟后被当成未送达重推）。
func TestMarkDeliveredSurvivesSpentContext(t *testing.T) {
	s := monitoringServer()
	var got error
	s.alertNotifier.persist = func(ctx context.Context, id string) error { got = ctx.Err(); return nil }
	spent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	s.markDelivered(spent, "a1")
	if got != nil {
		t.Fatalf("persisting notified_at must not reuse an expired context, got %v", got)
	}
}
