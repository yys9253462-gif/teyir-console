package httpapi

import "strings"

// subscription_rules.go — Clash 订阅的分组与分流规则（2026-10-17）。
//
// 原来只有「手动选择 + 自动测速」两个组和一条 MATCH,PROXY：国内网站也绕国外节点
// （慢，部分银行 / 视频站还会拒绝海外 IP），节点挂了也不会自动切走，
// AI 服务（对地区很挑）没法固定走美国节点。分组保持精简（节点不多时多分组只是负担）。
//
// 兼容性约束：只用所有 Clash 系客户端（Clash Premium / mihomo / Clash Verge /
// Stash / ClashX）都支持的规则类型 —— DOMAIN、DOMAIN-SUFFIX、IP-CIDR(6)、GEOIP、MATCH。
// 不用 GEOSITE（Stash 与老 Clash 不认，整份配置会被拒绝），也不用远程 RULE-SET
// （国内拉不到规则文件时，部分客户端会直接启动失败）。规则全部内置在订阅里。

const (
	groupSelect   = "🚀 节点选择"
	groupAuto     = "♻️ 自动选择"
	groupFallback = "🔯 故障转移"
	groupAI       = "🤖 AI 服务"
)

// clashProxyGroups 生成代理组（2026-10-17 按需精简为 4 个）：
// 节点选择 / 自动选择 / 故障转移，外加有美国节点时的 AI 服务（美国节点里自动测速）。
// names 是所有节点（含 Reality）的名字。
func clashProxyGroups(names []string) []map[string]any {
	// 组成员不能为空：Clash 遇到空组会拒绝整份配置（订阅暂时没有可用节点时就会这样）。
	members := names
	if len(members) == 0 {
		members = []string{"DIRECT"}
	}
	selectMembers := append([]string{groupAuto, groupFallback}, names...)
	selectMembers = append(selectMembers, "DIRECT")
	groups := []map[string]any{
		{"name": groupSelect, "type": "select", "proxies": selectMembers},
		{
			"name": groupAuto, "type": "url-test", "proxies": members,
			"url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50,
		},
		{
			// 按顺序用第一个可用的节点；当前节点不通时自动切到下一个。
			"name": groupFallback, "type": "fallback", "proxies": members,
			"url": "http://www.gstatic.com/generate_204", "interval": 300,
		},
	}
	if us := usNodes(names); len(us) > 0 {
		groups = append(groups, map[string]any{
			"name": groupAI, "type": "url-test", "proxies": us,
			"url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50,
		})
	}
	return groups
}

// usNodes 取名字以「美国-」开头的节点（命名规则：国家-城市）。
// AI 服务（ChatGPT / Claude / Gemini）对地区敏感，香港与部分亚洲 IP 会被拒。
func usNodes(names []string) []string {
	out := []string{}
	for _, n := range names {
		if strings.HasPrefix(n, "美国-") {
			out = append(out, n)
		}
	}
	return out
}

var aiDomainSuffixes = []string{
	"openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com", "sora.com",
	"anthropic.com", "claude.ai", "claude.com",
	"gemini.google.com", "aistudio.google.com", "generativelanguage.googleapis.com", "bard.google.com",
	"perplexity.ai", "x.ai", "grok.com", "poe.com", "copilot.microsoft.com", "mistral.ai", "cursor.sh", "cursor.com",
}

// 国内常用站点直连。GEOIP,CN 能兜住大部分国内 IP，这里先按域名命中，
// 避免「先走一次 DNS 解析再判断」，也覆盖使用海外 CDN 的国内站点。
var cnDomainSuffixes = []string{
	"cn", "baidu.com", "bdstatic.com", "bdimg.com", "qq.com", "gtimg.com", "qpic.cn", "wechat.com", "tencent.com",
	"taobao.com", "tmall.com", "alipay.com", "alicdn.com", "aliyun.com", "alibaba.com", "aliyuncs.com",
	"jd.com", "360buyimg.com", "bilibili.com", "hdslb.com", "biliapi.net", "bilivideo.com",
	"163.com", "126.net", "netease.com", "weibo.com", "sinaimg.cn", "sina.com", "zhihu.com", "zhimg.com",
	"douyin.com", "douyinpic.com", "douyinvod.com", "amemv.com", "bytedance.com", "pstatp.com", "toutiao.com",
	"xiaohongshu.com", "xhscdn.com", "meituan.com", "dianping.com", "pinduoduo.com", "yangkeduo.com",
	"ctrip.com", "12306.cn", "unionpay.com", "95516.com", "cmbchina.com", "icbc.com.cn",
	"iqiyi.com", "youku.com", "mgtv.com", "kuaishou.com", "xiaomi.com", "mi.com", "huawei.com", "hicloud.com",
	"sohu.com", "csdn.net", "cnblogs.com", "gitee.com", "deepseek.com", "kimi.com", "moonshot.cn",
}

// clashRules 生成分流规则：局域网直连 → AI 走 AI 组（有美国节点时）→ 国内直连 → 其余走节点选择。
func clashRules(withAI bool) []string {
	rules := []string{
		"DOMAIN-SUFFIX,local,DIRECT",
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
		"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,100.64.0.0/10,DIRECT,no-resolve",
		"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
		"IP-CIDR6,fc00::/7,DIRECT,no-resolve",
		"IP-CIDR6,fe80::/10,DIRECT,no-resolve",
	}
	if withAI {
		for _, d := range aiDomainSuffixes {
			rules = append(rules, "DOMAIN-SUFFIX,"+d+","+groupAI)
		}
	}
	for _, d := range cnDomainSuffixes {
		rules = append(rules, "DOMAIN-SUFFIX,"+d+",DIRECT")
	}
	return append(rules, "GEOIP,CN,DIRECT", "MATCH,"+groupSelect)
}
