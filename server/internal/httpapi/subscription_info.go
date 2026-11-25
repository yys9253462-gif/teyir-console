package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// subscription_info.go — 订阅里置顶的「套餐信息」条目（2026-10-17）。
//
// 买家导入订阅后，在节点列表最上面看到「📅 套餐到期：2026-11-05」「⏳ 剩余 28 天」。
// 这些是指向 127.0.0.1:1 的占位条目，名字就是信息本身；误点只会连不上，不影响任何东西。
// 它们只放进「节点选择」组（排在自动选择 / 故障转移之后，所以默认选中的仍是自动选择），
// 不进自动测速、故障转移与 AI 组，免得测速时被当成节点。
//
// 另外按通用约定返回 subscription-userinfo 头（Clash Verge / Shadowrocket / v2rayN 等
// 会在订阅卡片上直接显示到期时间）。

var displayZone = time.FixedZone("CST", 8*3600)

// updateHintLine 置顶的第一条：节点 IP / 端口换了以后，客户端只要更新订阅就能恢复，
// 多数买家不知道这一步，连不上就以为是服务挂了（2026-10-27）。
const updateHintLine = "🔄 连不上？先更新订阅"

// subscriptionInfoLines 生成置顶信息条目的名字；rec 为 nil（管理员预览）时不生成。
func subscriptionInfoLines(rec *subscriptionRecord, now time.Time) []string {
	if rec == nil {
		return nil
	}
	if rec.ExpiresAt == nil {
		return []string{updateHintLine, "📅 套餐：永久有效"}
	}
	lines := []string{updateHintLine, "📅 套餐到期：" + rec.ExpiresAt.In(displayZone).Format("2006-01-02")}
	if days := rec.daysLeft(now); days > 0 {
		lines = append(lines, fmt.Sprintf("⏳ 剩余 %d 天", days))
	} else {
		lines = append(lines, "⚠️ 已到期，请续费")
	}
	return lines
}

func infoClashProxy(name string) map[string]any {
	return map[string]any{
		"name": name, "type": "hysteria2", "server": "127.0.0.1", "port": 1,
		"password": "info", "skip-cert-verify": true,
	}
}

func infoURI(name string) string {
	return "hysteria2://info@127.0.0.1:1?insecure=1#" + url.PathEscape(name)
}

// setSubscriptionUserinfo 写 subscription-userinfo 头：只给到期时间（订阅没有单独的流量额度，
// 流量字段填 0 时部分客户端会显示成「已用 0 / 共 0」，所以这里干脆不写流量）。
func setSubscriptionUserinfo(w http.ResponseWriter, rec *subscriptionRecord) {
	if rec == nil || rec.ExpiresAt == nil {
		return
	}
	w.Header().Set("subscription-userinfo", fmt.Sprintf("expire=%d", rec.ExpiresAt.Unix()))
}

// renderSubscriptionWithInfo 与 renderSubscription 相同，额外把信息条目置顶。
// sing-box 不加：它的订阅是并入用户自己的配置，占位出站会混进用户的路由。
func renderSubscriptionWithInfo(nodes []subscriptionNode, format string, info []string) ([]byte, string, error) {
	switch format {
	case subscriptionFormatURI:
		return renderURIListWithInfo(nodes, info), "text/plain; charset=utf-8", nil
	case subscriptionFormatClash:
		body, err := renderClashWithInfo(nodes, info)
		return body, "text/yaml; charset=utf-8", err
	default:
		return renderSubscription(nodes, format)
	}
}

// withInfoInSelect 把信息条目插进「节点选择」组：放在自动选择 / 故障转移之后、节点之前，
// 这样列表里一眼能看到，而默认选中项（第一个）仍是自动选择。
func withInfoInSelect(groups []map[string]any, info []string) []map[string]any {
	if len(info) == 0 {
		return groups
	}
	for _, g := range groups {
		if g["name"] != groupSelect {
			continue
		}
		// ⚠️ 2026-10-30：断言失败必须**原样返回这一组**，不能拿 nil 继续。
		//
		// 原来用 `members, _ := ...` 吞掉 ok。一旦 proxies 不是 []string
		// （有人改了 clashProxyGroups 的构造、或中间做过 JSON 往返），
		// members 变成 nil ⇒ out 只剩 info 三条 ⇒ 这一组变成只有占位条目的
		// 组，而 Clash **遇到空/异常组会拒绝整份配置**（比"少一个组"严重得多：
		// 买家的整份订阅直接导入失败）。
		// 断言失败时保持该组不动，至少配置仍然可用。
		members, ok := g["proxies"].([]string)
		if !ok {
			continue
		}
		out := make([]string, 0, len(members)+len(info))
		inserted := false
		for _, m := range members {
			if !inserted && m != groupAuto && m != groupFallback {
				out = append(out, info...)
				inserted = true
			}
			out = append(out, m)
		}
		if !inserted {
			out = append(out, info...)
		}
		g["proxies"] = out
	}
	return groups
}
