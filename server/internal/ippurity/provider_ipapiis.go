package ippurity

// provider_ipapiis.go — ipapi.is 适配器（公网兜底提供方之一）。
//
// 为什么默认选 ipapi.is
// ---------------------
// 它一次请求同时给出连接类型判定（is_datacenter / is_isp / is_abuser）、
// ASN 类型、以及一个综合风险判断，字段最全，正好对上本功能要的三个维度：
//   - 是否机房 vs 家宽 → type / is_datacenter
//   - 是否双 ISP       → asn.type == "isp" 且公司类型暗示多家共用
//   - 是否纯净         → is_abuser / is_proxy / is_vpn + 复合评分
//
// 免费额度 1000 次/天，够用（节点数有上限，且结果落库缓存）。
//
// 密钥从环境变量 IP_PURITY_API_KEY 读。**没配密钥时本适配器不启用**
// （由 main.go 决定是否装配），所以这里不处理"无密钥"状态 —— 那只会在
// 装配层出现。这样 Lookup 里的每个分支都是"有密钥下真的可能发生的事"。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultIPAPIIsBaseURL 是 ipapi.is 的 JSON 接口基址，可用环境变量覆盖
// （自建服务/换供应商时不用改代码，与 geo 的 GeoAPIBaseURL 同思路）。
const DefaultIPAPIIsBaseURL = "https://api.ipapi.is"

// IPAPIIs 走 ipapi.is 的 JSON 接口。
type IPAPIIs struct {
	// APIKey 是访问密钥。空串表示调用方装配错误 —— 装配层不应在无密钥时
	// 构造本类型，但这里仍做保护性判断，返回明确错误而非发一个必然 401 的请求。
	APIKey string
	// Keys 非空时优先从它取当前密钥（面板里设置的值可热更新），取不到才用 APIKey。
	Keys *KeyStore
	// BaseURL 覆盖接口基址，留空用 DefaultIPAPIIsBaseURL。
	BaseURL string
}

func (p IPAPIIs) Name() string { return "ipapi.is" }

// ipapiIsResponse 是 ipapi.is 的返回结构（只取用得到的字段）。
//
// 字段名以 ipapi.is 官方文档为准。未知字段不解析 —— 多解析只会让
// "上游改字段"变成"我们静默拿到错值"，不如少解析、缺了就标 nil。
type ipapiIsResponse struct {
	IP           string `json:"ip"`
	IsDatacenter bool   `json:"is_datacenter"`
	IsMobile     bool   `json:"is_mobile"`
	IsProxy      bool   `json:"is_proxy"`
	IsVPN        bool   `json:"is_vpn"`
	IsTor        bool   `json:"is_tor"`
	IsAbuser     bool   `json:"is_abuser"`
	IsCrawler    bool   `json:"is_crawler"`
	// asn 子对象。type 是 ipapi.is 对 ASN 的分类（isp / hosting / business / ...）。
	ASN *struct {
		ASN     int    `json:"asn"`
		Org     string `json:"org"`
		Type    string `json:"type"`
		Country string `json:"country"`
		// AbuserScore 是 ipapi.is 的滥用风险。⚠️ 实测上游返回的是带单位的
		// 字符串（如 "0.0023 (Low)"），不是纯数字，因此这里用 json.Number
		// 承载并在 parseAbuserScore 里解析；直接声明成 float64 会在真实响应上
		// 报 "cannot unmarshal string into Go value of type float64"。
		AbuserScore json.RawMessage `json:"abuser_score"`
	} `json:"asn"`
	// company 与 datacenter 子对象用于辅助判定（有的机房 IP 不打 is_datacenter，
	// 但 company.type 是 hosting）。
	Company *struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"company"`
	Datacenter *struct {
		Datacenter string `json:"datacenter"`
	} `json:"datacenter"`
}

// parseAbuserScore 把 ipapi.is 的 abuser_score 折成 0.0-1.0 的浮点。
// 上游两种形态都出现过：
//   - 纯数字：   0.0023
//   - 带单位串： "0.0023 (Low)" / "12 (High)" / "0.5"
//
// 取串首的数字部分解析；解析不出返回 (0,false)，调用方据此保留 nil 不硬猜。
func parseAbuserScore(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	// 去掉 JSON 字符串引号
	s = strings.Trim(s, `"`)
	// 取开头连续的数字/小数点/负号
	end := 0
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// Lookup 查询一个 IP。返回 nil（不是错误）表示上游没有该 IP 的数据。
func (p IPAPIIs) Lookup(ctx context.Context, client *http.Client, ip string) (*Verdict, error) {
	key := strings.TrimSpace(p.Keys.Get(KeyIPAPIIs))
	if key == "" {
		key = strings.TrimSpace(p.APIKey)
	}
	if key == "" {
		return nil, ErrNoKey
	}
	base := strings.TrimSpace(p.BaseURL)
	if base == "" {
		base = DefaultIPAPIIsBaseURL
	}
	// ipapi.is 的查询方式：GET {base}/?q=<ip>&key=<key>。
	endpoint := strings.TrimRight(base, "/") + "/?q=" + url.QueryEscape(ip) + "&key=" + url.QueryEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		// 严禁直接返回 *url.Error 原文，避免把查询串中的密钥写入数据库并暴露给前端。
		return nil, errors.New("构造 ipapi.is 请求失败")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// 2026-11-03：不能带 err 原文。密钥在 query 里，*url.Error 会把完整 URL
		// （含 &key=...）打进错误；这条错误随后写进 node_ip_purity.error 并经
		// GET /api/ip-purity 返回给任何已登录用户。与 IPQS 同样处理。
		return nil, errors.New("ipapi.is 请求失败（网络错误或超时）")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("ipapi.is 限速（429），请稍后再试或检查配额")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("ipapi.is 鉴权失败（%d），请检查 IP_PURITY_API_KEY", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		// 读一点 body 便于排障，但别把整个响应灌进错误信息。
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("ipapi.is 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var parsed ipapiIsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("解析 ipapi.is 响应失败: %w", err)
	}

	return normalizeIPAPIIs(ip, parsed), nil
}

// normalizeIPAPIIs 把 ipapi.is 原始字段折成 Verdict。
//
// 判定优先级（每条都写清理由，因为这里的取舍直接决定用户看到什么）：
//
//   - 连接类型：datacenter 标记 > ASN/company 类型 > mobile > 兜底 unknown。
//     机房标记最直接；没有标记时看 ASN 类型（hosting=机房、isp=家宽/运营商、
//     business=专线）。mobile 单独成一类，因为它既非机房也非固网家宽。
//   - 双 ISP：业内「双 ISP」指这条 IP 在**两个独立分类口径**里都被标成
//     ISP（运营商/宽带）而不是 hosting —— 看起来像家宽，风控更宽松。
//     ipapi.is 给了两个独立口径：asn.type（ASN 层）与 company.type（该 IP
//     所属网段的实际使用者）。两者都是 isp → 是；两者都给了类型但不全是
//     isp → 否；任一缺失 → nil（未判定）。
//     ⚠️ 2026-10-15 修正：原实现把它理解成「同一段被两家 ISP 共用」，
//     要求两边都是 isp 且**组织名不同**才算是，并且 hosting 一律留 nil ——
//     结果机房 IP 全显示「未判定」，真正的双 ISP（两边都是 isp、同名）反被判「否」。
//   - 评分：ipapi.is 的 abuser_score 是 0.0-1.0 的小数，换算成 0-100 整数。
//     is_abuser 为真时至少给 70，避免"被标滥用但分数显示 0"的自相矛盾。
func normalizeIPAPIIs(ip string, r ipapiIsResponse) *Verdict {
	v := &Verdict{IP: ip}

	// --- 连接类型 ---
	switch {
	case r.IsDatacenter || (r.Datacenter != nil && r.Datacenter.Datacenter != ""):
		v.ConnectionType = "datacenter"
	case r.IsMobile:
		v.ConnectionType = "mobile"
	case r.ASN != nil && strings.EqualFold(r.ASN.Type, "hosting"):
		v.ConnectionType = "datacenter"
	case r.Company != nil && strings.EqualFold(r.Company.Type, "hosting"):
		v.ConnectionType = "datacenter"
	case r.ASN != nil && strings.EqualFold(r.ASN.Type, "business"):
		v.ConnectionType = "business"
	case r.ASN != nil && strings.EqualFold(r.ASN.Type, "isp"):
		v.ConnectionType = "residential"
	default:
		v.ConnectionType = "unknown"
	}

	// --- ASN ---
	if r.ASN != nil {
		if r.ASN.ASN != 0 {
			v.ASN = fmt.Sprintf("AS%d", r.ASN.ASN)
		}
		v.ASNOrg = strings.TrimSpace(r.ASN.Org)
	}
	if v.ASNOrg == "" && r.Company != nil {
		v.ASNOrg = strings.TrimSpace(r.Company.Name)
	}

	// --- 风险标记（仅在有明确布尔时才写，否则留 nil）---
	proxy := r.IsProxy || r.IsTor
	v.Proxy = BoolPtr(proxy)
	v.VPN = BoolPtr(r.IsVPN)
	v.Abuser = BoolPtr(r.IsAbuser)

	// --- 评分 ---
	// ⚠️ 必须先判 r.ASN != nil：ipapi.is 对部分 IP 不返回 asn 子对象，
	// 直接取 r.ASN.AbuserScore 会空指针崩溃。
	if r.ASN != nil {
		if f, ok := parseAbuserScore(r.ASN.AbuserScore); ok {
			score := int(f*100 + 0.5)
			if score < 0 {
				score = 0
			}
			if score > 100 {
				score = 100
			}
			if r.IsAbuser && score < 70 {
				// 被标滥用但换算分偏低时抬到 70，避免自相矛盾。
				score = 70
			}
			v.FraudScore = IntPtr(score)
		}
		// 上游给了 abuser_score 但解析不出数字 → 留 nil（未判定），不硬猜 0。
	}
	if r.IsAbuser {
		if v.FraudScore == nil || *v.FraudScore < 70 {
			// 被明确标滥用时至少给 70，即使没有分数。
			v.FraudScore = IntPtr(70)
		}
	}

	// --- 双 ISP：仅在能明确判定时给值，否则留 nil ---
	if r.ASN != nil && r.Company != nil {
		asnIsISP := strings.EqualFold(r.ASN.Type, "isp")
		companyIsISP := strings.EqualFold(r.Company.Type, "isp")
		bothTyped := strings.TrimSpace(r.ASN.Type) != "" && strings.TrimSpace(r.Company.Type) != ""
		if bothTyped {
			v.DualISP = BoolPtr(asnIsISP && companyIsISP)
		}
		// 缺任一口径的类型 → 保持 nil = 未判定。
	}

	// --- 原始结构（排障用）---
	v.Raw = map[string]any{
		"is_datacenter": r.IsDatacenter,
		"is_mobile":     r.IsMobile,
		"is_proxy":      r.IsProxy,
		"is_vpn":        r.IsVPN,
		"is_tor":        r.IsTor,
		"is_abuser":     r.IsAbuser,
		"asn":           r.ASN,
		"company":       r.Company,
	}
	if r.IP != "" {
		v.Raw["reported_ip"] = r.IP
	}
	return v
}

// 编译期断言：IPAPIIs 满足 APIProvider。
var _ APIProvider = IPAPIIs{}

// 让 time 不被误删（Lookup 的 ctx 超时由 client 控制，此处保留导入以防
// 将来加 per-request deadline）。
var _ = time.Second
