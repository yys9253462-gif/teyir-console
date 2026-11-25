// Package geo — 把「一个 IP 属于哪个地区、哪家 VPS 商家」变成一件可自动完成的事。
//
// 为什么需要它
// ------------
// 控制台的 servers / xui_panels 都有 region 与 provider 两列，但此前全靠人手打
// （界面上就是 placeholder="Tokyo" / placeholder="AWS"）。运维者每加一台机器，
// 都要自己回忆「这台是法兰克福的 Hetzner 还是赫尔辛基的 Hetzner」，
// 而这件事完全可以从 IP 推出来。
//
// 设计取舍：离线库优先，公网 API 兜底
// -----------------------------------
// 两条路各有一半缺点，所以都要，且离线优先：
//
//  1. 离线库（IP 段 → 地区/ASN）。优点：不联网、不泄漏被查 IP、无限速、稳定。
//     缺点：包体积大，且新分配的 IP 段可能还没有数据。
//  2. 公网 API（ip-api.com 之类）。优点：总有数据。缺点：要联网（而被本面板
//     管理的机器经常没有出网口）、有速率限制、免费版通常只允许非商用，
//     而且会把「我有哪几台机器」的 IP 清单持续告诉第三方。
//
// 所以：**有库就查库，查不到或没库才走 API**，且 API 结果会缓存进库（见 Cache），
// 让同一台机器第二次就不再出网。
//
// 安全姿态与项目一致
// ------------------
// 本包所有出网请求都走 SafeClient()：它复用 nodeapi.go 里那套拨号器语义 ——
// 解析目标 IP 后拒绝非公网地址、拒绝重定向。geo API 的地址虽然是我们自己写死的
// 常量，但一旦将来做成可配置（或有人改了代码里的 host），没有这层就是一条
// 「把控制台变成内网探测跳板」的路。宁可多一层。
//
// 另有一条本包独有的硬规则：**只接受公网 IP 作为查询输入**。
// 面板的 IP 来自节点自报（node/meta 的 public_ip）或用户手填，都属于外部输入；
// 拿内网地址去问 geo 服务既无意义，又会把内网拓扑当参数发出去。
package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Info 是一次识别结果。
//
// Region 与 Provider 是给用户看的**短标签**（"Tokyo"、"AWS"），不是全称；
// 因为它们的最终用途是拼进机器名（"Tokyo-AWS"），太长会没法看。
// 全称留在 RegionLong / ProviderLong 里备用。
type Info struct {
	// Region 是短地区名，如 "Tokyo"、"Frankfurt"。
	Region string `json:"region"`
	// RegionLong 是地区全称，如 "Tokyo, Japan"。
	RegionLong string `json:"region_long,omitempty"`
	// Country 是 ISO 国家代码（大写两位），如 "JP"。命名与排序用不上，
	// 但排查「为什么识别成这个地区」时是最硬的依据。
	Country string `json:"country,omitempty"`
	// Provider 是 VPS 商家短名，如 "AWS"、"Hetzner"、"Vultr"。
	Provider string `json:"provider,omitempty"`
	// ProviderLong 是 ASN 组织全称，如 "Amazon.com, Inc."。
	ProviderLong string `json:"provider_long,omitempty"`
	// ASN 是自治域号，如 16509。Provider 的推导依据。
	ASN string `json:"asn,omitempty"`
	// Source 记录这个结果从哪来：offline / api / cache。
	// 出问题时第一个要看的就是它。
	Source string `json:"source,omitempty"`
}

// Empty 表示什么都没识别出来。调用方据此决定不改动任何字段。
//
// 纳入 Country：命名规则已改成「国家-城市」，只识别到国家（城市缺失，
// 例如 ip-api 只给 country 不给 city）也算有结果，不能让 applyGeo 因为
// Empty 判真而整体跳过、连国家都不写进名字。
func (i Info) Empty() bool {
	return i.Region == "" && i.Provider == "" && i.Country == ""
}

// CountryName 返回中文国家名（"日本"、"美国"）。
//
// Country 字段存的是 ISO 码（"JP"），这里过一遍 zhCountry 翻译。
// 未收录的码原样返回（拿到 "XX" 也比空强，运维能自行判断）。
func (i Info) CountryName() string {
	return zhCountry(i.Country)
}

// Name 把识别结果拼成「国家-城市」形式的机器名（2026-10-09 规则变更）。
//
// 旧格式是「城市-商家」（Tokyo-AWS）。管理员要求改成「国家-区域」、且名字里
// 不再出现商家 —— 商家仍单独存在 Provider 字段里，只是不进名字。
//
// 只有一边有值时就只返回那一边 —— 宁可得到 "日本" 或 "东京"，
// 也不要 "日本-" 或 "-东京" 这种带孤零零连字符的名字。
// 两边都没有则返回空串，调用方应当保持原有名字不动。
func (i Info) Name() string {
	country := strings.TrimSpace(i.CountryName())
	region := strings.TrimSpace(i.Region)
	switch {
	case country != "" && region != "":
		return country + "-" + region
	case country != "":
		return country
	default:
		return region
	}
}

// ---------------------------------------------------------------------------
// 出网客户端
// ---------------------------------------------------------------------------

// newSafeClient 构造一个只允许访问公网、且不跟随重定向的 HTTP 客户端。
//
// 与 httpapi.newPortalClient 是同一套语义，这里独立实现而不是导出复用，
// 是因为那一份在 httpapi 包里、且被注释明确标注为「节点 API 专用」；
// geo 的失败模式与它不同（geo 挂了只是识别不出，不该影响节点管理），
// 所以超时给得更短、也不共享连接池。
func newSafeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("geo 查询不接受重定向")
		},
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("解析 %s 失败: %w", host, err)
				}
				for _, ip := range ips {
					if !ip.IP.IsGlobalUnicast() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() {
						return nil, fmt.Errorf("%s 解析到非公网地址，已拒绝访问", host)
					}
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
			},
		},
	}
}

// SafeClient 返回共享的安全客户端（geo 查询专用）。
var safeClient = newSafeClient()

// IsPublicIP 判断输入是否是可查询的公网 IP 字面量。
//
// 明确拒绝的：私有网段、环回、链路本地（含 169.254.169.254 云元数据端点）、
// 组播、未指定地址、以及任何带端口的输入（那不是 IP）。
// 域名也不接受 —— 识别对象是「这台机器在哪」，先把名字解析成 IP 是调用方的事，
// 且解析结果必须再复核一次（见 Lookup 的注释）。
func IsPublicIP(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" {
		return false
	}
	// 方括号与斜杠一律不认：那是「带端口的 IPv6」或 CIDR，不是裸 IP。
	//
	// ⚠️ 这里**不能**用 `strings.Count(value, ":") > 1` 来挡「带端口的 IPv4」——
	// 合法的 IPv6 地址天生就有多个冒号，那样写会把每一条 IPv6 都误杀
	// （单测 TestIsPublicIPRejectsNonPublic 抓到过这个 bug）。
	// 带端口的 IPv4 由 net.ParseIP 自然拒绝（"1.2.3.4:443" 不是合法 IP），
	// 所以不需要额外规则。
	if strings.ContainsAny(value, "[]/") {
		return false
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	// 文档/保留段（RFC 3849 的 2001:db8::/32、RFC 5737 的 192.0.2.0/24 等）
	// Go 的 IsGlobalUnicast 一律返回 true，但现实中它们不可能是 VPS 的公网地址。
	// 拿它们去查 geo 服务纯属浪费一次配额，直接在入口挡掉。
	// （这一条是实测确认过的：`IsGlobalUnicast("2001:db8::1") == true`。）
	if reservedForDocumentation(ip) {
		return false
	}
	return true
}

// reservedForDocumentation 判断是否为文档/示例保留地址段。
func reservedForDocumentation(ip net.IP) bool {
	for _, cidr := range documentationNets {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// documentationNets 是 RFC 里为文档与示例保留、不可用于真实公网的网段。
var documentationNets = func() []*net.IPNet {
	raw := []string{
		"192.0.2.0/24",    // RFC 5737 TEST-NET-1
		"198.51.100.0/24", // RFC 5737 TEST-NET-2
		"203.0.113.0/24",  // RFC 5737 TEST-NET-3
		"2001:db8::/32",   // RFC 3849
		"100.64.0.0/10",   // RFC 6598 运营商级 NAT（非公网可路由）
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
// 离线库
// ---------------------------------------------------------------------------

// OfflineDB 是离线归属库的只读视图。
//
// 用接口而不是具体类型，是为了让「没有库」这个状态变成合法的 nil 实现，
// 而不是满地的 if fileExists。调用方拿到 nil 也要能正常工作。
type OfflineDB interface {
	// Lookup 查一个公网 IP；查不到返回 nil（不是错误）。
	Lookup(ip string) *Info
	// Source 返回库的自述（文件名/版本），用于排查。
	Source() string
}

// ---------------------------------------------------------------------------
// 解析器
// ---------------------------------------------------------------------------

// Resolver 是识别入口。零值不可用，必须用 NewResolver 构造。
type Resolver struct {
	db     OfflineDB
	cache  *Cache
	api    APIProvider
	client *http.Client
	// now 可在测试里替换，用于验证缓存过期。
	now func() time.Time

	mu sync.Mutex
	// lastAPIErr 记录最近一次 API 失败原因，供 /api/geo/status 之类的
	// 诊断端点回显。不参与识别逻辑。
	lastAPIErr string
}

// Options 是 NewResolver 的入参。全部可选。
type Options struct {
	// DB 离线库，可为 nil（此时只走 API）。
	DB OfflineDB
	// Cache 结果缓存，可为 nil（此时不缓存，每次都真查）。
	Cache *Cache
	// API 兜底提供方，可为 nil（此时查不到就返回空结果）。
	API APIProvider
	// Client 覆盖 HTTP 客户端（测试用）。nil 时用 safeClient。
	Client *http.Client
}

// NewResolver 构造识别器。
func NewResolver(opts Options) *Resolver {
	client := opts.Client
	if client == nil {
		client = safeClient
	}
	return &Resolver{
		db:     opts.DB,
		cache:  opts.Cache,
		api:    opts.API,
		client: client,
		now:    time.Now,
	}
}

// Lookup 识别一个 IP。永远返回可用的 Info；识别不出时是零值且 Source 为空，
// 并把原因写进 error —— 调用方可以只记日志、不中断主流程。
//
// 注意这里**不返回致命错误**的意志：识别只是锦上添花，绝不能让「加节点」
// 因为第三方服务抖动而失败。所以错误只用于观测。
func (r *Resolver) Lookup(ctx context.Context, rawIP string) (Info, error) {
	if r == nil {
		return Info{}, errors.New("geo resolver 未初始化")
	}
	ip := strings.TrimSpace(rawIP)
	if !IsPublicIP(ip) {
		// 内网/非法输入不是异常，是「这台机器没法识别」。
		return Info{}, fmt.Errorf("不是可查询的公网 IP: %q", truncateForError(rawIP))
	}

	// 1. 缓存。同一台机器反复加节点/刷新面板时，避免重复出网。
	if r.cache != nil {
		if hit, ok := r.cache.Get(ip); ok {
			hit.Source = "cache"
			translateInPlace(&hit)
			return hit, nil
		}
	}

	// 2. 离线库。
	if r.db != nil {
		if info := r.db.Lookup(ip); info != nil && !info.Empty() {
			info.Source = "offline"
			r.store(ip, *info)
			translateInPlace(info)
			return *info, nil
		}
	}

	// 3. 公网 API。
	if r.api != nil {
		info, err := r.api.Lookup(ctx, r.client, ip)
		if err != nil {
			r.setAPIErr(err)
			return Info{}, fmt.Errorf("公网 geo 查询失败: %w", err)
		}
		if info != nil && !info.Empty() {
			info.Source = "api"
			r.store(ip, *info)
			translateInPlace(info)
			return *info, nil
		}
	}

	return Info{}, errors.New("离线库与公网 API 均未能识别该 IP")
}

func (r *Resolver) store(ip string, info Info) {
	if r.cache == nil {
		return
	}
	r.cache.Put(ip, info, r.now())
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

// OfflineSource 返回离线库自述，没有库时为空串。
func (r *Resolver) OfflineSource() string {
	if r == nil || r.db == nil {
		return ""
	}
	return r.db.Source()
}

// HasOfflineDB 用于状态展示：区分「查不到」和「根本没有库」。
func (r *Resolver) HasOfflineDB() bool {
	return r != nil && r.db != nil
}

// HasAPI 用于状态展示。
func (r *Resolver) HasAPI() bool {
	return r != nil && r.api != nil
}

// truncateForError 防止把超长输入原样写进日志/错误信息。
func truncateForError(s string) string {
	const max = 64
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ---------------------------------------------------------------------------
// 兜底 API：ip-api.com
// ---------------------------------------------------------------------------

// APIProvider 是公网兜底查询的抽象。抽出来有两个实际好处：
// 测试里能用一个假 provider 覆盖「API 超时/返回垃圾/返回空」这些分支，
// 以及将来换供应商（或加第二个）不用动 Resolver。
type APIProvider interface {
	Lookup(ctx context.Context, client *http.Client, ip string) (*Info, error)
	Name() string
}

// IPAPI 走 ip-api.com 的免费 JSON 接口。
//
// 为什么选它：无需注册、无需密钥（有密钥的都要账号，会让「开箱即用」变成
// 「先去注册」）、一次请求同时返回国家/城市与 ASN 组织 —— 正是我们要的两样。
//
// 免费版的硬约束（2026-10-01 查证）：
//   - 限速 45 请求/分钟（超了返回 429，本实现按失败处理并让缓存吸收）
//   - **仅允许非商业用途**，商用需换付费端点
//   - http 明文；https 需要付费计划
//
// 明文这一条我选择接受，理由是：查询内容只有「一个公网 IP 的地理归属」，
// 不含任何凭据；而这个 IP 本来就是我方服务器的公网地址，属公开信息。
// 反过来若为了加密而强制 https，免费版做不到，就只能要求用户去付费/注册，
// 那会把这个功能从「装上就有」变成「先配置再说」。取舍记在这里，便于将来复核。
// translateInPlace 在出口处把英文地区名翻成中文。
//
// 为什么是 in_place + 在 Lookup 出口做一次：
//   - 单一职责：cache / 离线库 / 公网 API 三个出口全部过这一道，
//     不会有"这条是中文那条是英文"的混乱
//   - 不污染源：离线库 JSON 保留英文原文（便于排障时与 ip-api 对齐）
//   - provider 按设计不翻译（保留 AWS/Alibaba Cloud），这里只动 Region / RegionLong
//   - 长名（"San Francisco, United States"）保留英文，不强翻 ——
//     用户如果想看短中文，先看 Region 列
//
// ⚠️ 为什么**不**写缓存层也翻译：
// 翻译函数读表 + 字符串比较，纯 CPU、几乎零成本，没必要为它再开缓存。
func translateInPlace(info *Info) {
	if info == nil {
		return
	}
	info.Region = zhRegion(info.Region)
	// RegionLong 是 "城市, 国家" 形式，国家段不在翻译表里，
	// 整体保留原文（"东京, 日本" 反而是噪音，运维更熟悉"Tokyo, Japan"）。
}

type IPAPI struct {
	// BaseURL 可覆盖（测试用）。默认 http://ip-api.com/json。
	BaseURL string
}

// Name 实现 APIProvider。
func (p IPAPI) Name() string { return "ip-api.com" }

// ipAPIResponse 是 ip-api.com 的返回结构（只取用得到的字段）。
type ipAPIResponse struct {
	Status      string `json:"status"` // success / fail
	Message     string `json:"message"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	City        string `json:"city"`
	RegionName  string `json:"regionName"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	AS          string `json:"as"` // 形如 "AS16509 Amazon.com, Inc."
	Query       string `json:"query"`
}

// Lookup 实现 APIProvider。
func (p IPAPI) Lookup(ctx context.Context, client *http.Client, ip string) (*Info, error) {
	base := p.BaseURL
	if base == "" {
		base = "http://ip-api.com/json"
	}
	// 明确只要这些字段，减小响应体；fields 是 ip-api 的正式参数。
	endpoint := strings.TrimRight(base, "/") + "/" + ip +
		"?fields=status,message,country,countryCode,city,regionName,isp,org,as,query"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hy2-ops-console")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 429 也走这里：限速是我们自己的问题，不该重试打爆对方。
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload ipAPIResponse
	// 2026-11-03：响应体限 64 KiB（正常不到 1 KiB）。原来不设上限，上游异常或被劫持时
	// 一个超大响应会被整个读进内存；面板其它外呼（portal、ipapi.is）都有同样的限制。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("响应无法解析: %w", err)
	}
	if payload.Status != "success" {
		if payload.Message != "" {
			return nil, errors.New(payload.Message)
		}
		return nil, errors.New("查询未成功")
	}

	info := normalizeIPAPI(payload)
	return &info, nil
}

// normalizeIPAPI 把 ip-api 的原始字段折成 Info。
//
// 这里是全部「命名质量」的所在，所以单独抽出来做纯函数、单测覆盖：
// 同样是 AWS 的机器，ip-api 可能给 "Amazon.com, Inc."、"Amazon Technologies Inc."
// 或 "AMAZON-02" 三种写法（取决于它命中的是哪条 ASN 记录）；
// 而我们要的是稳定的 "AWS"。识别不出来就退回 ASN 组织原文，
// 但至少把明显的噪音（多余空格、全大写、逗号后缀）收拾干净。
func normalizeIPAPI(p ipAPIResponse) Info {
	info := Info{
		Country: strings.ToUpper(strings.TrimSpace(p.CountryCode)),
		ASN:     extractASN(p.AS),
	}

	// 地区：优先「城市」，因为运维说的「法兰克福那台」指的就是城市；
	// 没有城市才退到国家名。ip-api 的 city 有时是 "Tokyo"，
	// 有时带行政区后缀，统一过一遍清洗。
	city := cleanPlaceName(p.City)
	country := cleanPlaceName(p.Country)
	switch {
	case city != "" && country != "":
		info.Region = city
		info.RegionLong = city + ", " + country
	case country != "":
		info.Region = country
		info.RegionLong = country
	}

	// 商家：先试已知商家匹配（把 Amazon 系列的各种写法归一到 AWS），
	// 失败则退回清洗后的组织名。
	org := firstNonEmpty(p.Org, p.ISP)
	info.ProviderLong = strings.TrimSpace(org)
	if short, ok := matchKnownProvider(info.ProviderLong, p.AS, p.ISP); ok {
		info.Provider = short
	} else {
		info.Provider = cleanProviderName(info.ProviderLong)
	}
	return info
}

// extractASN 从 "AS16509 Amazon.com, Inc." 里取出 "AS16509"。
func extractASN(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return ""
	}
	head := fields[0]
	if len(head) > 2 && (head[:2] == "AS" || head[:2] == "as") {
		digits := head[2:]
		for _, r := range digits {
			if r < '0' || r > '9' {
				return ""
			}
		}
		if digits != "" {
			return "AS" + digits
		}
	}
	return ""
}

// cleanPlaceName 收拾地名：去多余空格、去掉 ip-api 偶尔带上的括号补充。
func cleanPlaceName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 去掉括号注释，如 "Tokyo (Nerima)"。
	if idx := strings.IndexAny(s, "("); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// cleanProviderName 把 ASN 组织名收拾成能进机器名的短标签。
//
// 处理的是实测里最常见的三种噪音：
//   - 法律后缀："Hetzner Online GmbH" → 去掉 GmbH/Inc./Ltd. 这类
//   - 纯大写 ASN 名："AMAZON-02" → 这是 ASN 的记录名，不是给人看的
//   - 多段业务名："Google Cloud (us-central1)" → 取主体
//
// 收拾不动的就原样返回（宁可长一点，也别猜错）。
func cleanProviderName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if idx := strings.IndexAny(s, "("); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// 去掉法律实体后缀（大小写不敏感，按词匹配）。
	drop := map[string]bool{
		"gmbh": true, "inc": true, "inc.": true, "ltd": true, "ltd.": true,
		"llc": true, "llc.": true, "corp": true, "corp.": true, "co.": true,
		"sa": true, "s.a.": true, "bv": true, "b.v.": true, "ag": true,
		"pte": true, "pty": true, "srl": true, "s.r.l.": true, "oy": true,
		"ab": true, "as": true, "kk": true, "k.k.": true,
	}
	words := strings.Fields(s)
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if drop[strings.ToLower(w)] {
			continue
		}
		kept = append(kept, w)
	}
	if len(kept) == 0 {
		return strings.TrimSpace(raw)
	}
	s = strings.Join(kept, " ")

	// 全大写的 ASN 记录名（"AMAZON-02"、"OVH SAS"）不是给人看的名字，
	// 保留原样也比编造一个好；只把明显是编号后缀的 "-数字" 去掉。
	if s == strings.ToUpper(s) && strings.Contains(s, "-") {
		parts := strings.SplitN(s, "-", 2)
		if len(parts) == 2 && isAllDigits(parts[1]) {
			s = parts[0]
		}
	}
	return strings.TrimSpace(s)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 已知商家归一
// ---------------------------------------------------------------------------

// knownProviders 把各家云厂商/独立服务器商在 ASN 数据里的多种写法归一到
// 一个稳定短名。
//
// 为什么需要它：同一家 AWS 的机器，ip-api 可能返回
// "Amazon.com, Inc." / "Amazon Technologies Inc." / "Amazon Data Services Japan"
// —— 直接拿来当机器名，同一批机器会得到三种不同前缀，等于白识别。
//
// 匹配规则是「对 org/isp 原文 + ASN 号做小写子串匹配」，顺序敏感：
// **先匹配更具体的**（Amazon Data Services 属于 AWS 之前，要先排除它是别的），
// 所以列表从具体到泛化排列。
type knownProvider struct {
	// Short 是归一后的短名。
	Short string
	// Patterns 是命中条件（小写子串），任一命中即认定。
	Patterns []string
	// ASNs 是 ASN 号（形如 "AS16509"），命中同样认定。
	ASNs []string
}

var knownProviders = []knownProvider{
	// —— 三大云 ——
	{Short: "AWS", Patterns: []string{"amazon", "aws", "amazonaws"}, ASNs: []string{"AS16509", "AS14618", "AS8987", "AS38895"}},
	{Short: "Azure", Patterns: []string{"microsoft", "azure"}, ASNs: []string{"AS8075", "AS8068"}},
	{Short: "GCP", Patterns: []string{"google"}, ASNs: []string{"AS396982", "AS15169", "AS19527"}},
	{Short: "Oracle", Patterns: []string{"oracle"}, ASNs: []string{"AS31898", "AS14589"}},

	// —— 主流 VPS 商家 ——
	{Short: "Hetzner", Patterns: []string{"hetzner"}, ASNs: []string{"AS24940", "AS213230"}},
	{Short: "OVH", Patterns: []string{"ovh"}, ASNs: []string{"AS16276", "AS35540"}},
	{Short: "Vultr", Patterns: []string{"vultr", "choopa", "constant"}, ASNs: []string{"AS20473"}},
	{Short: "DigitalOcean", Patterns: []string{"digitalocean", "digital ocean"}, ASNs: []string{"AS14061", "AS46652"}},
	{Short: "Linode", Patterns: []string{"linode", "akamai"}, ASNs: []string{"AS63949", "AS20940"}},
	{Short: "Contabo", Patterns: []string{"contabo"}, ASNs: []string{"AS51167", "AS40021"}},
	{Short: "Cloudflare", Patterns: []string{"cloudflare"}, ASNs: []string{"AS13335"}},
	{Short: "Scaleway", Patterns: []string{"scaleway", "online sas", "iliad"}, ASNs: []string{"AS12876"}},
	{Short: "Netcup", Patterns: []string{"netcup"}, ASNs: []string{"AS197540"}},
	{Short: "RackNerd", Patterns: []string{"racknerd"}, ASNs: []string{"AS36352", "AS33182"}},
	{Short: "BandwagonHost", Patterns: []string{"bandwagon", "it7 networks"}, ASNs: []string{"AS25820"}},
	{Short: "Oracle Cloud", Patterns: []string{"oracle cloud"}, ASNs: nil},
	{Short: "Alibaba Cloud", Patterns: []string{"alibaba", "aliyun", "alicloud"}, ASNs: []string{"AS45102", "AS37963"}},
	{Short: "Tencent Cloud", Patterns: []string{"tencent"}, ASNs: []string{"AS45090", "AS132203"}},
	{Short: "Huawei Cloud", Patterns: []string{"huawei"}, ASNs: []string{"AS136907", "AS55990"}},
	{Short: "SoftBank", Patterns: []string{"softbank"}, ASNs: []string{"AS17676"}},
	{Short: "M247", Patterns: []string{"m247"}, ASNs: []string{"AS9009"}},
	{Short: "Leaseweb", Patterns: []string{"leaseweb"}, ASNs: []string{"AS60781", "AS28753"}},
	{Short: "ColoCrossing", Patterns: []string{"colocrossing", "hostwinds", "hivelocity"}, ASNs: []string{"AS36352", "AS54290"}},
	{Short: "Gcore", Patterns: []string{"gcore", "g-core"}, ASNs: []string{"AS199524", "AS202422"}},
	{Short: "Cherry Servers", Patterns: []string{"cherry"}, ASNs: []string{"AS204133", "AS59642"}},
	{Short: "GreenCloud", Patterns: []string{"greencloud"}, ASNs: []string{"AS42831", "AS199524"}},
	{Short: "ByteVirt", Patterns: []string{"bytevirt"}, ASNs: nil},
	{Short: "Akile", Patterns: []string{"akile"}, ASNs: nil},
	{Short: "DMIT", Patterns: []string{"dmit"}, ASNs: []string{"AS906", "AS54574"}},
	{Short: "Zenlayer", Patterns: []string{"zenlayer"}, ASNs: []string{"AS21859"}},
	{Short: "NTT", Patterns: []string{"ntt"}, ASNs: []string{"AS2914"}},
	{Short: "Cogent", Patterns: []string{"cogent"}, ASNs: []string{"AS174"}},
	{Short: "Hurricane Electric", Patterns: []string{"hurricane electric", "he.net"}, ASNs: []string{"AS6939"}},
}

// knownASNIndex 由 knownProviders 预计算，避免每次查询都线性扫每个 pattern。
var knownASNIndex = func() map[string]string {
	idx := make(map[string]string)
	for _, kp := range knownProviders {
		for _, asn := range kp.ASNs {
			// 先到先得：列表是具体→泛化排序的，靠前的更具体，不该被后面的
			// 泛化条目覆盖（例如 AS36352 既在 RackNerd 也在 ColoCrossing，
			// 保留靠前的那个）。
			if _, exists := idx[asn]; !exists {
				idx[asn] = kp.Short
			}
		}
	}
	return idx
}()

// matchKnownProvider 尝试把原始组织信息归一到已知商家短名。
//
// 匹配优先级：**ASN 号 > 组织名子串**。
// 理由：ASN 是权威且唯一的（一个 ASN 只属于一家），而组织名子串会误伤
// —— 例如 "Amazon" 也出现在某些 CDN 转售商的名字里。
// 但 ASN 数据不一定有（离线库可能只有国家），所以子串是必要的退路。
func matchKnownProvider(org, asRaw, isp string) (string, bool) {
	if asn := extractASN(asRaw); asn != "" {
		if short, ok := knownASNIndex[asn]; ok {
			return short, true
		}
	}
	// 组织名 + ISP 名一起做子串匹配（有的库把组织放在 isp 里）。
	haystack := strings.ToLower(firstNonEmpty(org, "") + " " + firstNonEmpty(isp, ""))
	if strings.TrimSpace(haystack) == "" {
		return "", false
	}
	for _, kp := range knownProviders {
		for _, pattern := range kp.Patterns {
			if strings.Contains(haystack, pattern) {
				return kp.Short, true
			}
		}
	}
	return "", false
}

// KnownProviders 返回已知商家短名清单（供前端做下拉/提示，也为测试固定住集合）。
func KnownProviders() []string {
	out := make([]string, 0, len(knownProviders))
	for _, kp := range knownProviders {
		out = append(out, kp.Short)
	}
	return out
}
