package httpapi

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// truncateRunes
// ---------------------------------------------------------------------------

// TestTruncateRunesNeverSplitsRune 盯的是一个真实事故：storeCreateOrder 原来
// 用 `if len(label) > 64 { label = label[:64] }` 截断。len 和切片都是**字节**操作，
// 中文标签被切成半个字就成了非法 UTF-8，Postgres 直接拒收 → 商城收到 500，
// 订单开不出来。ASCII 标签长度相同却没事，所以这个 bug 只在中文下现身。
func TestTruncateRunesNeverSplitsRune(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"ASCII 短于上限", "abcd", 64, "abcd"},
		{"ASCII 正好等于上限", "abcde", 5, "abcde"},
		{"ASCII 超限", strings.Repeat("A", 100), 64, strings.Repeat("A", 64)},
		{"中文超限：按字符而不是字节", strings.Repeat("长", 40), 64, strings.Repeat("长", 40)},
		{"中文正好卡在上限", strings.Repeat("长", 64), 64, strings.Repeat("长", 64)},
		{"中文超上限按字符截", strings.Repeat("长", 100), 64, strings.Repeat("长", 64)},
		{"混合内容", "商城订阅 · " + strings.Repeat("a", 200), 10, "商城订阅 · aaa"},
		{"emoji 是单字符但 4 字节", strings.Repeat("🙂", 10), 3, "🙂🙂🙂"},
		{"空串", "", 64, ""},
		{"零上限", "abc", 0, ""},
		{"负上限", "abc", -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateRunes(c.in, c.limit)
			if got != c.want {
				t.Fatalf("truncateRunes(%q, %d) = %q，期望 %q", c.in, c.limit, got, c.want)
			}
			// 最要紧的性质：输出永远是合法 UTF-8。上面那个 500 就是这条不成立。
			if !utf8.ValidString(got) {
				t.Fatalf("输出不是合法 UTF-8：%q", got)
			}
			if n := utf8.RuneCountInString(got); c.limit > 0 && n > c.limit {
				t.Fatalf("输出 %d 个字符，超过上限 %d", n, c.limit)
			}
		})
	}
}

// TestTruncateRunesCountsWhatAHumanCounts 把「字符数」这一条单独钉住：
// 40 个汉字是 40 个字符 / 120 字节。老实现按字节卡 64，会把它切成 21 个半字。
func TestTruncateRunesCountsWhatAHumanCounts(t *testing.T) {
	label := strings.Repeat("长", 40)
	if len(label) != 120 {
		t.Fatalf("前置假设不成立：40 个汉字应为 120 字节，实际 %d", len(label))
	}
	if got := truncateRunes(label, 64); got != label {
		t.Fatalf("40 个字符没有超过 64 的上限，不该被截断，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// nodeRenewDeltaDays
// ---------------------------------------------------------------------------

// TestNodeRenewDeltaDaysIsADeltaNotATotal 复现生产事故：
//
// 订阅先卖 30 天、到期日 T+30d，节点账号也开在 T+30d（两边对齐，没问题）。
// 买家续费 30 天 → 订阅到期日变成 T+60d。
//
//	老实现：extend_days = 「订阅剩余 = 60 天」→ 节点账号变成 T+90d，比订阅多活 30 天。
//	新实现：extend_days = 「还差 30 天」    → 节点账号变成 T+60d，与订阅对齐。
//
// 多活的那 30 天不是账面数字：买家已经下载了配置，订阅到期后 /sub 不再下发，
// 但那个账号在节点上仍然可用 —— 停了付费的人还能继续连。
func TestNodeRenewDeltaDaysIsADeltaNotATotal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	day := 24 * time.Hour
	at := func(d int) *time.Time { v := now.Add(time.Duration(d) * day); return &v }

	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(60)}, now.Add(30*day).Unix(), now); got != 30 {
		t.Fatalf("订阅 T+60d / 账号 T+30d：期望补 30 天，得到 %d 天（60 就是老 bug）", got)
	}
	// 一次续费之后紧接着再跑一遍（重放/重试），不能再加。
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(60)}, now.Add(60*day).Unix(), now); got != 0 {
		t.Fatalf("账号已到订阅到期日：期望 0（幂等），得到 %d", got)
	}
	// 账号被别的路径推过头了，不能往回缩，也不该再加。
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(60)}, now.Add(90*day).Unix(), now); got != 0 {
		t.Fatalf("账号已超过订阅到期：期望 0，得到 %d", got)
	}
	// 全新开通：账号还没建（expiry 0），应从 now 算起。
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(30)}, 0, now); got != 30 {
		t.Fatalf("账号不存在：期望 30，得到 %d", got)
	}
	// 订阅已经过期：不能返回负数。
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(-5)}, now.Add(-10*day).Unix(), now); got != 0 {
		t.Fatalf("订阅已过期：期望 0，得到 %d", got)
	}
	// 账号已过期但订阅还没到期：从 now 起算，不是从账号的旧到期日。
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: at(10)}, now.Add(-100*day).Unix(), now); got != 10 {
		t.Fatalf("账号过期、订阅还剩 10 天：期望 10，得到 %d", got)
	}
	// 不足一天按一天给：宁可多给几小时，也不能让付了钱的人提前断线。
	// 账号停在 now，订阅 1 小时后到期 → 至少要给 1 天。
	oneHour := now.Add(time.Hour)
	if got := nodeRenewDeltaDays(&subscriptionRecord{ExpiresAt: &oneHour}, now.Unix(), now); got != 1 {
		t.Fatalf("还差 1 小时：期望向上取整为 1 天，得到 %d", got)
	}
}

func TestNodeRenewDeltaDaysPerpetual(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// 永久订阅（无到期日）在开通时就按面板上限 3650 天开，重放不应再叠。
	if got := nodeRenewDeltaDays(&subscriptionRecord{}, now.AddDate(0, 0, 3650).Unix(), now); got != 0 {
		t.Fatalf("永久订阅且账号已达上限：期望 0，得到 %d", got)
	}
	if got := nodeRenewDeltaDays(&subscriptionRecord{}, 0, now); got != 3650 {
		t.Fatalf("永久订阅、账号缺失：期望 3650，得到 %d", got)
	}
}

// ---------------------------------------------------------------------------
// nodeAccountExpiry
// ---------------------------------------------------------------------------

// TestParseAccountExpiry 钉住「从节点读出账号当前到期日」这件事 —— 没有它就算不出
// 增量，只能退回按「剩余全额」瞎给（那就是多活 30 天的来源）。
//
// 这里只测解析、不测网络：portalCall 走的是带 SSRF 防护的客户端，它会拒绝一切
// 非公网地址，所以 httptest 的 127.0.0.1 服务根本握不上手。样本直接取自
// portal.py 的真实响应。
func TestParseAccountExpiry(t *testing.T) {
	raw := []byte(`{
      "ok": true,
      "count": 2,
      "users": [
        {"user_id":"admin_master","expires_at":2085974400,"active":true,"traffic_limit_bytes":0,"traffic_used_bytes":0,"ip_limit":0,"created_at":1700000000,"status":"active"},
        {"user_id":"sub_abc","expires_at":1800000000,"active":true,"traffic_limit_bytes":0,"traffic_used_bytes":0,"ip_limit":5,"created_at":1700000100,"status":"active"}
      ]
    }`)

	expiry, err := parseAccountExpiry(raw, "sub_abc")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if expiry != 1800000000 {
		t.Fatalf("期望取到 sub_abc 的 1800000000，得到 %d", expiry)
	}

	// 账号不在列表里必须报错，而不是静默当成 0 —— 那会让上面算出 3650 天。
	if _, err := parseAccountExpiry(raw, "sub_missing"); err == nil {
		t.Fatal("账号不存在时应报错")
	}
	if _, err := parseAccountExpiry([]byte("not json"), "sub_abc"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}
