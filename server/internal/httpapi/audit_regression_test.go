package httpapi

import (
	"net"
	"strings"
	"testing"
)

// TestRedactRequestPathCoversVariants 钉住「路径脱敏必须覆盖畸形变体」。
//
// 背景（2026-10-04 实测）：原实现按**精确段数**匹配，9 条用例漏 5 条
// （尾斜杠 / 多余段 / 路径穿越 / 大写 / 双斜杠），token 原样进日志。
// token 即凭据，且 requestLog 在 mux 外层，畸形路径也会被记录。
func TestRedactRequestPathCoversVariants(t *testing.T) {
	const tok = "SECRETTOKEN123"
	cases := []string{
		// 原有三条（必须继续通过）
		"/sub/" + tok,
		"/api/sub/" + tok,
		"/api/sub/view/" + tok,
		// 曾经泄漏的变体
		"/sub/" + tok + "/",
		"/api/sub/view/" + tok + "/extra",
		"/sub/" + tok + "/../x",
		"/SUB/" + tok,
		"/sub//" + tok,
		"/api/SUB/" + tok,
		"/sub/" + tok + "?x=1",
	}
	for _, c := range cases {
		got := redactRequestPath(c)
		if strings.Contains(got, tok) {
			t.Errorf("路径 %q 脱敏后仍含 token: %q\n"+
				"token 进日志等于凭据泄漏（subscription.go: token in the path IS the credential）。"+
				"脱敏逻辑必须与路径段数无关。", c, got)
		}
	}
}

// TestRedactRequestPathKeepsOrdinaryPaths 确认脱敏没把正常路径改坏。
//
// 只脱敏 sub 后面的那一段，其他路径必须原样通过 ——
// 否则日志会变得没用于排查。
func TestRedactRequestPathKeepsOrdinaryPaths(t *testing.T) {
	cases := map[string]string{
		"/api/nodes":              "/api/nodes",
		"/api/nodes/abc/overview": "/api/nodes/abc/overview",
		"/api/servers":            "/api/servers",
		"/":                       "/",
		"/api/subscriptions":      "/api/subscriptions", // 注意：不是 sub 段
	}
	for in, want := range cases {
		if got := redactRequestPath(in); got != want {
			t.Errorf("路径 %q 被改动了: 得到 %q, 期望 %q —— 脱敏不应影响普通路径", in, got, want)
		}
	}
}

// TestWebhookRejectsMetadataAndLoopbackVariants 钉住 webhook SSRF 校验。
//
// 背景（2026-10-04 复刻实现实测）：原来只比 4 个字面量，
// 8 种变体全部放行，其中 http://169.254.169.254/ 是云元数据端点，
// 实测从 api 容器网段**真的可达**（取回了 ami-id）。
func TestWebhookRejectsMetadataAndLoopbackVariants(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://[::1]/x",
		"http://0.0.0.0/x",
		// 曾经放行的变体
		"http://169.254.169.254/latest/meta-data/", // 云元数据（链路本地）
		"http://127.1/x",
		"http://127.0.0.2/x",
		"http://2130706433/x",         // 十进制回环
		"http://0x7f000001/x",         // 十六进制回环
		"http://0177.0.0.1/x",         // 八进制回环
		"http://[::ffff:127.0.0.1]/x", // IPv4-mapped IPv6
	}
	for _, u := range blocked {
		if err := validateWebhookURL(u); err == nil {
			t.Errorf("%s 必须被拒绝 —— 它是回环/链路本地地址。"+
				"169.254.169.254 是云元数据端点，实测可达，属盲 SSRF 面。", u)
		}
	}
}

// TestWebhookStillAllowsIntranetAndPublic 确认修复没有误伤。
//
// 作者**有意**允许内网 webhook（自建通知服务、局域网机器人网关），
// 这个取舍必须保留 —— 修 SSRF 不等于把内网一刀切掉。
func TestWebhookStillAllowsIntranetAndPublic(t *testing.T) {
	allowed := []string{
		"http://172.19.0.1:8085/x", // docker 网桥（内网）
		"http://192.168.1.10/x",    // 局域网
		"http://10.0.0.5:9000/x",   // 内网
		"https://oapi.dingtalk.com/robot/send?access_token=x",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=x",
	}
	for _, u := range allowed {
		if err := validateWebhookURL(u); err != nil {
			t.Errorf("%s 不该被拒绝（%v）—— 允许内网是刻意的取舍，"+
				"修复只应挡回环/链路本地/组播，不能把私有网段也切掉", u, err)
		}
	}
}

// TestWebhookAddrAllowedMatchesURLValidation 确认两条路径用同一策略。
//
// validateWebhookURL 挡字面量，webhookAddrAllowed 挡解析后的 IP
// （防 nip.io 这类「域名正常、解析到回环」的绕过）。
// 两者判断标准必须一致，否则会出现"字面量过了、解析挂了"的怪异不一致。
func TestWebhookAddrAllowedMatchesURLValidation(t *testing.T) {
	type tc struct {
		url   string
		ipStr string
	}
	cases := []tc{
		{"http://127.0.0.1/x", "127.0.0.1"},
		{"http://169.254.169.254/x", "169.254.169.254"},
		{"http://192.168.1.1/x", "192.168.1.1"},
		{"http://8.8.8.8/x", "8.8.8.8"},
	}
	for _, c := range cases {
		urlErr := validateWebhookURL(c.url)
		addrOK := webhookAddrAllowed(net.ParseIP(c.ipStr))
		// 两边应当一致：URL 被拒 <=> 地址不允许
		if (urlErr != nil) == addrOK {
			t.Errorf("%s / %s 两条校验路径结论不一致（urlErr=%v addrAllowed=%v）—— "+
				"它们必须用同一套策略", c.url, c.ipStr, urlErr, addrOK)
		}
	}
}
