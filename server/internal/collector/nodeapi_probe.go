package collector

// nodeapi_probe.go — 通过**节点门户 API** 探测节点在线状态。
//
// 为什么需要这条路径（2026-10-02 生产实测暴露的缺口）
// --------------------------------------------------
// 状态探测此前只有 SSH 一条路：SSH 到节点所属主机，在主机上跑
// `ss -lun | grep :端口` 看监听。它要求那台主机配了 SSH 凭据。
//
// 而「通过 API 导入的节点」恰恰**刻意不带 SSH 凭据** —— 见
// httpapi.ensureHostForImportedNode 的注释：不想让运维者为了导入一个
// 节点而去编一个他可能没有的 SSH 用户。
//
// 两条设计撞在一起就成了死结：
//   API 导入的节点 → 主机无 SSH 凭据 → collector 直接 return
//   → hysteria_nodes.status 永远是 unknown、last_checked_at 永远是 null
//   → 界面上永久显示「未知 / 未探测」，而节点的 API 其实是通的
//
// 典型场景示例：
//   node2.example.com     有 SSH 凭据 → online
//   node1.example.com 无 SSH 凭据 → unknown（但它的 API 返回了
//                     listen_port/users_count，一切正常）
//   node3.example.com      无 SSH 凭据 → unknown（这台是真挂了，API 也超时）
//
// 修法：**先试 API，API 不可用再退回 SSH**。顺序的理由见 probeStatusViaAPI
// 的注释 —— API 更便宜（一次 HTTPS，不用建 SSH 会话），且 API 能通本身
// 就证明了"节点门户活着"，那正是"节点在线"的定义。
//
// 为什么安全姿态要自己写一遍（而不是复用 httpapi.portalClient）
// -----------------------------------------------------------
// portalCall 在 httpapi 包里是未导出的，且 collector 与 httpapi 是
// 平级包（collector 不能 import httpapi，那会造成循环依赖：
// httpapi 已经 import 了 collector 用它启动采集器）。
// 所以这里独立实现，但**语义必须与 portalClient 一致**：
//   - 拒绝解析到私网/环回/链路本地的地址（防 SSRF）
//   - 拒绝重定向（防跳板）
//   - 明确的超时
// 这三条是安全底线，任何一条漏了都会把面板变成内网探测跳板。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"nodeops/server/internal/portaltls"
	"regexp"
	"strings"
	"time"
)

// nodeAPIProbe 是一次 API 探测的结果。
type nodeAPIProbe struct {
	// OK 表示 API 调用成功且返回了有效数据。
	OK bool
	// ErrorKind 在 OK=false 时说明失败类型：
	//   unreachable  —— 网络不通/超时（节点可能真的挂了）
	//   unauthorized —— 鉴权失败（节点活着，但我们的密钥不对）
	//   invalid      —— 地址非法或响应无法解析
	ErrorKind string
	// Detail 是给日志与界面看的简短原因。
	Detail string
	// NodeTime 是节点自己上报的时间戳（秒）。
	// 与面板时间对比可以判断节点的时钟是否漂移 —— 证书续期与
	// 订阅到期都依赖时间，时钟漂移是很隐蔽的故障源。
	NodeTime int64
	// ListenPort 是节点上报的监听端口，可与库里记录的值比对，
	// 发现"节点改了端口但面板没同步"。
	ListenPort int
}

// probeStatusViaAPI 调节点的 /api/v1/node/meta 判断它是否在线。
//
// 为什么用 node/meta 而不是更轻的端点：节点的 API 里没有专门的
// 健康检查端点，node/meta 是最轻的只读调用（返回几个标量字段），
// 且它同时给了 listen_port 与 node_time 这两个有用的附带信息。
//
// 为什么带 Bearer 密钥：这个端点是鉴权保护的。没用对密钥会拿到 401，
// 而 401 与"网络不通"必须区分开 —— 前者说明节点活着、只是密钥过期，
// 后者的处理动作完全不同（前者改密钥，后者去查机器）。
func probeStatusViaAPI(ctx context.Context, apiURL, apiKey string, timeout time.Duration) nodeAPIProbe {
	return probeStatusViaAPIWithDial(ctx, apiURL, apiKey, timeout, safeDialContext(5*time.Second))
}

// probeStatusViaAPIWithDial 是带可注入拨号器的实现。
//
// 为什么把拨号器做成参数（而不是直接调 safeDialContext）：
// 生产用的拨号器**刻意拒绝一切非公网地址**（那是必需的 SSRF 防护），
// 而 httptest 起的测试服务器就在 127.0.0.1 —— 于是所有需要真实 HTTP
// 往返的测试都拿不到响应，测不了状态解析。
//
// 第一版我直接调用 safeDialContext，结果 4 个测试全挂在
// "127.0.0.1 解析到非公网地址，已拒绝访问" 上。那是**测试设计问题**，
// 不是防护本身有问题（另有一条测试专门验证了它真的拦住了连接，
// 连监听端都收不到任何连接）。
//
// 抽成参数后：生产路径永远传 safeDialContext（见上面的包装函数），
// 测试路径传一个不做地址限制的。这样既测得到解析逻辑，
// 又不必为了"好测"而把生产防护拆掉 —— 后者是绝不能做的取舍。
func probeStatusViaAPIWithDial(ctx context.Context, apiURL, apiKey string, timeout time.Duration, dial func(ctx context.Context, network, addr string) (net.Conn, error)) nodeAPIProbe {
	if strings.TrimSpace(apiURL) == "" {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: "节点未绑定 API 地址"}
	}
	if strings.TrimSpace(apiKey) == "" {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: "节点未绑定 API 密钥"}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// 归一成**完整端点**再请求。
	// 不要在这里拼后缀 —— 那正是第一版 404 的成因（见函数注释）。
	endpoint, err := metaEndpointFromAPIURL(apiURL)
	if err != nil {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: err.Error()}
	}

	client := &http.Client{
		Timeout: timeout,
		// 拒绝重定向：节点门户不该重定向。跟随重定向会让"我连的是 A"
		// 变成"我实际连的是 B"，那正是 SSRF 的经典利用方式。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("节点 API 不接受重定向")
		},
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext:       dial,
			// 本 client 每次探测都新建一个 Transport（见函数上方注释的隔离理由），
			// 若开启 keep-alive，每个 Transport 都会各自攒一条 ESTAB 连接且永不回收：
			// 实测 60 秒涨 4 条，约 5760 条/天，最终打满进程 fd 上限导致探测全部失败。
			// 探测是低频短请求，复用连接没有收益，直接关掉 keep-alive 让连接用完即关。
			// 与 speedtest/portal.go:56 的既有写法保持一致。
			DisableKeepAlives: true,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "hy2-ops-console-collector")

	// 2026-11-04：与其它节点 API 调用一致，套上 PORTAL_TLS_PINS 的证书指纹。原来这里只认系统 CA：
	// 自签证书、靠指纹登记的节点，开户 / 用量 / 测速都正常，唯独状态探测每轮 TLS 校验失败，
	// 被判成「节点连不上（unreachable）」—— 告警、自动下线随之触发，而节点其实好好的。
	pinned, err := portaltls.Client(client, endpoint)
	if err != nil {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: err.Error()}
	}
	client = pinned
	// 每次探测均使用独立 Transport（含 TLS 指纹克隆分支），即使协商为 HTTP/2 也必须在退出时强制回收空闲连接，
	// 防止后台 HTTP/2 读循环协程与底层文件描述符持续泄漏。
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		// 网络层错误统一归到 unreachable。这里**不再细分**
		// （超时/DNS/拒绝连接对运维的含义都是"去查这台机器"）。
		return nodeAPIProbe{ErrorKind: "unreachable", Detail: shortErr(err)}
	}
	defer resp.Body.Close()

	// 响应体上限：node/meta 只有几个标量字段，正常不到 1KB。
	// 设上限是防"对端返回一个巨大的响应把面板内存吃满"——
	// 那是外部输入，不能假设它守规矩。
	const maxProbeResponseBytes = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
	if err != nil {
		return nodeAPIProbe{ErrorKind: "unreachable", Detail: shortErr(err)}
	}
	if len(body) > maxProbeResponseBytes {
		return nodeAPIProbe{ErrorKind: "invalid", Detail: "节点响应过大（超过 64 KiB），已拒绝解析"}
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nodeAPIProbe{
			ErrorKind: "unauthorized",
			Detail:    fmt.Sprintf("鉴权失败（HTTP %d），节点门户的 API 密钥可能已更换", resp.StatusCode),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nodeAPIProbe{
			ErrorKind: "unreachable",
			Detail:    fmt.Sprintf("节点 API 返回 HTTP %d", resp.StatusCode),
		}
	}

	// 节点门户的响应信封是 {ok, error, time, meta:{...}, users_count}。
	var payload struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Time  int64  `json:"time"`
		Meta  struct {
			ListenPort int `json:"listen_port"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nodeAPIProbe{
			ErrorKind: "invalid",
			Detail:    "响应不是合法 JSON：确认这个地址是节点门户而不是别的服务",
		}
	}
	// 信封的 ok=false 表示"请求到了、但节点说这次调用有问题"。
	// 这种情况**节点是活的**（能应答就证明活着），所以仍算探测成功，
	// 只是把节点的话带出来 —— 把它判成 offline 是错的。
	if !payload.OK {
		msg := payload.Error
		if msg == "" {
			msg = "节点返回 ok=false"
		}
		return nodeAPIProbe{OK: true, Detail: msg, NodeTime: payload.Time, ListenPort: payload.Meta.ListenPort}
	}

	return nodeAPIProbe{
		OK:         true,
		NodeTime:   payload.Time,
		ListenPort: payload.Meta.ListenPort,
	}
}

// safeDialContext 返回一个只允许连公网地址的拨号器。
//
// 与 httpapi.portalClient 的语义完全一致（那份在 httpapi 包里，
// 这里因包依赖方向无法复用，理由见文件头注释）。
// 三条硬规则：
//  1. 解析出的**每一个** IP 都必须是公网地址（只看第一个是漏洞：
//     攻击者可以让域名同时解析到公网与内网，先过检查、再连内网）
//  2. 实际连接用解析后的 IP（避免 TOCTOU：检查与连接之间的二次解析）
//  3. 拒绝链路本地地址（云元数据端点 169.254.169.254 就在这里）
func safeDialContext(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
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
			if !portaltls.PublicIP(ip.IP) {
				return nil, fmt.Errorf("%s 解析到非公网地址，已拒绝访问", host)
			}
		}
		// 全部地址通过公网安全校验后，按 IPv4 优先依次尝试拨号，避免双栈域名首个 IPv6 不可达导致假离线。
		return portaltls.DialCheckedIPs(ctx, dialer, network, port, ips)
	}
}

// shortErr 把底层错误压成一句话，去掉冗长的 URL 包装。
//
// 原始错误形如：
//
//	Get "https://node1.example.com:29421/api/v1/node/meta": dial tcp ...: i/o timeout
//
// 对日志与界面来说，中间那段 URL 是噪音（地址就写在相邻字段里）。
func shortErr(err error) string {
	msg := err.Error()
	if idx := strings.LastIndex(msg, ": "); idx > 0 && idx < len(msg)-2 {
		// 只取最后一段（真正的根因）。
		return msg[idx+2:]
	}
	return msg
}

// metaEndpointFromAPIURL 把库里的 api_url 归一成**完整的 node/meta 端点**。
//
// ⚠️ 这里踩过一个真机才暴露的 bug，完整记下来：
//
// 第一版叫 normalizeAPIBase，返回"base"，然后拼 `base + "/node/meta"`。
// 注释里我还写了一句「前者直接拼 /node/meta」—— **那句话本身就是错的**。
// 节点门户的 API 前缀是 `/api/v1/`，正确端点是 `/api/v1/node/meta`。
// 于是对 `https://node1.example.com:29421` 这种（不带路径的）api_url，
// 我拼出了 `https://node1.example.com:29421/node/meta` → **HTTP 404**。
//
// 生产实测（2026-10-02）：
//
//	node1.example.com:29421 容器内端口通、宿主机也通
//	但探测日志是 "节点 API 返回 HTTP 404"
//	—— 端口通却 404，就是路径拼错的典型症状。
//
// 教训：拼 URL 时不要"返回 base 再拼后缀"，而是**直接返回最终端点**。
// 前者把"前缀是什么"这件事散落在两处（拼接方 + 调用方），
// 一旦有一处想错就是静默的 404 —— 而 404 看起来像"对方没这个接口"，
// 很容易被误判成"节点版本太旧"。
//
// 现在支持的输入形态（都返回完整的 .../api/v1/node/meta）：
//
//	https://host:port
//	https://host:port/
//	https://host:port/api/v1/node/meta
//	https://host:port/<token>/api/v1/node/meta
//	https://host:port/<token>              ← 带 token 但没写全路径
func metaEndpointFromAPIURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("空的 API 地址")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("API 地址无法解析: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("API 地址必须是 http/https")
	}
	if parsed.Host == "" {
		return "", errors.New("API 地址缺少主机名")
	}

	path := strings.TrimSuffix(parsed.Path, "/")

	// 已经带完整路径：原样用。
	if strings.HasSuffix(path, "/api/v1/node/meta") {
		return parsed.Scheme + "://" + parsed.Host + path, nil
	}

	// 带 token 路径段但不完整（形如 /<64位hex>）：补上 /api/v1/node/meta。
	//
	// 为什么留着 token 段：带 token 的路径能同时使用节点的**网页会话**能力
	// （比 Bearer 多一批端点，比如 proxy-services）。丢掉它会让
	// 面板在那些能力上退化成"未绑定"。而 node/meta 本身两侧都认，
	// 所以保留 token 段对状态探测没有任何坏处。
	if segments := strings.Split(strings.TrimPrefix(path, "/"), "/"); len(segments) == 1 &&
		portalTokenPattern.MatchString(segments[0]) {
		return parsed.Scheme + "://" + parsed.Host + "/" + segments[0] + "/api/v1/node/meta", nil
	}

	// 裸 host:port（或带无关路径）：拼标准的 /api/v1/node/meta。
	return parsed.Scheme + "://" + parsed.Host + "/api/v1/node/meta", nil
}

// portalTokenPattern 匹配节点门户的 {token} 路径段。
//
// 与 httpapi 包里的同名模式保持一致：portal 用 secrets.token_hex(32)
// 生成（64 位小写 hex），但老版本用过 token_hex(16)（32 位），
// 所以两者都接受。
var portalTokenPattern = regexp.MustCompile(`^[a-f0-9]{16,128}$`)
