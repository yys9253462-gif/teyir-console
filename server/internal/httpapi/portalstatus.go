package httpapi

// portalstatus.go — 通过 portal 的网页会话（Basic）读取节点运行状态。
//
// 这补上了 portaloverview.go 里那段「unreachable」说明所指向的空白。
// 那份文件的注释写道：
//
//	check-version / reality-status / bbr-status / warp-status / traffic-speed are
//	NOT under /api/v1/ — they live behind the portal's *web session* prefix
//	(/{token}/…), authenticated by the admin session or Basic auth, and are
//	therefore unreachable with the Bearer key alone.
//
// 前半句是对的，后半句在当时也是对的 —— 因为面板只存了 Bearer 密钥。
// 现在 Basic 凭据也入库了（迁移 013），这些端点全部可达。注释里说的
// 「An earlier revision probed those paths anyway; every probe 404ed」之所以
// 404，不是因为路径不在，而是因为当时用的是 Bearer 头：portal 的 do_GET 在
// /{token}/ 分支里对未鉴权请求回的是 401，而 /api/v1/ 分支对未知子路径回 404。
//
// 九个只读端点全部并发拉取，任何一路失败都不影响其余八路 —— 网关节点的
// WARP 没装不该让版本信息也消失。
//
// 「并发」是受限的：每路探测都要先在 nodeReadGates 上取一个槽位
// （nodeReadConcurrency = 4），所以对同一个节点的在建连接有明确上界。
// 这不是洁癖 —— 节点侧的 portal 是单线程 Python http.server，九路并发
// 乘以两个标签页就是十八路，实测里那批 unexpected EOF 正是这么来的。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// portalStatusProbeCount 是 portal-status 的探测路数。
//
// ⚠️ 它与 probes 表的长度是**同一件事**，必须一起改：加端点时忘记调这里，
// 整批预算就会停留在旧路数上（见 portalStatusBudget 的推导）。
// 端到端测试 TestPortalStatusProbeCountMatchesTable 会钉住这一点。
const portalStatusProbeCount = 9

// portalStatusProbeTimeout 是**单路**探测的上限，比整批预算小得多。
//
// ⚠️ 2026-10-25 五轮排查第二轮：原来单路直接用整批的 15s 预算，
// 于是一个卡住的端点（reality-status 要现读 xray 配置、check-version 要
// 查远端版本）会把整批预算吃干，后面排队的探测全部变成 unknown ——
// 用户看到的是「其它状态也没了」，而不是「这一项慢」。
//
// 单路 8s 已经远大于正常往返（几十毫秒到一两秒）；超时的那一路单独报
// unknown，不影响其余各路。
const portalStatusProbeTimeout = 8 * time.Second

// portalStatusBudget 是整批探测的总预算：按「轮数 × 单路上限」推导，
// 再留一轮余量给排队抖动。
//
// 9 路 / 并发 4 ⇒ 3 轮 ⇒ 3×8s = 24s。路数再变时只需改
// portalStatusProbeCount，不会出现「加了路数、预算没变」的截断。
func portalStatusBudget(probes int) time.Duration {
	if probes < 1 {
		probes = 1
	}
	rounds := (probes + nodeReadConcurrency - 1) / nodeReadConcurrency
	return time.Duration(rounds+1) * portalStatusProbeTimeout
}

// portalStatusTimeout 保留为默认预算（供既有测试与调用方引用）。
var portalStatusTimeout = portalStatusBudget(portalStatusProbeCount)

// portalStatusResult 是单路探测的结果。ProbeState 沿用 NodeControlPanel
// 已有的渲染约定（healthy / warn / unknown），避免前端再长一套分支。
type portalStatusResult struct {
	State   string `json:"state"`             // healthy | warn | unknown | unsupported
	Error   string `json:"error,omitempty"`   // 该路失败的原因，成功时为空
	Payload any    `json:"payload,omitempty"` // 原始响应，交给前端按端点解释
}

type portalVersionInfo struct {
	CoreCurrent     string `json:"core_current"`
	CoreLatest      string `json:"core_latest"`
	CoreHasUpdate   bool   `json:"core_has_update"`
	PortalCurrent   string `json:"portal_current"`
	PortalLatest    string `json:"portal_latest"`
	PortalHasUpdate bool   `json:"portal_has_update"`
	AWGInstalled    bool   `json:"awg_installed"`
	AWGCurrent      string `json:"awg_current"`
	AWGLatest       string `json:"awg_latest"`
	AWGHasUpdate    bool   `json:"awg_has_update"`
}

type portalBBRInfo struct {
	Current    string `json:"current"`
	Qdisc      string `json:"qdisc"`
	Kernel     string `json:"kernel"`
	Configured string `json:"configured"`
	NeedReboot bool   `json:"need_reboot"`
}

// portalRealityInfo 是 reality-status 的**对外**响应结构。
//
// 2026-11-04：与 portalVMessInfo 同理，不再声明 uri / uuid / pub_key / short_id / qr_svg。
// 上游的 uri 与二维码是**管理员**的 VLESS-Reality 连接串，uuid / short_id 是管理员凭据；
// 这个结构体经 GET /api/nodes/{id}/portal-status 原样发给浏览器（viewer 也能调），
// 等于把一条可直接使用的管理员链接交给每个登录会话。前端只读 sni / port / flow。
type portalRealityInfo struct {
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	Config    struct {
		Flow string `json:"flow"`
		SNI  string `json:"sni"`
		Port int    `json:"port"`
	} `json:"config"`
}

// portalVMessInfo 是 vmess-status 的**对外**响应结构。
//
// ⚠️ 2026-10-25 五轮排查第二轮：不再声明 Config.URI / Config.UUID / QRSVG。
// 上游 vmess-status 的 uri 是**管理员**节点的 vmess:// 链接，uuid 是管理员
// UUID；这个结构体经 /api/nodes/{id}/portal-status 原样发给浏览器，
// 声明了就等于把管理员身份交给每个会话，而前端只用 domain / sni /
// ws_path / port 描述通道状态，从不读 uri。
// 服务端自己需要 URI 时走 portalVMessStatus（见下），绝不写进响应体。
type portalVMessInfo struct {
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	Config    struct {
		Domain string `json:"domain"`
		SNI    string `json:"sni"`
		WSPath string `json:"ws_path"`
		Port   int    `json:"port"`
	} `json:"config"`
}

// portalAnyTLSInfo 是 anytls-status 的只读子集。节点响应里的 config.uri 是管理员
// 自己的连接串，刻意不声明 —— 面板只需要知道「装了没、开了没、跑着没」。
type portalAnyTLSInfo struct {
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Active    bool   `json:"active"`
	Version   string `json:"version"`
}

// 注：服务端不需要管理员 VMess URI —— 订阅只从 capabilities 的公开参数构建，
// 所以这里不保留「内部完整响应」结构，免得有人顺手把它序列化出去。

// portalVLESSEncInfo 见 vlessenc.go —— Config.UUID 刻意不声明（不读管理员 UUID）。

type portalWarpInfo struct {
	Installed bool     `json:"installed"`
	Enabled   bool     `json:"enabled"`
	Connected bool     `json:"connected"`
	IP        string   `json:"ip"`
	Rules     []string `json:"rules"`
}

type portalAWGInfo struct {
	Installed bool   `json:"installed"`
	Active    bool   `json:"active"`
	Line      string `json:"line"`
	Port      string `json:"port"`
	Endpoint  string `json:"endpoint"`
	PeerCount int    `json:"peer_count"`
	Ctl       bool   `json:"ctl"`
	Peers     []struct {
		Name string `json:"name"`
	} `json:"peers"`
}

type portalGostInfo struct {
	GostInstalled bool   `json:"gost_installed"`
	GostActive    bool   `json:"gost_active"`
	Host          string `json:"host"`
	Services      []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Port int    `json:"port"`
	} `json:"services"`
}

// portalSpeedInfo 是 portal 自己维护的 10 秒滑动窗口。
//
// 这比采集器里 trafficStats 那条链路更好：trafficStats 的 ?clear=true 语义是
// 「取走并清零」，必须按固定周期轮询才能算准速率；而这里是 portal 直接给出的
// 瞬时速率，tx/rx 分开，不需要清任何计数器，也不需要在节点上额外开端口或
// 放宽 validTrafficEndpoint 只允许 127.0.0.1 的限制。
type portalSpeedInfo struct {
	NodeTx float64 `json:"node_tx"`
	NodeRx float64 `json:"node_rx"`
	Users  map[string]struct {
		Tx float64 `json:"tx"`
		Rx float64 `json:"rx"`
	} `json:"users"`
}

// GET /api/nodes/{id}/portal-status — 七路并发只读探测。
func (s *Server) portalNodeStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	target, err := s.loadPortalWebTarget(r.Context(), nodeID)
	if err != nil {
		// errPortalNoBasic 走 400，语义是「凭据没给」而非「服务端出错」。
		s.writePortalWebTargetError(w, err)
		return
	}

	type probe struct {
		path  string
		build func() any
	}
	probes := map[string]probe{
		"version": {"check-version", func() any { return &portalVersionInfo{} }},
		"bbr":     {"bbr-status", func() any { return &portalBBRInfo{} }},
		"reality": {"reality-status", func() any { return &portalRealityInfo{} }},
		"vmess":   {"vmess-status", func() any { return &portalVMessInfo{} }},
		// VLESS Encryption 与 VMess 同类：只读状态，凭 Basic 会话。
		// 老门户没有该端点 ⇒ probePortalEndpoint 返回 unsupported。
		"vlessenc": {"vlessenc-status", func() any { return &portalVLESSEncInfo{} }},
		// AnyTLS（2026-10-30）：同类只读状态；老门户没有该端点 ⇒ unsupported。
		"anytls": {"anytls-status", func() any { return &portalAnyTLSInfo{} }},
		"warp":   {"warp-status", func() any { return &portalWarpInfo{} }},
		"awg":    {"awg-state", func() any { return &portalAWGInfo{} }},
		"gost":   {"proxy-services", func() any { return &portalGostInfo{} }},
		"speed":  {"traffic-speed", func() any { return &portalSpeedInfo{} }},
	}

	// 预算按**实际路数**推导，而不是用固定常量：probes 表新增端点时，
	// 这里会自动跟着变长，不会出现「加了路数、预算没变」的截断。
	ctx, cancel := context.WithTimeout(r.Context(), portalStatusBudget(len(probes)))
	defer cancel()

	var (
		mu      sync.Mutex
		results = make(map[string]portalStatusResult, len(probes))
		wg      sync.WaitGroup
	)
	for key, p := range probes {
		wg.Add(1)
		go func(key string, p probe) {
			defer wg.Done()
			res := s.probePortalEndpoint(ctx, target, p.path, p.build)
			mu.Lock()
			results[key] = res
			mu.Unlock()
		}(key, p)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":    nodeID,
		"node_name":  target.NodeName,
		"origin":     target.Origin,
		"checked_at": time.Now().UTC().Format(time.RFC3339),
		"probes":     results,
	})
}

// probePortalEndpoint 跑一路探测，把成功/失败都归一成 portalStatusResult。
// 失败不返回 error，而是塞进 Error 字段 —— 单路失败是常态（没装 WARP 的节点
// 恰恰是通过这个接口才看得出来的），不该让整批请求失败。
func (s *Server) probePortalEndpoint(ctx context.Context, target *portalWebTarget, path string, build func() any) portalStatusResult {
	// 先过读闸门再拨号。九路探测在这里排队，任何时刻对同一个节点的在建
	// 连接数不超过 nodeReadConcurrency —— 否则两个标签页同时刷新就是
	// 十八路并发砸向一个单线程的 Python http.server。
	//
	// 闸门放在这一层而不是 portalNodeStatus 外面：那是「每个页面刷新限一次」，
	// 而这才是「每次拨号都受限」。前者挡不住两个并发的刷新请求，
	// 后者能。
	//
	// 单路超时在这里收紧：排队本身也计入这个预算，卡住的那一路不会
	// 吃掉整批（见 portalStatusProbeTimeout 的注释）。
	ctx, cancel := context.WithTimeout(ctx, portalStatusProbeTimeout)
	defer cancel()

	release, err := s.lockNodeRead(ctx, target.NodeID)
	if err != nil {
		return portalStatusResult{State: "unknown", Error: "节点门户繁忙，未能在超时前取得探测许可: " + err.Error()}
	}
	defer release()

	envelope, err := portalWebCall(ctx, target, http.MethodGet, path, nil)
	if err != nil {
		if isPortalEndpointMissing(err.Error()) {
			return portalStatusResult{State: "unsupported", Error: "节点门户尚未提供此端点，请升级节点门户"}
		}
		return portalStatusResult{State: "unknown", Error: err.Error()}
	}
	payload := build()
	if err := json.Unmarshal(envelope.Data, payload); err != nil {
		return portalStatusResult{State: "unknown", Error: "响应结构无法解析: " + err.Error()}
	}
	return portalStatusResult{State: "healthy", Payload: payload}
}

// writePortalTargetError 扩展了 nodeapi.go 里的版本，多认一个 errPortalNoBasic。
func (s *Server) writePortalWebTargetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPortalNoBasic):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      err.Error(),
			"need_basic": true,
		})
	default:
		s.writePortalTargetError(w, err)
	}
}
