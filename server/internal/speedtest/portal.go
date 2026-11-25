// portal.go — 通过节点门户的 /api/v1/speedtest 采集三网延迟。
//
// 为什么需要这条通道（2026-10-02）
// --------------------------------
// 三网测速的定义是「节点本机 ping 大陆靶点」—— 只有节点自己知道它回国线路好不好。
// 原实现只有一条路：面板 SSH 到节点跑探针脚本。于是**没绑 SSH 凭据的节点压根测不了**，
// 页面上是四行「未测得」。实测有 2/5 台属于这种（东京-AWS、洛杉矶-NTT）。
//
// 而节点本来就有 Bearer 鉴权的 /api/v1/ 通道（面板的商城对账、开户、回收
// 全靠它）。于是在节点侧 portal 加了 /api/v1/speedtest，由节点自己跑 ping。
// 这条通道的代价是**要改节点侧代码**，所以只能作为补充，不能取代 SSH 通道：
// 老节点没升级就还是只有 SSH 能用。
//
// 两条通道的取舍
// --------------
//   SSH 通道：节点零改动，靶点表可配（speedtest_targets），
//             但需要主机凭据，且凭据失效就整台没数据。
//   portal 通道：不需要任何主机凭据，但要求节点 portal ≥ 装了新端点的版本，
//             且靶点硬编码在节点侧（安全要求，见 SPEEDTEST_TARGETS 的注释）。
//
// 所以 RunOnServer 保持"SSH 优先"：有凭据走 SSH，没有才试 portal。
// 两个通道的返回结构一致，调用方不需要知道数据从哪来。

package speedtest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"nodeops/server/internal/portaltls"
	"strings"
	"time"
)

// portalClient 调节点 portal 用。
//
// ⚠️ 与 httpapi.portalClient 同一套安全姿态，但这里**独立实现**而不是跨包复用：
// speedtest 包不依赖 httpapi（方向相反 —— httpapi 依赖 speedtest），
// 引入反向依赖会成一个环。三处要点照抄，不简化：
//  1. 拒绝回环/内网地址（节点 api_url 是用户填的，不能让它把面板变成内网扫描器）
//  2. 10s 建连 + 整体 deadline
//  3. 拒绝重定向（否则 30x 能把带 Bearer 的请求带到别处）
var portalClient = &http.Client{
	Timeout: 200 * time.Second,
	// 2026-11-03：拒绝重定向。原来没设 CheckRedirect，30x 会把带 Bearer 的请求带到别处
	// （同主机名换端口时 Go 会继续带 Authorization）；与 httpapi.portalClient 一致。
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext:         portalDialer,
		TLSHandshakeTimeout: 10 * time.Second,
		// 默认验证证书；自签节点须信任其 CA，不能全局跳过验证。
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	},
}

// errPortalAddrNotAllowed 表示 api_url 解析出的地址落在回环/私有网段。
var errPortalAddrNotAllowed = errors.New("节点地址指向回环或私有网段，已拒绝")

// portalDialer 拨号时校验目标地址。
//
// ⚠️ 这里踩过一次设计错误，务必读完：最初写成"域名一律拒绝"，
// 理由是"防 DNS rebinding"。真机上一跑，两台节点全部失败：
//
//	连接节点 portal 失败: ... 节点地址 "node1.example.com" 不是 IP 字面量
//
// 而 hysteria_nodes.api_url **存的就是域名**（node2.example.com、node1.example.com…），
// 五台节点一台都不通。安全判断本身没错，**落点错了**：
// 要防的是"域名解析到内网地址"，不是"域名"这个形式。
//
// 正确做法就在下面：自己解析 → 对**每个**解析结果校验 → 再连。
// 顺带比 net.Dialer 强的地方：DNS 可能有多个 A 记录，
// 挨个校验能保证"没有一个解析结果指向内网"，而不是只看第一个。
//
// 为什么这值得较真：api_url 是管理员在面板里填的，看似可信；
// 但它同时也是域名 —— 谁能改 DNS（或等缓存过期），
// 就能把面板变成内网探测器。校验解析结果是这条防护的**唯一有效落点**，
// 禁域名只是碰巧挡住了本例。
func portalDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("节点地址 %q 格式不合法: %w", addr, err)
	}

	// 1) 已经是 IP 字面量 —— 直接校验。
	if ip := net.ParseIP(host); ip != nil {
		if !portalIPAllowed(ip) {
			return nil, fmt.Errorf("%w（%s）", errPortalAddrNotAllowed, host)
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}

	// 2) 域名 —— 自己解析并逐个校验，不交给 Dialer 隐式处理。
	//    LookupIPAddr 走系统解析器，返回的是本次实际会用的候选集。
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析节点域名 %q 失败: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("节点域名 %q 没有解析结果", host)
	}
	for _, candidate := range ips {
		if !portalIPAllowed(candidate.IP) {
			// 任一候选指向内网就整体拒绝，而不是"挑一个安全的连"：
			// 攻击者只要能让一个 A 记录指向 169.254.169.254，
			// "挑第一个"就会在轮询命中时打过去。
			return nil, fmt.Errorf("%w（%s 解析到 %s）", errPortalAddrNotAllowed, host, candidate.IP)
		}
	}

	// 2026-11-03：必须连「刚校验过的那个 IP」。原来这里再传域名给 Dialer，
	// 它会**重新解析**一次：低 TTL 的 DNS 在校验与连接之间换成内网地址，
	// 带着节点 API 密钥的请求就打进了内网（与 collector.safeDialContext 同一做法）。
	// TLS 的 SNI/证书校验仍取 URL 里的域名，不受影响。
	// 按 IPv4 优先依次回退尝试全部已校验地址，避免双栈域名首个 IPv6 不可达导致测速失败。
	d := &net.Dialer{Timeout: 10 * time.Second}
	return portaltls.DialCheckedIPs(ctx, d, network, port, ips)
}

// portalIPAllowed 判断一个解析结果能不能作为节点地址。
//
// 拒绝：回环、私有、链路本地（含 169.254.169.254 云元数据）、未指定。
// 注意**没有**拒绝 IPv6 ULA（fc00::/7）—— net.IP.IsPrivate() 已覆盖。
func portalIPAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// 2026-11-03：与其它出站连接同一口径（portaltls.PublicIP 额外拒绝 CGNAT / NAT64 等）。
	return portaltls.PublicIP(ip)
}

// portalSpeedResult 是节点侧端点返回的单个靶点结果。
type portalSpeedResult struct {
	TargetKey   string   `json:"target_key"`
	TargetName  string   `json:"target_name"`
	TargetHost  string   `json:"target_host"`
	Carrier     string   `json:"carrier"`
	ICMPAvgMs   *float64 `json:"icmp_avg_ms"`
	ICMPLossPct *float64 `json:"icmp_loss_pct"`
}

// portalSpeedResponse 是 /api/v1/speedtest 的响应信封。
type portalSpeedResponse struct {
	OK              bool                `json:"ok"`
	Error           string              `json:"error"`
	ErrorLabel      string              `json:"error_label"`
	Results         []portalSpeedResult `json:"results"`
	MeasuredTargets int                 `json:"measured_targets"`
	TotalTargets    int                 `json:"total_targets"`
}

// errPortalNoSpeedtest 表示节点 portal 还没升级到带测速端点的版本。
//
// 这是一个**预期内**的常态，不是故障：老节点跑着旧版 portal.py，
// 它对未知子路径回 404 `API endpoint not found`。
// 把它单列成一个哨兵错误，是为了让调用方能说清
// "这台节点的 portal 还没升级" 而不是笼统的"测速失败"。
var errPortalNoSpeedtest = errors.New("节点 portal 尚未提供测速端点（需升级 portal）")

// RunOnPortal 通过节点门户的 API 在**节点本机**跑一轮测速。
//
// 返回的结果与 SSH 通道同构（[]Result），调用方可以直接落库。
// 采不到时返回 errPortalNoSpeedtest 或具体错误，绝不返回"空结果但看起来成功"。
func (p *Prober) RunOnPortal(ctx context.Context, nodeID, baseURL, apiKey string) ([]Result, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" || apiKey == "" {
		return nil, errors.New("节点未绑定 portal API")
	}

	// 节点侧端点最长要跑 18 个靶点 × 6 秒 ≈ 108 秒。
	// 这里给 probeTimeout 同款预算再加余量，避免面板先超时、
	// 节点还在跑（那样会留下一条半截数据，且用户看到的是"没数据"）。
	timeout := probeTimeout(SPEEDTEST_TARGET_COUNT_HINT) * 2
	if timeout < 2*time.Minute {
		timeout = 2 * time.Minute
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/api/v1/speedtest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client, err := portaltls.Client(portalClient, req.URL.String())
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接节点 portal 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return nil, fmt.Errorf("节点 portal 返回重定向（HTTP %d），已拒绝跟随", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("节点 portal 鉴权失败（HTTP %d），请检查节点 API 密钥", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, errors.New("读取节点响应失败")
	}

	var parsed portalSpeedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return nil, errPortalNoSpeedtest
		}
		return nil, fmt.Errorf("节点 portal 返回 %d，且响应不是合法 JSON（可能不是 portal）", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errPortalNoSpeedtest
	}
	if !parsed.OK {
		// 节点侧明确报错时，把它的原话带上来 —— 例如 NO_PING
		// （节点没装 ping）是有用的可操作信息，不能吞掉换成"测速失败"。
		if parsed.ErrorLabel != "" {
			return nil, errors.New(parsed.ErrorLabel)
		}
		if parsed.Error != "" {
			return nil, errors.New("节点 portal 报错: " + parsed.Error)
		}
		return nil, fmt.Errorf("节点 portal 返回 %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("节点 portal 返回异常状态码 %d", resp.StatusCode)
	}
	if len(parsed.Results) == 0 {
		return nil, errors.New("节点 portal 返回 0 个靶点")
	}

	results := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if !validTargetKey(r.TargetKey) {
			// 节点侧回了不认识的结构化靶点标识 —— 宁可丢掉这条，
			// 也不要写进库里（前端按 carrier 分组，脏值会让整组凭空消失）。
			continue
		}
		results = append(results, Result{
			TargetKey:   r.TargetKey,
			Name:        r.TargetName,
			Host:        r.TargetHost,
			Carrier:     r.Carrier,
			ICMPAvgMs:   clampPtrPtr(r.ICMPAvgMs, 0, 60000),
			ICMPLossPct: clampPtrPtr(r.ICMPLossPct, 0, 100),
			ProbeOK:     true,
		})
	}
	if len(results) == 0 {
		return nil, errors.New("节点 portal 返回的靶点全部无法识别")
	}
	return results, nil
}

// clampPtrPtr 对**可空**的数值做区间夹取，null 保持 null。
//
// ⚠️ 这里第一版写的是 clampPtr(deref(v), lo, hi) —— deref 把 nil 变成 0，
// 于是「100% 丢包、完全没测到」被写成「icmp_avg_ms = 0」。
// 界面上就显示成 "0 ms"，和"延迟真的是 0"（同机房才可能）完全无法区分。
// 这正是 Result 注释里写的那条铁律（必须能区分"测到 0ms"与"没测到"），
// 而我在同一批改动里违反了它。写完单测立刻就抓到了 ——
// 这条断言不是装饰，是它唯一的价值所在。
func clampPtrPtr(v *float64, low, high float64) *float64 {
	if v == nil {
		return nil
	}
	return clampPtr(*v, low, high)
}

// SPEEDTEST_TARGET_COUNT_HINT 是节点侧硬编码靶点数的**提示值**，
// 只用来给 HTTP 预算留够时间，不参与任何数据判定。
//
// 为什么放在这而不是直接写 18：改节点侧靶点清单时这里可能要跟着动，
// 但它错了只会让超时偏大或偏小，不会让数据错 —— 与真正的靶点表解耦。
var SPEEDTEST_TARGET_COUNT_HINT = 18
