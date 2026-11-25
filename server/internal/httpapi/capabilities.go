package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// 节点能力聚合（2026-10-04 加）。
//
// 背景：主面板此前看不到「这台节点有什么能力」——
// portal 侧的 /reality-status、/bbr-status 等全部走**网页会话**鉴权，
// Bearer api_key 够不到，面板只能显示"未知"。
// 节点侧新增了 /api/v1/capabilities（Bearer 通道，只报能力不报凭据），
// 这里负责聚合，让前端一眼看到全部节点的协议/扩展/版本。
//
// ⚠️ 兼容性：capabilities 是**新端点**，老节点（PORTAL_VERSION < 2.2）
// 上没有它，会返回 404。这不是错误 —— 是「该节点门户偏旧」的正常表现，
// 前端据此显示"需要升级节点"而不是报红。

// nodeCapabilities 是单个节点的能力快照。
type nodeCapabilities struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	// Available=false indicates the configured API key could not be decrypted locally.
	Available bool `json:"available"`
	// Reachable=false 表示这一台没取到（网络/密钥/端点缺失）。
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	// Supported=false 表示端点不存在（老门户），与"取不到"区分开 ——
	// 前者要升级节点，后者要查网络，处理动作完全不同。
	Supported bool `json:"supported"`
	// PortalVersion 来自节点门户自己的 PORTAL_VERSION 常量。
	PortalVersion string         `json:"portal_version,omitempty"`
	Protocols     map[string]any `json:"protocols,omitempty"`
	Extras        map[string]any `json:"extras,omitempty"`
	Node          map[string]any `json:"node,omitempty"`
}

// nodeCapabilitiesResult 是一次聚合的完整结果。
type nodeCapabilitiesResult struct {
	Nodes     []nodeCapabilities `json:"nodes"`
	Fetched   int                `json:"fetched"`
	Reachable int                `json:"reachable"`
	Outdated  int                `json:"outdated"`
}

// capabilitiesNodeTimeout 是**单台节点**能力聚合的预算。
//
// 一条节点最坏要走 capabilities + reality-status + vlessenc-status 三次往返，
// 其中 vlessenc-status 在节点侧会现跑 qrencode（上游 timeout=3s）。
// 沿用「建账号」的 provisionTimeout(15s) 会让最后一段被截断，
// 表现成「VLESS Encryption 不可交付」的假告警。
const capabilitiesNodeTimeout = 25 * time.Second

// capabilitiesMaxNodesPerFetch 是聚合时假设的节点数上限。
//
// 生产实测 13 台；这里给到 64（= 16 批 × 4 并发）足够覆盖增长，
// 同时把总预算封顶在一个可接受的范围（见 capabilitiesTotalBudget）。
const capabilitiesMaxNodesPerFetch = 64

// capabilitiesTotalBudget 是整批能力聚合的总预算：
// 批次数 × 单节点预算 + 一轮余量。
//
// 2×provisionTimeout(30s) 的旧值只够两台慢节点，节点一多就截断。
func capabilitiesTotalBudget() time.Duration {
	batches := (capabilitiesMaxNodesPerFetch + provisioningConcurrency - 1) / provisioningConcurrency
	return time.Duration(batches+1) * capabilitiesNodeTimeout
}

// protocolSections 只保留「值是对象」的协议段，丢弃形状不对的键。
//
// 节点侧的能力段是 map[string]object；但**未来协议**完全可能用布尔或字符串
// 表示状态（例如 {"future_protocol": true}）。直接解成 map[string]map[string]any
// 会让一个不认识的键拖垮整包解析，已知协议一起消失 —— 这里逐个过滤，
// 未知形状的键被忽略即可，不影响已知协议。
func protocolSections(raw map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(raw))
	for key, value := range raw {
		if section, ok := value.(map[string]any); ok {
			out[key] = section
		}
	}
	return out
}

// isPortalEndpointMissing 判断错误是否表示「节点门户没有这个端点」。
//
// 与「节点连不上 / 密钥不对」严格区分：前者要升级节点门户，
// 后者要查网络或换密钥。混为一谈会让运维照错误的方向排查。
//
// 匹配的是 **portal 自己的措辞**（见 portal.py 的
// `return self.reply_json(404, {'ok': False, 'error': 'API endpoint not found'})`），
// 而不是 HTTP 状态码 —— portalCall 会把 body 里的 error 文案透传上来，
// 状态码在那条路径上已经丢失。
//
// 状态码形态要**按整词**匹配，不能用裸子串。
//
// ⚠️ 2026-10-25 五轮排查第三轮修正：原来写 `strings.Contains(m, "404")`，
// 于是任何含 404 的端口号都会命中 —— 实测
//
//	连接节点 API 失败: dial tcp 1.2.3.4:40401: connect: connection refused
//
// 被判成「门户过旧，去升级」，而真实原因是节点连不上。处理动作完全相反，
// 这是最坏的一类误诊：运维会去点升级，然后升级也失败。
//
// 现在只认 ` 404` / `404 ` 这类独立词形态（"节点 API 返回 404"、
// "HTTP 404"），端口号 `:40401`、`14040` 不再命中。
func isPortalEndpointMissing(msg string) bool {
	m := strings.ToLower(msg)
	if strings.Contains(m, "endpoint not found") {
		return true
	}
	return hasStandaloneToken(m, "404")
}

// hasStandaloneToken 判断 token 是否作为独立词出现。
//
// 规则按「它可能是端口号的一部分」来定：
//   - 前面紧邻数字 / ':' / '.' ⇒ 是 `host:40401` 或 `1.2.3.404` 的一部分，拒绝
//   - 后面紧邻数字 / '.'      ⇒ 是 `40401` / `404.1` 的一部分，拒绝
//   - 后面是 ':' 则**允许**：`返回 404: {"ok":false}` 是真实的状态码写法
func hasStandaloneToken(msg, token string) bool {
	for i := 0; i+len(token) <= len(msg); i++ {
		if msg[i:i+len(token)] != token {
			continue
		}
		if i > 0 && !isLeftBoundary(msg[i-1]) {
			continue
		}
		if i+len(token) < len(msg) && !isRightBoundary(msg[i+len(token)]) {
			continue
		}
		return true
	}
	return false
}

func isLeftBoundary(c byte) bool {
	if c >= '0' && c <= '9' {
		return false
	}
	return c != ':' && c != '.' && c != '/'
}

func isRightBoundary(c byte) bool {
	if c >= '0' && c <= '9' {
		return false
	}
	return c != '.'
}

// loadNodeCapabilities 并发拉取所有在役节点的能力。
//
// 只在**节点绑了 api_key** 时才调 —— 没绑的连 node/meta 都读不了，
// 不必多打一次必然失败的请求。
func (s *Server) loadNodeCapabilities(ctx context.Context) (nodeCapabilitiesResult, error) {
	type row struct {
		id      string
		name    string
		baseURL string
		apiKey  string
	}
	var nodes []row
	unavailable := make([]nodeCapabilities, 0)
	rows, err := s.db.Query(ctx, `
SELECT id, name, COALESCE(api_url,''), api_key
FROM hysteria_nodes
WHERE deleted_at IS NULL AND COALESCE(api_url,'') <> '' AND api_key IS NOT NULL
ORDER BY created_at`)
	if err != nil {
		s.logger.Warn("capabilities: 读节点列表失败", "error", err)
		return nodeCapabilitiesResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		var cipher []byte
		if err := rows.Scan(&r.id, &r.name, &r.baseURL, &cipher); err != nil {
			rows.Close()
			s.logger.Warn("capabilities: 节点列表读取不完整", "error", err)
			return nodeCapabilitiesResult{}, err
		}
		if s.box == nil {
			unavailable = append(unavailable, nodeCapabilities{NodeID: r.id, NodeName: r.name, Available: false, Error: "服务端未配置凭据解密器"})
			continue
		}
		plain, err := s.box.Open(cipher)
		if err != nil {
			unavailable = append(unavailable, nodeCapabilities{NodeID: r.id, NodeName: r.name, Available: false, Error: "节点 API 密钥解密失败"})
			s.logger.Warn("capabilities: 节点 API 凭据解密失败", "node_id", r.id, "error", err)
			continue
		}
		r.apiKey = string(plain)
		nodes = append(nodes, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.logger.Warn("capabilities: 节点列表读取不完整", "error", err)
		return nodeCapabilitiesResult{}, err
	}

	out := make([]nodeCapabilities, len(nodes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, provisioningConcurrency)
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n row) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// ⚠️ 2026-10-25 五轮排查第四轮：单节点预算按「三段串行 + 一次
			// qrencode」给足。
			//
			// provisionTimeout 是给「建账号」这种单次写操作定的 15s；
			// 而这里一台节点最坏要走 capabilities + reality-status +
			// vlessenc-status（后者节点侧现跑 qrencode，上游 timeout=3s）。
			// 沿用 15s 会让最后一程被截断，表现成「不可交付」的假告警。
			callCtx, cancel := context.WithTimeout(ctx, capabilitiesNodeTimeout)
			defer cancel()

			item := nodeCapabilities{NodeID: n.id, NodeName: n.name, Available: true}
			envelope, err := portalCall(callCtx, n.baseURL, n.apiKey,
				http.MethodGet, "/capabilities", nil)
			if err != nil {
				// 端点不存在 ⇒ 老门户，与"网络不通"分开报。
				//
				// ⚠️ 判据的坑（2026-10-04 真机实测踩到）：
				// 第一版按 `strings.Contains(msg, "404")` 判断，**永远不命中** ——
				// 因为老 portal 对未知端点返回的是
				//   {"ok":false,"error":"API endpoint not found"}（HTTP 200 外壳）
				// 而 portalCall 把它包装成 "节点 API 报错: API endpoint not found"，
				// **消息里根本没有状态码**。
				// 结果 4 台老节点全被显示成"能力读取失败"（红色，像故障），
				// 而它们其实只是门户旧了 —— 处理动作完全不同（升级 vs 查网络）。
				//
				// 现在按**门户的实际措辞**匹配，并保留 "404" 兜底
				// （换实现/换版本时状态码可能又出现）。
				msg := err.Error()
				item.Reachable = false
				// 2026-11-03：Supported 必须在这里定下来。原来出错时一律保持 false，
				// 前端按 supported 选文案 ⇒ 连接被拒/超时的节点也显示「门户待升级」，
				// Outdated 计数也把它们算进去 —— 处理动作被指向升级而不是查网络。
				item.Supported = !isPortalEndpointMissing(msg)
				if !item.Supported {
					item.Error = "节点门户版本过旧（无 capabilities 端点），到节点 Web 面板点「更新」即可"
				} else {
					item.Error = truncateRunes(msg, 160)
				}
				out[i] = item
				return
			}
			var payload struct {
				OK            bool           `json:"ok"`
				PortalVersion string         `json:"portal_version"`
				Protocols     map[string]any `json:"protocols"`
				Extras        map[string]any `json:"extras"`
				Node          map[string]any `json:"node"`
			}
			if err := json.Unmarshal(envelope.Data, &payload); err != nil {
				item.Error = "节点返回的能力数据无法解析"
				// 端点存在、只是内容解析不了：不是「门户旧了」。
				item.Supported = true
				out[i] = item
				return
			}
			item.Reachable = true
			item.Supported = true
			item.PortalVersion = payload.PortalVersion
			item.Protocols = payload.Protocols

			// ⚠️ 2026-10-25 五轮排查第二轮：Reality 与 VLESS Encryption 的
			// Basic 状态读取必须并发，且只加载一次网页凭据。
			//
			// 原来两段各自 loadPortalWebTarget（每台节点多一次数据库往返），
			// 且串行发两次请求，共用一个 15s 的 callCtx —— 而节点侧
			// vlessenc-status 会现跑 qrencode（上游 timeout=3s）。
			// 慢节点上第二段必然被 ctx 截断，表现成「不可交付」的假告警。
			sections := protocolSections(payload.Protocols)
			reality, hasReality := sections["vless_reality"]
			enc, hasEnc := sections["vlessenc"]
			_, modern := sections["vmess"]

			var target *portalWebTarget
			var targetErr error
			if (modern && hasReality) || hasEnc {
				// ⚠️ 2026-10-25 五轮排查第三轮：不要用 `_` 吞掉凭据加载错误。
				// 「没绑凭据」和「凭据解密失败 / 数据库出错」是两种完全不同的
				// 问题（前者去设置页补，后者要查服务端密钥），而原来都表现为
				// target=nil ⇒ 统一报「未绑定 portal 网页凭据」，把人引错方向。
				target, targetErr = s.loadPortalWebTarget(callCtx, n.id)
			}
			// 两路 Basic 读取并发：它们打的是**不同主机**（provisioningConcurrency
			// 限的是节点数），同一台节点上最多 2 路，仍在
			// nodeReadConcurrency=4 的允许范围内，不会把单线程 portal 打崩。
			var readWG sync.WaitGroup
			if modern && hasReality {
				readWG.Add(1)
				go func() {
					defer readWG.Done()
					verifyRealityCapability(callCtx, reality, target)
				}()
			}
			// VLESS Encryption 的客户端密钥不在 capabilities 里，只出现在
			// Basic 会话的 vlessenc-status。这里补一个「能否交付」的结论，
			// 让节点页能区分「通道已启用」与「买家实际拿得到参数」。
			if hasEnc {
				readWG.Add(1)
				go func() {
					defer readWG.Done()
					applyVLESSEncryptionCapabilityWithTarget(callCtx, enc, target, targetErr)
				}()
			}
			readWG.Wait()
			item.Extras = payload.Extras
			item.Node = payload.Node
			item.Available = true
			out[i] = item
		}(i, n)
	}
	wg.Wait()

	all := append(unavailable, out...)
	res := nodeCapabilitiesResult{Nodes: all, Fetched: len(all)}
	for _, it := range all {
		if it.Reachable {
			res.Reachable++
		} else if it.Available && !it.Supported {
			res.Outdated++
		}
	}
	return res, nil
}

// GET /api/nodes/capabilities —— 所有节点的能力总览。
//
// ⚠️ 路由顺序：必须注册在 `GET /api/nodes/{id}` **之前**，
// 否则 Go ServeMux 的 {id} 占位符会把 "capabilities" 当成某个节点 id
// （2026-10-03 在 /api/sub-ids 上踩过同类问题）。
func (s *Server) nodeCapabilities(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	result, err := s.cachedNodeCapabilities(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// 能力要逐台实时问节点，最慢的那台决定整个接口的耗时（2026-10-14 实测 ~10 秒，
// 节点门户的 capabilities 会现场跑一串 systemctl 与证书检查）。节点页每次打开
// 都等这 10 秒没有意义 —— 协议/版本/扩展是慢变量。缓存 60 秒，并让同时到达的
// 请求共用同一次拉取；?refresh=1 跳过缓存。
const nodeCapabilitiesTTL = 60 * time.Second

type nodeCapsFlight struct {
	done chan struct{}
	res  nodeCapabilitiesResult
	err  error
}

var nodeCapsCache struct {
	sync.Mutex
	result    nodeCapabilitiesResult
	fetchedAt time.Time
	gen       uint64
	inflight  *nodeCapsFlight
	lastErr   error
}

// invalidateNodeCapsCache 在节点增删改绑或协议开关变更后立即作废能力总览缓存，
// 并递增版本号防止在途旧拉取回写过期数据。
func invalidateNodeCapsCache(_ string) {
	c := &nodeCapsCache
	c.Lock()
	c.fetchedAt = time.Time{}
	c.gen++
	c.Unlock()
}

func (s *Server) cachedNodeCapabilities(ctx context.Context, force bool) (nodeCapabilitiesResult, error) {
	c := &nodeCapsCache
	c.Lock()
	if !force && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < nodeCapabilitiesTTL {
		// TTL 内只回缓存的成功结果；失败只回给发起（和等待）那次拉取的请求。
		res := c.result
		c.Unlock()
		return res, nil
	}
	if flight := c.inflight; flight != nil {
		c.Unlock()
		select {
		case <-flight.done:
			res, fetchErr := flight.res, flight.err
			return res, fetchErr
		case <-ctx.Done():
			return nodeCapabilitiesResult{}, ctx.Err()
		}
	}
	flight := &nodeCapsFlight{done: make(chan struct{})}
	startGen := c.gen
	c.inflight = flight
	c.Unlock()

	// 实际拉取放到独立协程并加 defer/recover：
	// 1. 任何 panic 都会保证清空 inflight 并 close(done)，绝不永久死锁；
	// 2. 结果与错误绑定在本次 flight 实例上，防止缓存失效或下一次并发拉取篡改等待者读到的结果。
	go func() {
		var finalRes nodeCapabilitiesResult
		var finalErr error
		defer func() {
			fetchErr := finalErr
			if r := recover(); r != nil {
				if s.logger != nil {
					s.logger.Error("capabilities: 拉取节点能力协程发生异常", "panic", r, "stack", string(debug.Stack()))
				}
				fetchErr = fmt.Errorf("读取节点能力时发生内部异常: %v", r)
			}
			flight.res = finalRes
			flight.err = fetchErr
			c.Lock()
			if fetchErr == nil {
				if c.gen == startGen {
					c.result, c.fetchedAt = finalRes, time.Now()
				}
				c.lastErr = nil
			} else {
				c.lastErr = fetchErr
			}
			if c.inflight == flight {
				c.inflight = nil
			}
			c.Unlock()
			close(flight.done)
		}()
		// 用独立的超时上下文：发起者断开不应让共享这次拉取的其它请求一起失败。
		fetchCtx, cancel := context.WithTimeout(context.Background(), capabilitiesTotalBudget())
		res, fetchErr := s.loadNodeCapabilities(fetchCtx)
		cancel()
		finalRes, finalErr = res, fetchErr
	}()

	select {
	case <-flight.done:
		res, fetchErr := flight.res, flight.err
		return res, fetchErr
	case <-ctx.Done():
		return nodeCapabilitiesResult{}, ctx.Err()
	}
}
