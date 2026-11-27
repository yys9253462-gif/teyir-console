// Package ippurity — 把「一个 IP 干不干净」变成一件可自动回答的事。
//
// 为什么需要它
// ------------
// 这个控制台卖的节点，买家最在意的不是带宽而是**这条 IP 有没有前科**：
// 一查就是机房托管、被 Netflix/OpenAI/银行风控标记为代理、或是被滥用过的
// 高风险段，那这个节点对目标用途就是废的。此前只能人工拿 IP 去第三方网站
// 逐个查，查完还不记得 —— 于是「这台节点的 IP 属于什么性质」从来不是
// 控制台里一个可查的属性。
//
// 本包把这件事做成可自动完成的：给一个公网 IP，返回一份 Verdict：
//   - ConnectionType：家宽 / 机房 / 移动 / 专线
//   - DualISP：是否双 ISP（同一段被两家 ISP 共用）
//   - FraudScore + Proxy/VPN/Abuser 标记：脏不脏、脏在哪
//
// 设计取舍：离线判定优先，公网 API 兜底
// ---------------------------------------
// 与 geo 包同一套取舍，理由也相同（详见那一包的论证）：
//
//  1. 离线判定（ASN 类型 / 已知机房网段）。优点：不联网、不泄漏节点 IP 清单、
//     无限速、稳定。缺点：判定精度有限，"双 ISP"这种需要厂商数据的能力
//     基本给不出。
//  2. 公网 API（ipapi.is 之类）。优点：字段全、给评分。缺点：要联网、有限速、
//     会把「我有哪几台机器」的 IP 清单持续告诉第三方。
//
// 所以：**先离线判定，离线给不出关键结论（评分/双ISP）时才走 API**，
// 且 API 结果由调用方缓存进库（node_ip_purity 表），第二次不再出网。
//
// 安全姿态与项目一致
// ------------------
// 本包所有出网请求都走 safeClient：它复用 geo 包同一套拨号语义 ——
// 解析目标 IP 后拒绝非公网地址、拒绝重定向。且本包只接受公网 IP 作为
// 查询输入（IsPublicIP），因为输入来自节点自报或用户手填，都属于外部输入。
package ippurity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"nodeops/server/internal/portaltls"
	"strings"
	"sync"
	"time"
)

// Verdict 是一次 IP 纯净度检测的结果。
//
// 指针字段的用意：**"未判定"与"判定为否"必须区分开**。
// 第三方 API 有一半不给 is_dual_isp，把"没数据"当"不是双 ISP"是在骗用户 ——
// 所以无法判定的布尔一律留 nil，界面上显示"未判定"而不是红色的"否"。
type Verdict struct {
	// IP 是本次检测的对象（公网 IP 字面量）。
	IP string `json:"ip"`
	// ConnectionType 是连接类型：residential / datacenter / mobile / business / unknown。
	ConnectionType string `json:"connection_type"`
	// DualISP 是否双 ISP。nil=未判定。
	DualISP *bool `json:"is_dual_isp"`
	// FraudScore 纯净度风险分 0-100（越大越脏）。nil=未判定。
	FraudScore *int `json:"fraud_score"`
	// Proxy / VPN / Abuser 三个风险标记。nil=未判定。
	Proxy  *bool `json:"is_proxy"`
	VPN    *bool `json:"is_vpn"`
	Abuser *bool `json:"is_abuser"`
	// ASN / ASNOrg 判定连接类型的主要依据，也方便运维一眼看出挂在谁家。
	ASN    string `json:"asn,omitempty"`
	ASNOrg string `json:"asn_org,omitempty"`
	// Source 记录结论从哪来：offline / api。出问题时第一个要看的就是它。
	Source string `json:"source,omitempty"`
	// ScoreSource 记录评分与风险标记来自哪里："ipqs"（单 IP 欺诈分）或
	// "ipapi"（ASN 滥用比例）。两者含义不同，界面据此换列名。
	ScoreSource string `json:"score_source,omitempty"`
	// Raw 是第三方原始结构或离线判定明细，排障用。可能是 nil。
	Raw map[string]any `json:"raw,omitempty"`
	// Partial 非空表示这次只拿到了一部分结论（公网 API 或单 IP 评分失败，用的是降级结果），内容是原因。
	// 2026-11-06：原来这种情况与完整成功无法区分，落库时评分被写成 NULL 或被 ASN 级分数覆盖、error 被清空。
	Partial string `json:"-"`
}

// Empty 表示什么都没判定出来。调用方据此决定不改库。
func (v Verdict) Empty() bool {
	return v.ConnectionType == "" || v.ConnectionType == "unknown"
}

// ConnectionTypeLabel 返回中文标签，供界面直接展示。
func ConnectionTypeLabel(t string) string {
	switch t {
	case "residential":
		return "家庭宽带"
	case "datacenter":
		return "机房托管"
	case "mobile":
		return "移动网络"
	case "business":
		return "企业专线"
	default:
		return "未知"
	}
}

// ---------------------------------------------------------------------------
// 出网客户端（与 geo 包同一套语义，独立实现以免包间耦合）
// ---------------------------------------------------------------------------

func newSafeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("IP 纯净度查询不接受重定向")
		},
		Transport: &http.Transport{
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("解析 %s 失败: %w", host, err)
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("%s 没有解析到任何地址", host)
				}
				for _, ip := range ips {
					// 统一经由 portaltls.PublicIP 复核，封堵运营商级 NAT、NAT64 及保留段旁路。
					if !portaltls.PublicIP(ip.IP) {
						return nil, fmt.Errorf("%s 解析到非公网地址，已拒绝访问", host)
					}
				}
				return portaltls.DialCheckedIPs(ctx, dialer, network, port, ips)
			},
		},
	}
}

var safeClient = newSafeClient()

// IsPublicIP 判断输入是否是可查询的公网 IP 字面量。
//
// 拒绝的：私有网段、环回、链路本地（含 169.254.169.254 云元数据端点）、
// 组播、未指定地址、带端口/方括号/CIDR 的输入、以及文档保留段。
// 与 geo.IsPublicIP 行为一致 —— 两处对"公网 IP"的定义必须相同，
// 否则同一台机器在 geo 那边能识别、在这里却被拒，会让运维困惑。
func IsPublicIP(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" {
		return false
	}
	// 方括号与斜杠一律不认：那是"带端口的 IPv6"或 CIDR，不是裸 IP。
	// 不能用 strings.Count(value, ":") > 1 挡带端口的 IPv4 —— 合法 IPv6
	// 天生多个冒号，那样写会误杀每一条 IPv6。
	if strings.ContainsAny(value, "[]/") {
		return false
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return false
	}
	if !portaltls.PublicIP(ip) || ip.IsMulticast() {
		return false
	}
	if reservedForDocumentation(ip) {
		return false
	}
	return true
}

func reservedForDocumentation(ip net.IP) bool {
	for _, cidr := range documentationNets {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var documentationNets = func() []*net.IPNet {
	raw := []string{
		"192.0.2.0/24",    // RFC 5737 TEST-NET-1
		"198.51.100.0/24", // RFC 5737 TEST-NET-2
		"203.0.113.0/24",  // RFC 5737 TEST-NET-3
		"2001:db8::/32",   // RFC 3849
		"100.64.0.0/10",   // RFC 6598 运营商级 NAT
		"192.0.0.0/24",    // RFC 6890 IETF 协议专用
		"198.18.0.0/15",   // RFC 2544 基准测试
	}
	out := make([]*net.IPNet, 0, len(raw))
	for _, item := range raw {
		if _, network, err := net.ParseCIDR(item); err == nil {
			out = append(out, network)
		}
	}
	return out
}()

// ---------------------------------------------------------------------------
// 离线判定库
// ---------------------------------------------------------------------------

// OfflineDB 是离线判定库的只读视图。
//
// 用接口而不是具体类型，是为了让"没有库"这个状态变成合法的 nil 实现，
// 而不是满地的 if fileExists。调用方拿到 nil 也要能正常工作。
type OfflineDB interface {
	// Lookup 判定一个公网 IP 的连接类型；查不到返回 nil（不是错误）。
	Lookup(ip string) *Verdict
	// Source 返回库的自述（文件名/版本），用于排查。
	Source() string
}

// ---------------------------------------------------------------------------
// 公网 API 提供方
// ---------------------------------------------------------------------------

// APIProvider 是公网兜底查询的抽象。抽出来的好处与 geo 相同：测试里能用
// 假 provider 覆盖"超时/返回垃圾/返回空"分支，换供应商不用动 Resolver。
type APIProvider interface {
	Lookup(ctx context.Context, client *http.Client, ip string) (*Verdict, error)
	Name() string
}

// ---------------------------------------------------------------------------
// 解析器
// ---------------------------------------------------------------------------

// Resolver 是检测入口。零值不可用，必须用 NewResolver 构造。
type Resolver struct {
	db     OfflineDB
	api    APIProvider
	score  APIProvider
	client *http.Client

	mu         sync.Mutex
	lastAPIErr string
}

// Options 是 NewResolver 的入参。全部可选。
type Options struct {
	// DB 离线判定库，可为 nil（此时只走 API）。
	DB OfflineDB
	// API 兜底提供方，可为 nil（此时离线判定不出就返回空结果）。
	API APIProvider
	// Score 是可选的单 IP 评分提供方（IPQS）。配置了密钥时覆盖评分与风险标记。
	Score APIProvider
	// Client 覆盖 HTTP 客户端（测试用）。nil 时用 safeClient。
	Client *http.Client
}

// NewResolver 构造检测器。
func NewResolver(opts Options) *Resolver {
	client := opts.Client
	if client == nil {
		client = safeClient
	}
	return &Resolver{db: opts.DB, api: opts.API, score: opts.Score, client: client}
}

// Lookup 检测一个 IP。永远返回可用的 Verdict；检测不出时是零值且 Source 为空，
// 并把原因写进 error —— 调用方可以只记日志、不中断主流程。
//
// 与 geo 同样的意志：这只是一个附加能力，绝不能让"加节点/刷新节点"因为
// 第三方服务抖动而失败。所以错误只用于观测，并在库里记进 error 列。
//
// skipOffline：当调用方明确要求"强制走 API 拿评分"时置 true（界面的
// 「深度检测」按钮），此时即使离线库能判连接类型，也仍继续查 API 以补
// 评分/双ISP —— 因为那正是离线库给不出的东西。
func (r *Resolver) Lookup(ctx context.Context, rawIP string, forceAPI bool) (Verdict, error) {
	if r == nil {
		return Verdict{}, errors.New("ippurity resolver 未初始化")
	}
	ip := strings.TrimSpace(rawIP)
	if !IsPublicIP(ip) {
		// 内网/非法输入不是异常，是"这个 IP 没法检测"。
		return Verdict{}, fmt.Errorf("不是可查询的公网 IP: %q", truncateForError(rawIP))
	}

	var verdict Verdict
	haveOffline := false

	// 1. 离线判定。能给连接类型 + ASN，但给不出评分/双ISP。
	if r.db != nil {
		if hit := r.db.Lookup(ip); hit != nil && !hit.Empty() {
			verdict = *hit
			verdict.Source = "offline"
			haveOffline = true
		}
	}

	// 2. 需要评分/双ISP，或离线没判出来 → 走 API。
	//    评分与双ISP是买家最关心的两项，只要 API 可用就尽力补全。
	if r.api != nil && (forceAPI || !offlineComplete(verdict)) {
		apiVerdict, err := r.api.Lookup(ctx, r.client, ip)
		if err != nil {
			r.setAPIErr(err)
			// API 失败不致命：有离线结果就返回离线的（标注来源），
			// 完全没有才返回错误。
			if haveOffline {
				if !errors.Is(err, ErrNoKey) {
					verdict.Partial = "公网 API 查询失败，仅有离线库结果：" + err.Error()
				}
				return verdict, nil
			}
			if errors.Is(err, ErrNoKey) {
				return Verdict{}, errors.New("未配置 ipapi.is 密钥：请在「系统设置 → IP 检测密钥」里填写")
			}
			return Verdict{}, fmt.Errorf("公网纯净度查询失败: %w", err)
		}
		if apiVerdict != nil {
			// API 结果优先用于它独有的字段（评分/双ISP/风险标记）；
			// 连接类型若 API 给了就用 API 的（更权威），否则保留离线的。
			verdict = mergeVerdict(verdict, *apiVerdict)
			verdict.Source = "api"
			haveOffline = true
		}
	}

	if !haveOffline {
		return Verdict{}, errors.New("离线库与公网 API 均未能判定该 IP")
	}
	if verdict.Source == "api" && verdict.ScoreSource == "" {
		verdict.ScoreSource = "ipapi"
	}

	// 3. 单 IP 评分（IPQS）：配置了才查；失败只记录，不影响已有结论。
	if r.score != nil {
		if sv, err := r.score.Lookup(ctx, r.client, ip); err == nil && sv != nil {
			if sv.FraudScore != nil {
				verdict.FraudScore = sv.FraudScore
			}
			if sv.Proxy != nil {
				verdict.Proxy = sv.Proxy
			}
			if sv.VPN != nil {
				verdict.VPN = sv.VPN
			}
			if sv.Abuser != nil {
				verdict.Abuser = sv.Abuser
			}
			verdict.ScoreSource = "ipqs"
			// 写回前克隆 Raw 映射，防止离线库或上游返回共享映射时在并发查询下触发 concurrent map writes 崩溃。
			rawCopy := make(map[string]any, len(verdict.Raw)+1)
			for k, v := range verdict.Raw {
				rawCopy[k] = v
			}
			rawCopy["ipqs"] = sv.Raw
			verdict.Raw = rawCopy
		} else if err != nil && !errors.Is(err, ErrNoKey) {
			r.setAPIErr(err)
			verdict.Partial = "单 IP 评分（IPQS）查询失败：" + err.Error()
		}
	}
	if verdict.IP == "" {
		verdict.IP = ip
	}
	return verdict, nil
}

// offlineComplete 判断一份离线结果是否已经"够用"——
// 连接类型判出来了、且评分与双ISP也有值（离线库若能提供的话）。
// 目前离线库不提供评分/双ISP，所以只要它判出了连接类型，其余仍需要 API。
func offlineComplete(v Verdict) bool {
	return !v.Empty() && v.FraudScore != nil && v.DualISP != nil
}

// mergeVerdict 把 API 结果合并进离线结果。
//
// 规则：API 有值的字段覆盖；API 没值的（nil）保留离线的。这样离线已经
// 判出的连接类型不会被 API 的空字段清掉，而 API 独有的评分/双ISP能补进来。
func mergeVerdict(base, api Verdict) Verdict {
	out := base
	if api.ConnectionType != "" && api.ConnectionType != "unknown" {
		out.ConnectionType = api.ConnectionType
	}
	if api.DualISP != nil {
		out.DualISP = api.DualISP
	}
	if api.FraudScore != nil {
		out.FraudScore = api.FraudScore
	}
	if api.Proxy != nil {
		out.Proxy = api.Proxy
	}
	if api.VPN != nil {
		out.VPN = api.VPN
	}
	if api.Abuser != nil {
		out.Abuser = api.Abuser
	}
	if api.ASN != "" {
		out.ASN = api.ASN
	}
	if api.ASNOrg != "" {
		out.ASNOrg = api.ASNOrg
	}
	if api.Raw != nil {
		out.Raw = api.Raw
	}
	if out.IP == "" {
		out.IP = api.IP
	}
	return out
}

func (r *Resolver) setAPIErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAPIErr = err.Error()
}

// LastAPIError 返回最近一次公网查询的失败原因（供诊断展示）。
func (r *Resolver) LastAPIError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastAPIErr
}

// OfflineSource / HasOfflineDB / HasAPI 供状态展示。
func (r *Resolver) OfflineSource() string {
	if r == nil || r.db == nil {
		return ""
	}
	return r.db.Source()
}

func (r *Resolver) HasOfflineDB() bool { return r != nil && r.db != nil }
func (r *Resolver) HasAPI() bool       { return r != nil && r.api != nil }

func truncateForError(s string) string {
	const maxRunes = 64
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// BoolPtr / IntPtr 是构造判定值的便利函数，供离线库与适配器共用。
func BoolPtr(v bool) *bool { return &v }
func IntPtr(v int) *int    { return &v }
