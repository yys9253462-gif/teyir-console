package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// subscription.go builds the merged subscription: one URL that carries every
// registered node, so a client imports it once and gets the whole fleet.
//
// Credential model (chosen deliberately): each node contributes its own
// *master* auth_password, read live from that node's `/api/v1/node/meta`.
// Nothing new is stored, no node is written to, and every node that already has
// a bound portal API works immediately. Two consequences must not be papered
// over, and are surfaced in the UI rather than hidden:
//
//  1. Every client importing the link authenticates as the same identity on a
//     given node (the portal's base user). Per-user traffic accounting on that
//     node therefore merges across those clients.
//  2. The link itself is as sensitive as those master passwords. It is a bearer
//     capability with no login, so it is randomly generated, revocable in one
//     click, and rate-limited.
//
// Nodes without a bound portal API cannot be included: users/list deliberately
// omits passwords, so there is no other source for a usable credential.

const (
	subscriptionFormatClash   = "clash"
	subscriptionFormatSingBox = "singbox"
	subscriptionFormatURI     = "uri"
)

// subscriptionCacheTTL bounds how stale a resolved node list may be. Resolution
// costs one portal round trip per node; a subscription client refreshes every
// few hours, but the preview button is cheap to press, so the resolved list is
// cached briefly instead of re-probing the whole fleet per request.
const subscriptionCacheTTL = 60 * time.Second

// subscriptionStaleMax：缓存过期后、节点列表未变时，最多再用旧结果多久（期间后台刷新）。
const subscriptionStaleMax = 10 * time.Minute

// subscriptionResolveConcurrency caps simultaneous portal probes. A 40-node
// fleet fired all at once would open 40 sockets to 40 hosts; six at a time keeps
// the fan-out polite while still finishing far faster than a serial loop.
//
// ⚠️ 2026-10-24 审计 P1-3：从 6 提到 16。
//
// 原来的 6 是按「一节点一次请求」估的，但实际每个节点要发**两次**
// （/node/meta + /capabilities），而两次是串行的 —— 于是 11 个节点要跑
// 两轮、每轮含两次往返。实测冷缓存 5662~6304ms，正是「两轮 × 一次慢往返」。
//
// 16 的上限依据：实测最多 12 个节点，16 已能一轮打完；对节点侧来说
// 同一瞬间最多 16 个并发连接，仍远低于它们各自的 HTTP 连接上限
// （ctr 配置的默认 512），不构成压力。再多也没有收益 —— 节点总数就那么多。
const subscriptionResolveConcurrency = 16

// subscriptionNodeProbeTimeout 是**单个节点**的探测总时长上限。
//
// ⚠️ 2026-10-25 五轮排查第四轮：从 5s 调到 12s，与「探测不再串行」配套。
//
// 5s 是按「一节点两次请求」估的（/node/meta + /capabilities）。
// 现在现代节点还要再读 reality-status 与 vlessenc-status —— 后两者虽然已
// 改成并发（第二轮），但**第一步 capabilities 仍是前置**，所以最坏路径是
// capabilities(慢) + max(reality, vlessenc)。而节点侧 vlessenc-status
// 会现跑 qrencode（上游 timeout=3s）。
//
// 5s 下这些节点会稳定地拿不到备用通道，表现为「VLESS Encryption 时有时无」。
// 12s 给足「一次慢 capabilities + 一次 3s qrencode」，同时仍远小于
// 订阅接口的同步等待上限（缓存会在后台刷新，慢的只是冷启动那一次）。
const subscriptionNodeProbeTimeout = 12 * time.Second

// subscriptionNode is one node's material, already resolved into the fields a
// client config needs.
type subscriptionNode struct {
	ID        string
	Name      string
	Host      string
	Port      int
	Password  string
	SNI       string
	Insecure  bool
	PinSHA256 string
	ObfsPass  string
	HopRange  string
	InStore   bool

	// AccountUserID is the node-side account this node's password belongs to,
	// when the subscriber owns one (see provisioning.go). Empty means the config
	// is using the node's shared portal password. Never rendered into a client
	// config — it exists so logs and the operator UI can name the account.
	AccountUserID string

	// Reality 携带该节点的 VLESS-Reality 备用通道参数（2026-10-04 加）。
	//
	// 为什么需要：合并订阅此前**只下发 Hy2**，于是客户端在「UDP 被 QoS/阻断」
	// 的网络里没有任何兜底 —— 而节点侧其实早就为每个用户准备好了独立的
	// Reality 身份。缺口在控制台这一环：它拿不到节点的 pbk/sid/sni，
	// 也就拼不出 Reality 节点。
	//
	// 为零值即表示「该节点没有开启 Reality」，此时不输出额外节点 ——
	// 不是错误，只是该节点没有这个能力（例如未装 xray）。
	Reality *nodeReality
	VMess   *nodeVMess

	// VLESSEnc 是 VLESS Encryption 备用通道的客户端公开参数（2026-10-24 加）。
	//
	// 与 VMess 同样需要域名 + 可信证书，但带 ML-KEM-768 后量子密钥交换。
	// 它的客户端 encryption 参数**不在 capabilities 里**，必须通过 Basic
	// 会话读 vlessenc-status 才能拿到 —— 因此没有 Basic 凭据或端点缺失时
	// 该字段为 nil，订阅里不会出现这条通道（不可交付，不输出坏配置）。
	VLESSEnc *nodeVLESSEncryption
}

// nodeReality 是一台节点上 Reality 的**客户端公开参数**。
//
// 这些值来自节点侧 portal 的 /api/v1/capabilities（vless_reality 段）。
// 它们本来就会写进下发给客户端的订阅链接，属于公开值 ——
// **服务端私钥永远不在这里**，节点侧也从不通过 Bearer 通道输出它。
type nodeReality struct {
	// PublicKey 是 Reality 的公钥（客户端 pbk 参数）。
	PublicKey string
	// ShortID 是 short id（客户端 sid 参数）。
	ShortID string
	// DestSNI 是客户端握手的 SNI（也是对端伪装目标）。
	DestSNI string
	// Port 是 Reality 入站端口。注意它**通常与 Hy2 端口不同**
	// （例如 Hy2 与 Reality 使用不同端口），所以必须单独携带。
	Port int
}

// ready 判断参数是否齐全到足以生成客户端配置。
//
// 三要素缺一不可：pbk 用来校验服务端、sid 用来区分入站、sni 用来握手。
// 缺任一项拼出来的链接在客户端会直接连不上，且**报错信息毫无指向性**，
// 因此这里宁可不下发该节点，也不发一条注定失败的配置。
func (r *nodeReality) ready() bool {
	return r != nil && r.PublicKey != "" && r.ShortID != "" && r.DestSNI != "" &&
		r.Port >= 1 && r.Port <= 65535
}

// subscriptionExcluded records a node that could not be included, with the
// reason. The UI lists these so a missing node is never a silent mystery.
type subscriptionExcluded struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type subscriptionBuild struct {
	Nodes    []subscriptionNode
	Excluded []subscriptionExcluded
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

type subscriptionCache struct {
	mu       sync.Mutex
	key      string
	build    *subscriptionBuild
	storedAt time.Time
	// gen 每次 InvalidateSubscriptionCache 加一：失效前开始的后台刷新不得把旧结果写回。
	gen      uint64
	inflight map[subscriptionFlightKey]*subscriptionFlight
}

var subCache = &subscriptionCache{}

type subscriptionCandidate struct {
	id         string
	name       string
	apiURL     string
	ciphertext []byte
	domain     string
	listenPort int
	updatedAt  time.Time
	inStore    bool
	// hasWebCredential：绑了门户网页凭据（Basic 账号 + 路径令牌）。订阅里每个节点的客户端参数
	// 只能经网页会话读（user-config），没有它的节点一律给不出（2026-11-06）。
	hasWebCredential bool
}

// applySubscriptionAccounts stamps this subscription's OWN node-side credentials
// onto the shared node build, and decides which nodes this subscription may see.
//
// 业务规则（加入商城）：
// 对于买家订单订阅 (rec.OrderID != "")，严格只推送 InStore=true 的节点。
// 未加入商城 (in_store=false) 的节点仅限管理员自用，绝不下发给买家。
// 而**管理员自用订阅（order_id 为空）本来就应该能用全部节点** ——
// in_store=false 的语义是"不卖给买家"，不是"不可用"。
//
// ⚠️ 2026-10-03 修的真 BUG：
//
// **下发的节点必须持有该订阅的专属账号，否则客户端必然连不上。**
//
// 原判据只有 isBuyer/in_store，于是漏了最关键的一种情况：**账号没开成功**
// （或开了但被重建、密码变了）时照样把节点下发出去，而节点侧不认识这个客户端 →
// 直接 `authentication error, HTTP status code: 200`。
//
// 实测（2026-10-03）：
//   - 洛杉矶-NTT 出现在手工订阅配置里，而该节点上 sub_ 账号数 = 0，
//     且它用的是**节点主口令**（不是订阅专属密码）→ 必然失败；
//   - 哥伦布-AWS 的账号被重复创建换过密码，库里记的旧密码失效 → 认证失败。
//
// 所以完整判据是四件事同时成立：
//  1. 该节点有专属账号（user_id 非空）
//  2. 该账号有密码
//  3. 买家订阅还要求 in_store=true
//  4. 账号在节点侧真的存在（由 JIT 补齐保证；补齐失败的节点在此被过滤）
//
// 不满足就**不下发**：宁可少一个节点，也不要给客户端一个连不上的节点 ——
// 后者会让买家以为"整个订阅坏了"，前者只是"这个节点暂时不可用"。
//
// ⚠️ 2026-10-03 新增：**分类过滤**（第 5 条判据）
//
// 订阅可以指定一个分类（AI 专用 / 跨境专用…），此时只下发该分类下的节点。
// categoryNodeIDs 就是那个分类的节点 id 集合，由调用方查好传进来：
//   - nil  → 不按分类圈定（= 改分类功能之前的行为，全部分类）
//   - 非 nil → 只有在这个集合里的节点才下发
//
// 为什么把集合作为**参数**而不是在这里查库：
//
//	本函数是纯函数、被多处调用（下发、交付页、预览），
//	查库会让每次调用多一次往返，也很容易在某个调用点忘传而静默退化成"全给"。
//	传参能让"忘了传"变成编译期就能发现的类型问题（nil 有明确语义）。
//
// categoryScopeFor 解析一条订阅的分类圈定，返回给 applySubscriptionAccounts 用的集合。
//
// 语义（三态，别合并）：
//
//	rec == nil 或 rec.CategoryID == nil  → 返回 (nil, nil)
//	    不按分类圈定 = 全部分类 = 加分类功能之前的行为。
//	rec.CategoryID 指向一个存在的分类     → 返回 (该分类的节点集合, nil)
//	    即使集合为空也返回**空 map**（而不是 nil）——
//	    空集合意味着"这个分类下确实没有节点"，订阅应该拿到 0 个节点；
//	    返回 nil 会被下游理解成"不圈定"，于是把全部节点发出去 ——
//	    那是"配置错误导致多发"的最坏情况，必须避开。
//	分类 id 查不到（被删了）              → 返回 (空集合, error)
//	    同样返回空集合，宁可少发也不多发。
//
// 这个"空集合 ≠ nil"的区分是本函数唯一容易出错的地方，所以写成显式三态。
func (s *Server) categoryScopeFor(ctx context.Context, rec *subscriptionRecord) (map[string]bool, error) {
	// 直接圈定的节点优先（手工开通多选）；空集合不会出现在这里（len>0 才走）。
	if rec != nil && len(rec.NodeIDs) > 0 {
		scope := make(map[string]bool, len(rec.NodeIDs))
		for _, id := range rec.NodeIDs {
			scope[id] = true
		}
		return scope, nil
	}
	if rec == nil || rec.CategoryID == nil || strings.TrimSpace(*rec.CategoryID) == "" {
		return nil, nil
	}
	// ⚠️ 2026-10-30：分类被删掉时必须**区分**于「分类存在但下面没有节点」。
	//
	// 两种情况的正确反应完全不同：前者是配置坏了（需要人来改订阅），
	// 后者是正常的空范围（等节点上架即可）。而负载节点查询对不存在的
	// category_id 只会返回空集合、不报错，于是「分类被删」这条路
	// 静默地按空范围处理：订阅给买家 0 个节点，交付页 node_count=0，
	// 没有任何日志、没有任何提示 —— 与「管理员故意圈了个空分类」长得一模一样。
	// 这里补上存在性判断，让调用方至少能记一条可定位的告警。
	exists, err := s.categoryExists(ctx, *rec.CategoryID)
	if err != nil {
		return map[string]bool{}, subscriptionScopeUnavailable{err}
	}
	if !exists {
		return map[string]bool{}, fmt.Errorf("订阅引用的分类 %s 已不存在", *rec.CategoryID)
	}
	ids, err := s.loadCategoryNodeIDs(ctx, *rec.CategoryID)
	if err != nil {
		// 查不到就把范围收敛成空，绝不放行全部。
		return map[string]bool{}, subscriptionScopeUnavailable{err}
	}
	return ids, nil
}

// subscriptionScopeUnavailable：范围暂时解析不了（数据库错误），不是「范围就是空的」。
type subscriptionScopeUnavailable struct{ err error }

func (e subscriptionScopeUnavailable) Error() string {
	return "订阅范围暂时无法读取：" + e.err.Error()
}
func (e subscriptionScopeUnavailable) Unwrap() error { return e.err }

// errSubscriptionTemporarilyEmpty：本该有节点，却因为临时错误一个都给不出（2026-11-06）。
//
// 原来这两种情况都照常渲染并回 200：一份只有占位条目和 DIRECT 的「合法」配置。
// 客户端用它覆盖掉原来能用的配置，又因 profile-update-interval=12 最长 12 小时不再更新 ——
// 数据库抖一下，买家半天没有节点可用。回 503 时客户端保留旧配置，下次再拉。
var errSubscriptionTemporarilyEmpty = errors.New("订阅暂时无法生成，请稍后再试")

// subscriptionNodesFor 是**唯一**的「这条订阅能看到哪些节点」出口。
//
// 下发、交付页、预览全部走它 —— 上次交付页与实际下发不一致，
// 根因就是两条路径各写了一遍过滤。这里收敛成一处后，
// 以后加判据（比如这次的分类）只需要改这一个地方。
// excludedInScope 只保留落在这条订阅下发范围内、且已为它开号的「暂时下发不了」节点。
//
// 2026-11-03：交付页原来只看 node_accounts 里有没有映射。映射会保留已移出分类 / 已下架
// 节点的旧账号（回收要用），于是这些节点故障时，买家交付页上会出现一个 /sub 永远不会给他的
// 「暂时不可用」节点 —— 与下发内容不同源。这里套用与 subscriptionNodesFor 相同的两条规则：
// 分类 / 直接圈定的范围，以及买家订阅只给已上架节点。
func (s *Server) excludedInScope(ctx context.Context, excluded []subscriptionExcluded, rec *subscriptionRecord) []subscriptionExcluded {
	out, _ := s.excludedInScopeChecked(ctx, excluded, rec)
	return out
}

func (s *Server) excludedInScopeChecked(ctx context.Context, excluded []subscriptionExcluded, rec *subscriptionRecord) ([]subscriptionExcluded, error) {
	if len(excluded) == 0 || rec == nil {
		return nil, nil
	}
	scope, err := s.categoryScopeFor(ctx, rec)
	if err != nil {
		var unavailable subscriptionScopeUnavailable
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, nil
	}
	var listed map[string]bool
	if rec.OrderID != "" {
		listed = map[string]bool{}
		ids := make([]string, 0, len(excluded))
		for _, e := range excluded {
			ids = append(ids, e.NodeID)
		}
		rows, err := s.db.Query(ctx, `SELECT id::text FROM hysteria_nodes WHERE id::text = ANY($1) AND in_store`, ids)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			listed[id] = true
		}
		rows.Close()
		if rows.Err() != nil {
			return nil, rows.Err()
		}
	}
	out := make([]subscriptionExcluded, 0, len(excluded))
	for _, e := range excluded {
		if scope != nil && !scope[e.NodeID] {
			continue
		}
		if listed != nil && !listed[e.NodeID] {
			continue
		}
		if acc, ok := rec.nodeAccounts[e.NodeID]; !ok || acc.UserID == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// hasScopedExcludedNode 判断失败排除列表中是否存在落在该订阅范围内（买家要求已上架）的节点，
// 不要求该订阅已持有节点账号映射——用于全新订单在构建期节点集体掉线时准确回 503 而非空配置。
func (s *Server) hasScopedExcludedNode(ctx context.Context, excluded []subscriptionExcluded, rec *subscriptionRecord) (bool, error) {
	if len(excluded) == 0 || rec == nil {
		return false, nil
	}
	scope, err := s.categoryScopeFor(ctx, rec)
	if err != nil {
		var unavailable subscriptionScopeUnavailable
		if errors.As(err, &unavailable) {
			return false, err
		}
		return false, nil
	}
	var listed map[string]bool
	if rec.OrderID != "" {
		listed = map[string]bool{}
		ids := make([]string, 0, len(excluded))
		for _, e := range excluded {
			ids = append(ids, e.NodeID)
		}
		rows, err := s.db.Query(ctx, `SELECT id::text FROM hysteria_nodes WHERE id::text = ANY($1) AND in_store`, ids)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			listed[id] = true
		}
		rows.Close()
		if rows.Err() != nil {
			return false, rows.Err()
		}
	}
	for _, e := range excluded {
		if scope != nil && !scope[e.NodeID] {
			continue
		}
		if listed != nil && !listed[e.NodeID] {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (s *Server) subscriptionNodesFor(ctx context.Context, build []subscriptionNode, rec *subscriptionRecord) []subscriptionNode {
	nodes, _ := s.subscriptionNodesForChecked(ctx, build, rec)
	return nodes
}

// subscriptionNodesForChecked 与 subscriptionNodesFor 相同，另外报告「暂时给不出节点」：
// 范围读库失败，或范围内有候选节点却因读取错误全部被剔除。对外下发（/sub、交付页）要用它，
// 遇到 errSubscriptionTemporarilyEmpty 回 503 而不是一份空配置。
func (s *Server) subscriptionNodesForChecked(ctx context.Context, build []subscriptionNode, rec *subscriptionRecord, excluded ...[]subscriptionExcluded) ([]subscriptionNode, error) {
	scope, err := s.categoryScopeFor(ctx, rec)
	if err != nil {
		s.logger.Warn("subscription: 解析分类范围失败，按空范围处理",
			"subscription", rec.ID, "category", rec.CategoryID, "error", err)
		var unavailable subscriptionScopeUnavailable
		if errors.As(err, &unavailable) {
			return []subscriptionNode{}, errSubscriptionTemporarilyEmpty
		}
	}
	nodes := applySubscriptionAccounts(build, rec, scope)
	// 2026-11-06：范围里有节点、却一个专属账号都还没开好（开户挂起、即时补开户超时）= 暂时给不出，
	// 不是「这个订阅没有节点」。原来回 200 + 只有占位条目的配置：客户端覆盖掉原配置、最长 12 小时不再拉。
	if len(nodes) == 0 && len(missingAccountNodes(build, rec, scope)) > 0 {
		return nodes, errSubscriptionTemporarilyEmpty
	}
	hadAssignedNodes := len(nodes) > 0
	// 故障节点自动下线（见 healthgate.go）：端口不通或疑似被墙超过 5 分钟的节点不下发。
	nodes = s.filterHealthyNodes(ctx, nodes)
	if len(nodes) == 0 {
		if hadAssignedNodes {
			return nodes, errSubscriptionTemporarilyEmpty
		}
		for _, failures := range excluded {
			hasScoped, err := s.hasScopedExcludedNode(ctx, failures, rec)
			if err != nil || hasScoped {
				return nodes, errSubscriptionTemporarilyEmpty
			}
		}
		return nodes, nil
	}

	// The low-privilege /api/v1/node/meta endpoint intentionally redacts
	// obfs_password and pin_sha256. Fetch the subscriber's already-authorized
	// user-config through the stored Basic credential instead: it is the
	// canonical client profile and includes Salamander, certificate pin, and
	// port-hopping options without widening the Bearer API's privilege.
	type result struct {
		node   subscriptionNode
		ok     bool
		resync bool
	}
	results := make([]result, len(nodes))
	sem := make(chan struct{}, subscriptionResolveConcurrency)
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			configured, err := s.cachedSubscriptionClientOptions(ctx, nodes[i])
			if err != nil {
				s.logger.Warn("subscription: 节点客户端配置读取失败，已从订阅剔除",
					"node", nodes[i].Name, "error", err)
				// 2026-11-04：节点说「没有这个用户」= 映射还在、账号却没了（节点重装、在门户里被删）。
				// 判断「要不要补开户」只看映射，于是 JIT 与每小时补开户都不会碰它，这台节点就一直从
				// 这位买家的订阅里消失。排一次同步：同步会逐节点 GET → 不存在 → create_only 重新开户。
				// 2026-11-06：口令对不上同样要排同步（原来只认「用户不存在」，这台节点就永久从买家订阅里消失）。
				if isUserNotFound(err) || errors.Is(err, errSubscriptionAccountMismatch) {
					results[i].resync = true
				}
				return
			}
			results[i] = result{node: configured, ok: true}
		}(i)
	}
	wg.Wait()

	ready := make([]subscriptionNode, 0, len(nodes))
	needsSync := false
	for _, item := range results {
		needsSync = needsSync || item.resync
		if item.ok {
			ready = append(ready, item.node)
		}
	}
	if needsSync {
		if qerr := s.queueSubscriptionSyncBounded(ctx, rec.ID); qerr != nil {
			s.logger.Warn("subscription: 节点账号修复排队失败", "subscription", shortID(rec.ID), "error", qerr)
		}
	}
	if len(ready) == 0 {
		return ready, errSubscriptionTemporarilyEmpty
	}
	return ready, nil
}

// 每个买家在每个节点上的客户端参数（混淆、证书指纹、端口跳跃…）只在节点重装时才变，
// 原来每次拉订阅都对每个节点实时请求一次（11 个节点 = 11 次，速度取决于最慢的那个）。
// 现在按「节点 + 账号 + 密码」缓存：5 分钟内直接用；读取失败时，1 小时内的上次成功结果
// 仍然下发（原来是直接把节点从订阅里剔除，节点短暂抖一下买家就少一个节点）。
const (
	clientOptionsFresh = 5 * time.Minute
	clientOptionsStale = time.Hour
)

type clientOptionsEntry struct {
	node     subscriptionNode
	storedAt time.Time
}

var clientOptionsCache = struct {
	sync.Mutex
	m map[string]clientOptionsEntry
}{m: map[string]clientOptionsEntry{}}

func (s *Server) cachedSubscriptionClientOptions(ctx context.Context, node subscriptionNode) (subscriptionNode, error) {
	sum := sha256.Sum256([]byte(node.ID + "\x00" + node.AccountUserID + "\x00" + node.Password + "\x00" + node.Host + "\x00" + strconv.Itoa(node.Port)))
	key := hex.EncodeToString(sum[:])
	clientOptionsCache.Lock()
	entry, ok := clientOptionsCache.m[key]
	clientOptionsCache.Unlock()
	if ok && time.Since(entry.storedAt) < clientOptionsFresh {
		return withCachedClientOptions(node, entry.node), nil
	}
	fresh, err := s.fetchSubscriptionClientOptions(ctx, node)
	if err != nil {
		// 节点明确说「没有这个用户」或口令不匹配时主动清除已证伪的缓存，
		// 防止后续网络抖动把无效旧配置当作兜底继续下发。
		if isUserNotFound(err) || errors.Is(err, errSubscriptionAccountMismatch) {
			clientOptionsCache.Lock()
			delete(clientOptionsCache.m, key)
			clientOptionsCache.Unlock()
			return fresh, err
		}
		if ok && time.Since(entry.storedAt) < clientOptionsStale {
			return withCachedClientOptions(node, entry.node), nil
		}
		return fresh, err
	}
	clientOptionsCache.Lock()
	now := time.Now()
	if len(clientOptionsCache.m) > 20000 {
		// 先清理过期条目，避免整表清空引发缓存雪崩。
		for k, v := range clientOptionsCache.m {
			if now.Sub(v.storedAt) >= clientOptionsStale {
				delete(clientOptionsCache.m, k)
			}
		}
		// 全部都未过期时按写入时间淘汰最旧的一半条目。
		if len(clientOptionsCache.m) > 20000 {
			type agedKey struct {
				k string
				t time.Time
			}
			aged := make([]agedKey, 0, len(clientOptionsCache.m))
			for k, v := range clientOptionsCache.m {
				aged = append(aged, agedKey{k: k, t: v.storedAt})
			}
			sort.Slice(aged, func(i, j int) bool { return aged[i].t.Before(aged[j].t) })
			evictCount := len(aged) / 2
			for i := 0; i < evictCount; i++ {
				delete(clientOptionsCache.m, aged[i].k)
			}
		}
	}
	clientOptionsCache.m[key] = clientOptionsEntry{node: fresh, storedAt: now}
	clientOptionsCache.Unlock()
	return fresh, nil
}

// withCachedClientOptions reuses only the Hy2 transport settings fetched from
// user-config. Names, store visibility and Reality capabilities belong to the
// current build; restoring a whole cached node would undo refreshed metadata.
func withCachedClientOptions(node, cached subscriptionNode) subscriptionNode {
	node.SNI = cached.SNI
	node.Insecure = cached.Insecure
	node.PinSHA256 = cached.PinSHA256
	node.ObfsPass = cached.ObfsPass
	node.HopRange = cached.HopRange
	return node
}

// fetchSubscriptionClientOptions reads the specific account's canonical Hy2
// URI over the already-bound Basic portal session, then copies only client
// transport settings into the generated subscription. The account password is
// checked against the URI before use, and never logged.
func (s *Server) fetchSubscriptionClientOptions(ctx context.Context, node subscriptionNode) (subscriptionNode, error) {
	if node.AccountUserID == "" || node.Password == "" {
		return node, errors.New("订阅节点账号信息不完整")
	}
	target, err := s.loadPortalWebTarget(ctx, node.ID)
	if err != nil {
		return node, fmt.Errorf("读取节点门户凭据失败: %w", err)
	}
	query := url.Values{"user_id": []string{node.AccountUserID}}
	envelope, err := portalWebCall(ctx, target, http.MethodGet, "user-config", nil, query)
	if err != nil {
		return node, fmt.Errorf("读取专属用户配置失败: %w", err)
	}
	var response struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(envelope.Data, &response); err != nil {
		return node, fmt.Errorf("解析专属用户配置失败: %w", err)
	}
	return applySubscriptionClientURI(node, response.URI)
}

// applySubscriptionClientURI validates the portal-generated URI and imports
// only its transport parameters. The service continues to own the node name,
// endpoint, and account password that are rendered to every subscription format.
// errSubscriptionAccountMismatch：节点上这个账号的口令与订阅记录不一致（早期随机口令、节点重装后被改过）。
// 2026-11-06：这是确定性失败 —— 下发库里的口令必然认证失败。不能再拿旧缓存兜底，并要排一次同步重新开户
// （口令由订阅与节点确定性推导，重复 create 不会换口令，重刷是幂等的）。
var errSubscriptionAccountMismatch = errors.New("节点门户返回的账号口令与订阅记录不一致")

// normalizeHopRange 校验端口跳跃区间：只接受逗号分隔的「起-止」（1-65535，起 ≤ 止），单个端口写成 p-p。
// 不合法时返回空串（只是不启用跳跃，节点照常下发）：一台节点的脏值不能让整份 sing-box 配置加载失败。
func normalizeHopRange(raw string) string {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), " ", "")
	if raw == "" {
		return ""
	}
	segs := strings.Split(raw, ",")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		lo, hi, ranged := strings.Cut(seg, "-")
		if !ranged {
			hi = lo
		}
		a, errA := strconv.Atoi(lo)
		b, errB := strconv.Atoi(hi)
		if errA != nil || errB != nil || a < 1 || b > 65535 || a > b {
			return ""
		}
		out = append(out, fmt.Sprintf("%d-%d", a, b))
	}
	return strings.Join(out, ",")
}

func applySubscriptionClientURI(node subscriptionNode, rawURI string) (subscriptionNode, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil || parsed.Scheme != "hysteria2" || parsed.User == nil {
		return node, errors.New("节点门户返回的 Hy2 URI 格式无效")
	}
	if !strings.EqualFold(parsed.Hostname(), node.Host) || parsed.Port() != strconv.Itoa(node.Port) {
		return node, errors.New("节点门户返回的 Hy2 地址与登记地址不一致")
	}
	if parsed.User.Username() != node.Password {
		return node, errSubscriptionAccountMismatch
	}

	query := parsed.Query()
	obfs := strings.TrimSpace(query.Get("obfs"))
	obfsPassword := query.Get("obfs-password")
	if obfs != "" && obfs != "salamander" {
		return node, fmt.Errorf("节点门户返回不支持的 Hy2 混淆模式 %q", obfs)
	}
	if (obfs == "salamander") != (obfsPassword != "") {
		return node, errors.New("节点门户返回的 Salamander 参数不完整")
	}
	if value := strings.TrimSpace(query.Get("sni")); value != "" {
		node.SNI = value
	}
	node.Insecure = query.Get("insecure") == "1"
	node.PinSHA256 = validPinSHA256(query.Get("pinSHA256"))
	node.ObfsPass = obfsPassword
	node.HopRange = normalizeHopRange(query.Get("mport"))
	return node, nil
}

func applySubscriptionAccounts(build []subscriptionNode, rec *subscriptionRecord, categoryNodeIDs map[string]bool) []subscriptionNode {
	if len(build) == 0 {
		return build
	}
	isBuyer := rec != nil && rec.OrderID != ""
	out := make([]subscriptionNode, 0, len(build))
	for _, n := range build {
		// 买家订阅：未入商城的节点绝不下发（这条与既有测试一致）。
		if isBuyer && !n.InStore {
			continue
		}
		// 分类圈定：订阅指定了分类时，只下发属于该分类的节点。
		// categoryNodeIDs 为 nil 表示"不圈定"，此处跳过。
		if categoryNodeIDs != nil && !categoryNodeIDs[n.ID] {
			continue
		}
		// 必须有该节点的专属账号 —— 这是本次新增、也是最关键的一条。
		if rec == nil || rec.nodeAccounts == nil {
			continue
		}
		account, ok := rec.nodeAccounts[n.ID]
		if !ok || account.UserID == "" || account.Password == "" {
			continue
		}
		n.Password = account.Password
		n.AccountUserID = account.UserID
		out = append(out, n)
	}
	return out
}

// needsAccountProvisioning checks if any node **in this subscription's scope**
// is missing a dedicated account for this buyer.
//
// ⚠️ 2026-10-30：判据必须落在订阅的圈定范围上，不能扫全站节点。
//
// 原来遍历的是 build.Nodes（全站在役节点），完全不看 category_id / node_ids：
// 一条只圈了 2 台节点的分类订阅，只要全站还有第 3 台节点，这里就**永远**返回 true。
// 后果不是"多做一次开户"（reconcile 会按范围过滤，不会越界开户），而是：
//   - 买家每次拉订阅都走 JIT 分支，白等最多 8 秒（这条路径是同步阻塞的）；
//   - gap-fill 每小时把这条订阅重新排进同步队列，永远排不完，
//     挤占只有 3 个名额的 worker —— 真正缺账号的订阅反而排在它后面。
//
// 范围解析失败时返回 true 而不是 false：此时确实不知道有没有缺口，
// 而"多排一次同步"的代价远小于"新节点漏开户、客户拿不到可用节点"。
func (s *Server) needsAccountProvisioning(ctx context.Context, nodes []subscriptionNode, rec *subscriptionRecord) bool {
	if rec == nil || !rec.Enabled || rec.expired(time.Now()) {
		return false
	}
	scope, scopeErr := s.categoryScopeFor(ctx, rec)
	if scopeErr != nil {
		s.logger.Warn("provisioning gap check: 解析订阅范围失败，按「可能有缺口」处理",
			"subscription", shortID(rec.ID), "error", scopeErr)
		return true
	}
	return len(missingAccountNodes(nodes, rec, scope)) > 0
}

// missingAccountNodes 返回订阅范围内缺账号的节点。needsAccountProvisioning 与
// ProvisioningGapReport（--gapfill-dry-run）共用它（2026-11-04）：原来诊断报告另写一套、只看 in_store，
// 不看分类 / 直接圈定，也不认自用订阅的未上架节点 —— 报出真实补开户永远不会补的缺口，又漏掉会补的。
func missingAccountNodes(nodes []subscriptionNode, rec *subscriptionRecord, scope map[string]bool) []subscriptionNode {
	if rec == nil {
		return nil
	}
	isBuyer := rec.OrderID != ""
	var out []subscriptionNode
	for _, n := range nodes {
		// 2026-11-03：与 applySubscriptionAccounts / filterProvisionCandidates 同一规则：
		// 只有买家订阅要求节点已上架；自用订阅勾选的未上架节点缺账号也要补。
		if isBuyer && !n.InStore {
			continue
		}
		if scope != nil && !scope[n.ID] {
			continue
		}
		acc, ok := rec.nodeAccounts[n.ID]
		if !ok || acc.UserID == "" || acc.Password == "" {
			out = append(out, n)
		}
	}
	return out
}

// The cache key is derived from each node's id and the fields that shape the
// subscription (name, api_url, domain, port, in_store, api key), so adding,
// deleting, renaming or re-binding a node invalidates immediately; only an
// out-of-band change on the node itself (e.g. someone rotating its password in
// the portal) waits for the TTL. It deliberately ignores updated_at, which the
// status probes bump every few seconds.
func (s *Server) subscriptionBuildFor(ctx context.Context) (*subscriptionBuild, error) {
	rows, err := s.db.Query(ctx, `
SELECT id, name, COALESCE(api_url,''), api_key, COALESCE(domain,''), listen_port, updated_at, in_store,
       COALESCE(portal_basic_user,''), portal_basic_password, COALESCE(portal_path_token,'')
FROM hysteria_nodes
WHERE deleted_at IS NULL
ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	var candidates []subscriptionCandidate
	var keyParts []string
	for rows.Next() {
		var c subscriptionCandidate
		var portalUser, portalPath string
		var portalPass []byte
		if err := rows.Scan(&c.id, &c.name, &c.apiURL, &c.ciphertext, &c.domain, &c.listenPort, &c.updatedAt, &c.inStore, &portalUser, &portalPass, &portalPath); err != nil {
			rows.Close()
			return nil, err
		}
		// 2026-11-06：必须同时具备 Basic 账号、密码与门户路径令牌，网页会话才能读出客户端参数。
		c.hasWebCredential = portalUser != "" && len(portalPass) > 0 && portalPath != ""
		candidates = append(candidates, c)
		// 缓存键纳入 API 密钥与网页门户凭据三元组摘要：轮换任意一项凭据都会立刻失效旧缓存。
		keyHash := sha256.New()
		_, _ = keyHash.Write(c.ciphertext)
		_, _ = keyHash.Write([]byte{0})
		_, _ = keyHash.Write([]byte(portalUser))
		_, _ = keyHash.Write([]byte{0})
		_, _ = keyHash.Write(portalPass)
		_, _ = keyHash.Write([]byte{0})
		_, _ = keyHash.Write([]byte(portalPath))
		keySum := keyHash.Sum(nil)
		keyParts = append(keyParts, fmt.Sprintf("%s@%s@%s@%s@%d@%t@%t@%x", c.id, c.name, c.apiURL, c.domain, c.listenPort, c.inStore, c.hasWebCredential, keySum[:8]))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cacheKey := strings.Join(keyParts, "|")

	return subCache.load(ctx, cacheKey, func(bctx context.Context) *subscriptionBuild {
		return s.resolveSubscriptionBuild(bctx, candidates)
	})
}

// subscriptionRebuildTimeout 同步重建的上限：与后台刷新一致。单节点探测另有 12 秒上限。
var subscriptionRebuildTimeout = 60 * time.Second

// rebuildSubscription 并发查询所有节点、生成订阅构建结果并写入缓存。
func (s *Server) rebuildSubscription(ctx context.Context, candidates []subscriptionCandidate, cacheKey string) *subscriptionBuild {
	subCache.mu.Lock()
	startGen := subCache.gen
	subCache.mu.Unlock()
	build := s.resolveSubscriptionBuild(ctx, candidates)
	subCache.mu.Lock()
	// 若重建过程因上下文取消而提前中断，不将残缺结果写入共享缓存。
	if subCache.gen == startGen && ctx.Err() == nil {
		subCache.key, subCache.build, subCache.storedAt = cacheKey, build, time.Now()
	}
	subCache.mu.Unlock()
	return build
}

func (s *Server) resolveSubscriptionBuild(ctx context.Context, candidates []subscriptionCandidate) *subscriptionBuild {
	build := &subscriptionBuild{
		Nodes:    make([]subscriptionNode, 0, len(candidates)),
		Excluded: make([]subscriptionExcluded, 0),
	}

	// Probe nodes concurrently with a bounded pool; collect results by index so
	// the output order stays stable (config diffs stay readable).
	type slot struct {
		node     *subscriptionNode
		excluded *subscriptionExcluded
	}
	slots := make([]slot, len(candidates))
	sem := make(chan struct{}, subscriptionResolveConcurrency)
	var wg sync.WaitGroup
	for i, candidate := range candidates {
		wg.Add(1)
		go func(i int, candidate subscriptionCandidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			node, reason := s.resolveSubscriptionNode(ctx, candidate)
			if node != nil {
				slots[i] = slot{node: node}
				return
			}
			slots[i] = slot{excluded: &subscriptionExcluded{NodeID: candidate.id, Name: candidate.name, Reason: reason}}
		}(i, candidate)
	}
	wg.Wait()

	for _, entry := range slots {
		if entry.node != nil {
			build.Nodes = append(build.Nodes, *entry.node)
		}
		if entry.excluded != nil {
			build.Excluded = append(build.Excluded, *entry.excluded)
		}
	}

	build.Nodes = dedupeNodeNames(build.Nodes)

	return build
}

func InvalidateSubscriptionCache() {
	subCache.mu.Lock()
	subCache.build = nil
	subCache.key = ""
	subCache.gen++
	subCache.mu.Unlock()

	clientOptionsCache.Lock()
	clientOptionsCache.m = map[string]clientOptionsEntry{}
	clientOptionsCache.Unlock()
}

// dedupeNodeNames makes every proxy name unique.
//
// This is not cosmetic: Clash and sing-box key proxies by name, and a duplicate
// makes the client drop one of them (or refuse the profile) with no useful error
// — the node simply never appears. Two nodes legitimately share a name whenever
// an operator copies an existing entry, so collisions are resolved
// deterministically by suffixing the port, then by a counter for the remaining
// pathological cases.
// ⚠️ 2026-10-25 五轮排查第四轮：去重必须覆盖**实际下发的全部名字**，
// 而不只是主名。
//
// 每条备用通道都会派生出自己的名字（Name-Reality / Name-VMess /
// Name-VLESS-Enc）。只对主名去重时，一台叫「东京」的节点，它的 Reality
// 派生名「东京-Reality」可能正好等于另一台节点的主名 —— 于是客户端里
// 两个代理同名，Clash 会**静默丢掉其中一个**，用户只看到少了一台。
//
// 这里按「主名 + 所有会出现的派生名」一次性占位：先给主名让路，
// 再给派生名让路，冲突时统一走同一套后缀规则。
func dedupeNodeNames(nodes []subscriptionNode) []subscriptionNode {
	seen := make(map[string]bool, len(nodes)*4+8)
	// 预占 Clash / sing-box 内置策略名，防止节点名与其冲突导致客户端加载配置失败。
	for _, reserved := range []string{"PROXY", "AUTO", "DIRECT", "REJECT"} {
		seen[reserved] = true
	}
	for i := range nodes {
		base := nodes[i].Name
		name := base
		// 与原来一致：先试原名，再试「原名-端口」，最后加 -x。
		if nameFamilyTaken(name, seen) {
			name = fmt.Sprintf("%s-%d", base, nodes[i].Port)
		}
		for nameFamilyTaken(name, seen) {
			name += "-x"
		}
		registerNameFamily(name, seen)
		nodes[i].Name = name
	}
	return nodes
}

// subscriptionDerivedSuffixes 是所有会出现在客户端里的派生名后缀。
// 新增备用通道时必须同步这里，否则它的名字不参与去重。
var subscriptionDerivedSuffixes = []string{"-Reality", "-VMess", "-VLESS-Enc"}

func nameFamilyTaken(base string, seen map[string]bool) bool {
	if seen[base] {
		return true
	}
	for _, suffix := range subscriptionDerivedSuffixes {
		if seen[base+suffix] {
			return true
		}
	}
	return false
}

func registerNameFamily(base string, seen map[string]bool) {
	seen[base] = true
	for _, suffix := range subscriptionDerivedSuffixes {
		seen[base+suffix] = true
	}
}

// resolveSubscriptionNode turns one candidate into a ready node, or explains why
// it cannot be used. Returning a reason (rather than a bare error) is what lets
// the UI show "hy2-kr-443：未绑定节点侧 API，拿不到口令".
func (s *Server) resolveSubscriptionNode(ctx context.Context, c subscriptionCandidate) (*subscriptionNode, string) {
	if c.apiURL == "" || len(c.ciphertext) == 0 {
		return nil, "未绑定节点侧 API，拿不到该节点的口令"
	}
	// 2026-11-06：没有门户网页凭据的节点在 subscriptionNodesFor 里必然读不到客户端参数、被静默剔除 ——
	// 订阅少一台节点，交付页的「暂时不可用」与订阅中心的「未包含」里都看不到它。在这里如实排除并写明原因。
	if !c.hasWebCredential {
		return nil, "未绑定完整的节点门户网页凭据（Basic 账号或门户路径令牌缺失），读不到客户端参数；请在节点页粘贴 portal-access.json 重新接入"
	}
	if s.box == nil {
		return nil, "服务端未配置凭据加密密钥"
	}
	plain, err := s.box.Open(c.ciphertext)
	if err != nil {
		return nil, "节点 API 密钥解密失败"
	}
	probeCtx, cancel := context.WithTimeout(ctx, subscriptionNodeProbeTimeout)
	defer cancel()

	// ⚠️ 2026-10-24 审计 P1-3：/node/meta 与 /capabilities 并发发。
	//
	// 原来是先 meta、再（在结构体字面量里）串行取 Reality —— 单节点两趟往返。
	// 两趟之间没有数据依赖：capabilities 只要 apiURL + apiKey，不需要 meta 的
	// 任何结果。串行发只是历史顺序留下的，代价是每个节点白等一趟往返，
	// 冷缓存时累积成 5~6 秒（实测 5662~6304ms）。
	//
	// 并发后单节点耗时 = max(meta, capabilities) 而不是两者之和。
	var reality *nodeReality
	var vmess *nodeVMess
	var vlessEnc *nodeVLESSEncryption
	realityDone := make(chan struct{})
	go func() {
		defer close(realityDone)
		reality, vmess, vlessEnc = s.fetchNodeProtocols(probeCtx, c.id, c.apiURL, string(plain))
	}()
	// 任意提前返回路径都先取消 probeCtx 并回收并发协议探测协程，防止协程与连接泄漏。
	defer func() {
		cancel()
		<-realityDone
	}()

	envelope, err := portalCall(probeCtx, c.apiURL, string(plain), http.MethodGet, "/node/meta", nil)
	if err != nil {
		return nil, "读取节点 meta 失败：" + err.Error()
	}
	var parsed struct {
		Meta portalMeta `json:"meta"`
	}
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		return nil, "节点 meta 无法解析"
	}
	meta := parsed.Meta

	serverName := strings.TrimSpace(meta.ServerName)
	if serverName == "" {
		serverName = strings.TrimSpace(meta.PublicIP)
	}
	if serverName == "" {
		serverName = strings.TrimSpace(c.domain)
	}
	publicIP := strings.TrimSpace(meta.PublicIP)
	if publicIP == "" {
		publicIP = serverName
	}
	if serverName == "" {
		return nil, "节点 meta 里既没有 server_name 也没有 public_ip"
	}
	// Mirror portal.py's artifacts(): a self-signed node connects by IP and skips
	// verification, a trusted-cert node connects by name.
	host := serverName
	if meta.IsInsecure {
		host = publicIP
	}

	port := meta.ListenPort
	if port < 1 || port > 65535 {
		port = c.listenPort
	}
	if port < 1 || port > 65535 {
		return nil, "节点 meta 里没有合法的 listen_port"
	}

	password := strings.TrimSpace(meta.AuthPassword)
	// 注意：这里**不再**把空 auth_password 判为致命错误（2026-10-09）。
	//
	// 缘由：hysteria2-installer 的 portal 2.4（2026-10-09 发布）把 /node/meta
	// 改成了白名单输出，剔除 auth_password/obfs_password/pin_sha256 —— 修的是
	// 「低权限 api_key 换到机主主密码」的提权泄漏。而自 2026-10-03 专属账号
	// 改造之后，买家配置里的口令在 applySubscriptionAccounts 里**永远**会被
	// rec.nodeAccounts 的专属口令覆盖，master auth_password 本就不该再作为
	// 下发前提。旧判据的后果是：2.4 节点整个进 excluded，JIT 开户循环不到它，
	// 新节点永远推不给老订阅（2026-10-09 哥伦布-AWS-10088 实际踩中）。
	//
	// 空口令在这里只表示「这台不发放 master 口令」，节点照常进 build，
	// 由 users/create（2.4 仍是完整契约）补齐专属账号后下发。
	//
	// ⚠️ 边界：2.4 同时隐藏 obfs_password。若节点开了 obfs 而这里拿不到口令，
	// 下发的配置会缺 obfs 参数导致连不上。capabilities/node/meta 在 2.4 里都
	// 不再暴露 obfs 状态，面板无法自行判定 —— 根治要靠门户侧把
	// obfs_password/pin_sha256 恢复为公开参数（它们本来就是每个客户端配置
	// 必须携带的值，与 reality 的 pbk/sid/sni 同类，不是机密）。
	// 在那之前，给 2.4 节点开 obfs 属于**未支持组合**。

	name := strings.TrimSpace(c.name)
	if name == "" {
		name = fmt.Sprintf("hy2-%d", port)
	}

	// 等协议探测那一趟落地再组装。
	//
	// 必须在 return 之前等：defer cancel() 会在函数返回的瞬间掐掉 probeCtx，
	// 若这时 goroutine 还在读响应体，它会拿到 context canceled ——
	// 表现为「备用通道偶发读不到」，而日志里看不出任何异常
	// （fetchNodeProtocols 按设计把任何错误都吞成 nil）。
	// 这一等是这段并发改造的正确性前提。
	<-realityDone

	return &subscriptionNode{
		ID:        c.id,
		Name:      name,
		Host:      host,
		Port:      port,
		Password:  password,
		SNI:       serverName,
		Insecure:  meta.IsInsecure,
		PinSHA256: validPinSHA256(meta.PinSHA256),
		ObfsPass:  strings.TrimSpace(meta.ObfsPassword),
		HopRange:  normalizeHopRange(meta.HopPortRange),
		InStore:   c.inStore,
		Reality:   reality,
		VMess:     vmess,
		VLESSEnc:  vlessEnc,
	}, ""
}

// fetchNodeProtocols 读取一台节点的**全部**备用通道公开参数。
//
// 返回 nil 表示「这台没有该通道」，覆盖三种正常情况：
//   - 节点门户较旧（没有 capabilities 端点 / 没有对应协议段）；
//   - 节点装了门户但没开该协议（未装 xray / 未生成配置）；
//   - 参数不全（缺 pbk / sid / sni / domain / port / encryption）。
//
// 三种都**不是错误**，所以只返回 nil 而不向上报错 —— 调用方据此决定
// 「少输出一条备用通道」，而不会因此把整台节点踢出订阅。
// 这是刻意的：备用通道缺失不该影响 Hy2 主通道的可用性。
//
// ⚠️ 复用调用方传入的 ctx：resolveSubscriptionNode 已经设了单节点超时，
// 这里再起一个会让「单节点探测总耗时」变成两倍，批量探测时明显变慢。
//
// 注：原来的 fetchNodeReality 只是这个函数的 Reality 投影，已随
// 2026-10-25 五轮排查第五轮删除（无调用点）。
func (s *Server) fetchNodeProtocols(ctx context.Context, nodeID, apiURL, apiKey string) (*nodeReality, *nodeVMess, *nodeVLESSEncryption) {
	envelope, err := portalCall(ctx, apiURL, apiKey, http.MethodGet, "/capabilities", nil)
	if err != nil {
		return nil, nil, nil
	}
	// ⚠️ 2026-10-25 五轮排查第一轮：这里必须解成 map[string]any，不能解成
	// map[string]map[string]any。
	//
	// 后者遇到**任何**一个值不是对象的协议键（未来协议可能用布尔/字符串表示
	// 状态，例如 {"future_protocol": true}）就会让整个 Unmarshal 失败，
	// 于是 Reality / VMess / VLESS Encryption 三条通道一起从订阅里消失 ——
	// 而且日志里没有任何异常，因为这里按设计把错误吞成 nil。
	// 一个不认识的新协议不该让所有已知协议失效（同 NodeCapabilities 的
	// 「未知协议不自动适配、但也不能破坏已知协议」原则）。
	var payload struct {
		Protocols map[string]any `json:"protocols"`
	}
	if json.Unmarshal(envelope.Data, &payload) != nil {
		return nil, nil, nil
	}
	protocols := protocolSections(payload.Protocols)
	r := protocols["vless_reality"]
	_, modern := protocols["vmess"]
	encCap := vlessEncFromCapability(protocols["vlessenc"])

	// ⚠️ 2026-10-25 五轮排查第二轮：Reality 与 VLESS Encryption 的状态读取
	// 必须**并发**，且只加载一次网页凭据。
	//
	// 原来的写法是串行三步（capabilities → reality-status → vlessenc-status），
	// 全部共用一个 5 秒的单节点预算。而节点侧 vlessenc-status 会现跑
	// qrencode（上游 timeout=3s），现代节点上三步叠加极易顶穿 5 秒 ——
	// 表现是「Reality / VLESS Encryption 偶发从订阅里消失」，
	// 且因为错误按设计被吞成 nil，日志里什么都看不到。
	//
	// 两步之间没有数据依赖，并发后单节点耗时从
	// capabilities + reality + vlessenc 变成 capabilities + max(reality, vlessenc)。
	var target *portalWebTarget
	if nodeID != "" && (modern || encCap != nil) {
		// 只查一次库：原来 Reality 与 VLESS 各自 loadPortalWebTarget 一遍，
		// 每台节点白白多一次数据库往返（批量探测时是 N 次）。
		target, _ = s.loadPortalWebTarget(ctx, nodeID)
	}

	var (
		wg       sync.WaitGroup
		vlessEnc *nodeVLESSEncryption
	)
	if modern {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verifyRealityCapability(ctx, r, target)
		}()
	}
	if encCap != nil && nodeID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 无 Basic 凭据 / 端点缺失时返回 nil + 原因；订阅里不出现该通道。
			vlessEnc, _ = loadNodeVLESSEncryptionWithTarget(ctx, target, encCap)
		}()
	}
	wg.Wait()

	var reality *nodeReality
	if capabilityBool(r, "installed") && capabilityBool(r, "active") {
		reality = &nodeReality{PublicKey: capabilityString(r, "public_key"), ShortID: capabilityString(r, "short_id"), DestSNI: capabilityString(r, "dest_sni"), Port: capabilityPort(r)}
		if !reality.ready() {
			reality = nil
		}
	}
	return reality, vmessFromCapability(protocols["vmess"]), vlessEnc
}

// validPinSHA256 keeps only a lowercase HEX64 pin. portal.py enforces the same
// rule because Xray rejects anything else with `encoding/hex: invalid byte`, and
// a stale base64 pin from an older installer must not propagate into fresh
// configs.
func validPinSHA256(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if len(value) != 64 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return value
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// pctEncode percent-encodes everything outside RFC 3986's unreserved set.
//
// url.QueryEscape is deliberately NOT used: it encodes a space as "+", and the
// password lands in the URI's userinfo section where "+" is a literal plus, not
// a space. Python's quote(safe=”) — which portal.py uses — encodes space as
// "%20", and the two must agree or a password containing a space would be
// silently wrong.
func pctEncode(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

// hysteria2URI mirrors portal.py's artifacts() URI exactly, including the
// pinSHA256 / insecure pairing rule (pin is only emitted on self-signed nodes).
// realityUUIDForUser 复刻 portal.py 的
//
//	uuid5(NAMESPACE_URL, 'hy2-portal-reality:' + user_id)
//
// ⚠️ 这是**跨语言契约**，一个字节都不能差：
// 节点侧 xray.json 里该用户的 client id 就是这个值，
// 面板算出来的必须与它完全一致，否则客户端拿到的 UUID 不在服务端白名单里，
// 表现为「连接被拒」且日志只会说 authentication failed。
//
// 因此这里手写 UUIDv5（SHA-1 + 版本/变体位），不引第三方库 ——
// 少一个依赖，也就少一处版本漂移导致「两边算法悄悄不一致」的风险。
// 正确性已用 portal 侧同输入的实际输出做过对拍验证。
//
// 为什么用 uuid5 而不是随机的：同一个 user_id 永远得到同一个 UUID，
// 于是订阅可以反复拉取、销户再开户也保持一致；随机 UUID 会让客户端的
// 每次刷新都变成一个新节点。
func realityUUIDForUser(userID string) string {
	return protocolUUIDForUser("hy2-portal-reality:", userID)
}

func protocolUUIDForUser(prefix, userID string) string {
	const nsURL = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
	ns := parseUUIDBytes(nsURL)
	name := []byte(prefix + userID)

	h := sha1.New()
	h.Write(ns)
	h.Write(name)
	sum := h.Sum(nil)

	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

// parseUUIDBytes 把标准 36 字符 UUID 文本解成 16 字节。
// 输入不合法时返回全零 —— 调用方只在本文件内用它处理常量命名空间，
// 该常量是编译期已知的合法值，所以这里不做错误传播。
func parseUUIDBytes(s string) []byte {
	out := make([]byte, 16)
	hi, lo := -1, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c == '-':
			continue
		default:
			return make([]byte, 16)
		}
		if hi < 0 {
			hi = int(v)
		} else {
			if lo >= 16 {
				return out
			}
			out[lo] = byte(hi<<4 | int(v))
			lo++
			hi = -1
		}
	}
	return out
}

// formatUUID 输出 8-4-4-4-12 的小写标准形式（与 Python str(uuid) 一致）。
func formatUUID(b [16]byte) string {
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, 36)
	for i, x := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf = append(buf, '-')
		}
		buf = append(buf, hex[x>>4], hex[x&0x0f])
	}
	return string(buf)
}

func hysteria2URI(n subscriptionNode) string {
	params := []string{"sni=" + pctEncode(n.SNI)}
	if n.Insecure {
		params = append(params, "insecure=1")
		if n.PinSHA256 != "" {
			params = append(params, "pinSHA256="+n.PinSHA256)
		}
	}
	if n.ObfsPass != "" {
		params = append(params, "obfs=salamander", "obfs-password="+pctEncode(n.ObfsPass))
	}
	if n.HopRange != "" {
		params = append(params, "mport="+pctEncode(n.HopRange))
	}
	// 2026-11-03：host:port 用 net.JoinHostPort 拼 —— IPv6 地址必须加方括号，
	// 原来 hysteria2://pw@2001:db8::1:443 这种 URI 客户端解析不了，节点静默消失。
	return fmt.Sprintf("hysteria2://%s@%s?%s#%s",
		pctEncode(n.Password), net.JoinHostPort(n.Host, strconv.Itoa(n.Port)), strings.Join(params, "&"), pctEncode(n.Name))
}

func clashProxy(n subscriptionNode) map[string]any {
	proxy := map[string]any{
		"name":             n.Name,
		"type":             "hysteria2",
		"server":           n.Host,
		"port":             n.Port,
		"password":         n.Password,
		"sni":              n.SNI,
		"skip-cert-verify": n.Insecure,
	}
	if n.HopRange != "" {
		proxy["ports"] = fmt.Sprintf("%d,%s", n.Port, n.HopRange)
	}
	if n.ObfsPass != "" {
		proxy["obfs"] = "salamander"
		proxy["obfs-password"] = n.ObfsPass
	}
	// 2026-11-03：自签节点带上证书指纹。URI 列表一直有 pinSHA256，Clash 配置却只有
	// skip-cert-verify —— Clash 又是未知 UA 的默认格式，于是这些客户端对自签节点完全不验证证书。
	// mihomo 的 fingerprint 是整张 DER 证书的 SHA-256（hex，冒号可有可无），与 pinSHA256 同一口径；
	// 设了它 mihomo 就按指纹校验，skip-cert-verify 不再生效。
	// sing-box 的同类字段 certificate_sha256 要 1.15+，老客户端遇到未知字段会整份配置报错，暂不下发。
	if n.Insecure && n.PinSHA256 != "" {
		proxy["fingerprint"] = n.PinSHA256
	}
	return proxy
}

// realityURI 拼一条 VLESS-Reality 直链，字段顺序与取值口径对齐
// portal.py 的 reality_uri_for_user()：
//
//	vless://<uuid>@<host>:<port>?security=reality&encryption=none&pbk=...&
//	  headerType=none&fp=chrome&type=tcp&flow=xtls-rprx-vision&sni=...&sid=...
//
// ⚠️ flow 必须是 xtls-rprx-vision：portal 生成 xray clients 时写死了它，
// 客户端不带同一个 flow 会被服务端拒绝。
func realityURI(n subscriptionNode, uuid string) string {
	r := n.Reality
	params := []string{
		"security=reality",
		"encryption=none",
		"pbk=" + pctEncode(r.PublicKey),
		"headerType=none",
		"fp=chrome",
		"type=tcp",
		"flow=xtls-rprx-vision",
		"sni=" + pctEncode(r.DestSNI),
		"sid=" + pctEncode(r.ShortID),
	}
	return fmt.Sprintf("vless://%s@%s?%s#%s",
		uuid, net.JoinHostPort(n.Host, strconv.Itoa(r.Port)), strings.Join(params, "&"), pctEncode(n.RealityName()))
}

// RealityName 是这条 Reality 节点在客户端里显示的名字。
//
// 后缀 -Reality 与 portal.py 的 artifacts() 保持一致 ——
// 用户在两处看到同一个名字才不会以为是两个不同的东西。
func (n subscriptionNode) RealityName() string {
	return n.Name + "-Reality"
}

// clashRealityProxy 生成 Clash/mihomo 的 vless + reality 节点。
//
// 字段名以 mihomo 实际接受为准：reality_opts 是**下划线**形式
// （新版 mihomo 也接受连字符写法，但下划线兼容面更广，
// 且与 portal.py 输出的单节点 Clash 完全一致）。
func clashRealityProxy(n subscriptionNode, uuid string) map[string]any {
	r := n.Reality
	return map[string]any{
		"name":               n.RealityName(),
		"type":               "vless",
		"server":             n.Host,
		"port":               r.Port,
		"uuid":               uuid,
		"udp":                true,
		"tls":                true,
		"flow":               "xtls-rprx-vision",
		"servername":         r.DestSNI,
		"client-fingerprint": "chrome",
		"reality_opts": map[string]any{
			"public_key": r.PublicKey,
			"short_id":   r.ShortID,
		},
	}
}

// singBoxRealityOutbound 生成 sing-box 的 vless + reality outbound。
func singBoxRealityOutbound(n subscriptionNode, uuid string) map[string]any {
	r := n.Reality
	return map[string]any{
		"type":        "vless",
		"tag":         n.RealityName(),
		"server":      n.Host,
		"server_port": r.Port,
		"uuid":        uuid,
		"flow":        "xtls-rprx-vision",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": r.DestSNI,
			"utls": map[string]any{
				"enabled":     true,
				"fingerprint": "chrome",
			},
			"reality": map[string]any{
				"enabled":    true,
				"public_key": r.PublicKey,
				"short_id":   r.ShortID,
			},
		},
	}
}

func singBoxOutbound(n subscriptionNode) map[string]any {
	out := map[string]any{
		"type":     "hysteria2",
		"tag":      n.Name,
		"server":   n.Host,
		"password": n.Password,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": n.SNI,
			"insecure":    n.Insecure,
		},
	}
	if n.HopRange != "" {
		// sing-box replaces server_port with a list when port hopping is on.
		// 每一项都必须是 "起:止" 区间 —— 单个端口也要写成 "14617:14617"，
		// 写成 "14617" 时 sing-box 直接报 bad port range、整份配置加载失败（2026-10-17 用官方内核实测）。
		// 2026-11-06：多段（「20000-30000,40000-50000」）要拆成多项。原来整串只换了「-」，得到一项
		// 「20000:30000,40000:50000」—— sing-box 报 bad port range，整份配置里所有节点都用不了。
		ports := []string{fmt.Sprintf("%d:%d", n.Port, n.Port)}
		for _, seg := range strings.Split(n.HopRange, ",") {
			lo, hi, _ := strings.Cut(seg, "-")
			ports = append(ports, lo+":"+hi)
		}
		out["server_ports"] = ports
	} else {
		out["server_port"] = n.Port
	}
	if n.ObfsPass != "" {
		out["obfs"] = map[string]any{"type": "salamander", "password": n.ObfsPass}
	}
	return out
}

// subscriptionChannel 是「备用通道」的标识。三个格式渲染器共用
// subscriptionEmitsChannel 决定发不发，避免三处各写一遍判据后悄悄漂移
// （2026-10-25 五轮排查第五轮抽出）。
const (
	channelReality  = "reality"
	channelVMess    = "vmess"
	channelVLESSEnc = "vlessenc"
)

// subscriptionEmitsChannel 回答「某个格式要不要下发某条备用通道」。
//
// 唯一的格式差异是 sing-box：官方 VLESS outbound 没有 encryption 字段，
// 塞进去会被静默忽略 ⇒ 客户端按普通 VLESS 握手 ⇒ 服务端要求加密 ⇒ 连不上。
// 所以 VLESS Encryption 在 sing-box 下**不下发**，其余全部下发。
//
// 新增备用通道或新增格式时，只改这里。
func subscriptionEmitsChannel(format, channel string) bool {
	if format == subscriptionFormatSingBox && channel == channelVLESSEnc {
		return false
	}
	return true
}

// renderClash emits the full Clash/mihomo document.
//
// Groups: manual pick, auto (lowest latency), failover, and an AI group over the
// US nodes when there are any; rules route LAN/China direct and AI to its group.
// See subscription_rules.go.
//
// Output is JSON, which is also valid YAML. portal.py does the same for its
// clash.yaml; emitting real YAML would add an encoder and quoting rules for no
// functional gain, since every Clash-family client parses this as-is.
func renderClash(nodes []subscriptionNode) ([]byte, error) {
	return renderClashWithInfo(nodes, nil)
}

// renderClashWithInfo：info 是置顶的套餐信息条目名（见 subscription_info.go）。
func renderClashWithInfo(nodes []subscriptionNode, info []string) ([]byte, error) {
	proxies := make([]map[string]any, 0, len(nodes)+len(info))
	names := make([]string, 0, len(nodes))
	for _, name := range info {
		proxies = append(proxies, infoClashProxy(name))
	}
	for _, n := range nodes {
		proxies = append(proxies, clashProxy(n))
		names = append(names, n.Name)
		// Reality 备用通道（2026-10-04）：同一台节点再发一条 VLESS-Reality。
		//
		// 为什么要发两条而不是二选一：Hy2 走 UDP，快但在
		// 「UDP 被 QoS / 阻断」的网络里直接不可用；Reality 走 TCP，
		// 慢一点但能穿过去。**客户端按延迟自动择优**才是正确形态 ——
		// 让用户自己判断"我的网络有没有封 UDP"是不现实的。
		//
		// 用 AccountUserID 派生 UUID，与节点侧 xray client 一一对应；
		// 没有专属账号时不下发（拿了也用不了 —— 那不是他的身份）。
		if n.Reality.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatClash, channelReality) {
			uuid := realityUUIDForUser(n.AccountUserID)
			proxies = append(proxies, clashRealityProxy(n, uuid))
			names = append(names, n.RealityName())
		}
		if n.VMess.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatClash, channelVMess) {
			proxies = append(proxies, clashVMessProxy(n))
			names = append(names, n.VMessName())
		}
		// VLESS Encryption（2026-10-24）：mihomo 官方 VLESS 代理支持顶层
		// `encryption` 字段，因此 Clash 格式可以下发。
		// UUID 必须与 Reality 共用命名空间 —— 上游 vlessenc_clients() 就是
		// reality_uuid_for_user(user_id)，用别的 UUID 会被服务端拒绝。
		if n.VLESSEnc.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatClash, channelVLESSEnc) {
			proxies = append(proxies, clashVLESSEncryptionProxy(n, realityUUIDForUser(n.AccountUserID)))
			names = append(names, n.VLESSEncName())
		}
	}
	// 分组与分流规则见 subscription_rules.go。
	document := map[string]any{
		"mixed-port":   7890,
		"allow-lan":    false,
		"mode":         "rule",
		"log-level":    "info",
		"proxies":      proxies,
		"proxy-groups": withInfoInSelect(clashProxyGroups(names), info),
		"rules":        clashRules(len(usNodes(names)) > 0),
	}
	return json.MarshalIndent(document, "", "  ")
}

// renderSingBox emits outbounds only: a client importing a subscription merges
// outbounds, and inventing inbound/DNS sections would fight the user's own config.
func renderSingBox(nodes []subscriptionNode) ([]byte, error) {
	outbounds := make([]map[string]any, 0, len(nodes)+3)
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		outbounds = append(outbounds, singBoxOutbound(n))
		names = append(names, n.Name)
		// 与 renderClash 同一判据：有 Reality + 有专属账号才追加。
		if n.Reality.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatSingBox, channelReality) {
			uuid := realityUUIDForUser(n.AccountUserID)
			outbounds = append(outbounds, singBoxRealityOutbound(n, uuid))
			names = append(names, n.RealityName())
		}
		if n.VMess.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatSingBox, channelVMess) {
			outbounds = append(outbounds, singBoxVMessOutbound(n))
			names = append(names, n.VMessName())
		}
		// ⚠️ VLESS Encryption 刻意**不下发 sing-box**（2026-10-24 核对官方文档）：
		// sing-box 官方 VLESS outbound 字段只有 uuid/flow/network/tls/
		// packet_encoding/multiplex/transport，**没有 encryption**。
		// 把该参数塞进去会被静默忽略，客户端仍按普通 VLESS 握手 ——
		// 服务端要求加密，结果就是连不上。宁可不发，也不发一条被忽略的配置。
		// （第三方分支 sing-box-lx 才支持，不能当作官方能力依赖。）
	}
	// tag 名 PROXY / AUTO 保持不变：sing-box 订阅是并入用户自己的配置，
	// 用户的路由规则可能按这两个名字引用。
	selectMembers := append([]string{"AUTO"}, names...)
	selectMembers = append(selectMembers, "DIRECT")
	autoMembers := names
	if len(autoMembers) == 0 {
		autoMembers = []string{"DIRECT"}
	}
	outbounds = append(outbounds,
		map[string]any{"type": "selector", "tag": "PROXY", "outbounds": selectMembers, "default": "AUTO"},
		map[string]any{
			"type":      "urltest",
			"tag":       "AUTO",
			"outbounds": autoMembers,
			"url":       "http://www.gstatic.com/generate_204",
			"interval":  "5m",
			"tolerance": 50,
		},
		map[string]any{"type": "direct", "tag": "DIRECT"},
	)
	document := map[string]any{
		"log":       map[string]any{"level": "info"},
		"outbounds": outbounds,
	}
	return json.MarshalIndent(document, "", "  ")
}

// renderURIList emits a base64-encoded newline-joined URI list — the format
// v2rayN/v2rayNG and most generic "订阅" importers expect.
func renderURIList(nodes []subscriptionNode) []byte {
	return renderURIListWithInfo(nodes, nil)
}

func renderURIListWithInfo(nodes []subscriptionNode, info []string) []byte {
	uris := make([]string, 0, len(nodes)*2+len(info))
	for _, name := range info {
		uris = append(uris, infoURI(name))
	}
	for _, n := range nodes {
		uris = append(uris, hysteria2URI(n))
		// 同样的追加规则：v2rayN/v2rayNG 这类客户端也应当拿到备用通道。
		if n.Reality.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatURI, channelReality) {
			uris = append(uris, realityURI(n, realityUUIDForUser(n.AccountUserID)))
		}
		if n.VMess.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatURI, channelVMess) {
			uris = append(uris, vmessURI(n))
		}
		if n.VLESSEnc.ready() && n.AccountUserID != "" && subscriptionEmitsChannel(subscriptionFormatURI, channelVLESSEnc) {
			uris = append(uris, vlessEncURI(n, realityUUIDForUser(n.AccountUserID)))
		}
	}
	return []byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(uris, "\n"))))
}

// negotiateSubscriptionFormat picks the payload shape. An explicit ?format=
// always wins; otherwise the User-Agent decides, which is how every
// subscription service behaves because clients cannot send an Accept header
// meaningfully.
func negotiateSubscriptionFormat(r *http.Request) string {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))) {
	case "clash", "clashmeta", "mihomo", "yaml", "yml":
		return subscriptionFormatClash
	case "singbox", "sing-box", "sfa", "sfi", "sft", "nekobox":
		return subscriptionFormatSingBox
	case "uri", "base64", "v2ray", "v2rayn", "v2rayng", "text":
		return subscriptionFormatURI
	}
	ua := strings.ToLower(r.UserAgent())
	switch {
	case strings.Contains(ua, "clash"), strings.Contains(ua, "mihomo"),
		strings.Contains(ua, "stash"), strings.Contains(ua, "surfboard"),
		strings.Contains(ua, "clashx"), strings.Contains(ua, "clash-verge"):
		return subscriptionFormatClash
	case strings.Contains(ua, "sing-box"), strings.Contains(ua, "singbox"),
		strings.Contains(ua, "sfa"), strings.Contains(ua, "sfi"),
		strings.Contains(ua, "nekobox"), strings.Contains(ua, "nekoray"):
		return subscriptionFormatSingBox
	case strings.Contains(ua, "hysteria"), strings.Contains(ua, "v2rayng"),
		strings.Contains(ua, "v2rayn"), strings.Contains(ua, "shadowrocket"),
		strings.Contains(ua, "kitsunebi"):
		return subscriptionFormatURI
	}
	// Clash/mihomo is the most common merged-subscription target in practice, and
	// ?format= is always available for everything else.
	return subscriptionFormatClash
}

func renderSubscription(nodes []subscriptionNode, format string) ([]byte, string, error) {
	switch format {
	case subscriptionFormatSingBox:
		body, err := renderSingBox(nodes)
		return body, "application/json; charset=utf-8", err
	case subscriptionFormatURI:
		return renderURIList(nodes), "text/plain; charset=utf-8", nil
	default:
		body, err := renderClash(nodes)
		return body, "text/yaml; charset=utf-8", err
	}
}

// subscriptionNodePublic is the browser-facing view of an included node.
//
// It carries no credential: `Password` is the node's master auth_password and
// `PinSHA256` is a cert pin. The management API only needs to tell the operator
// *which* nodes made it into the link, so it reports the routing facts and a
// boolean for obfs, never the secrets that the link itself embodies.
type subscriptionNodePublic struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	SNI      string `json:"sni"`
	Insecure bool   `json:"insecure"`
	HasObfs  bool   `json:"has_obfs"`
	HopRange string `json:"hop_range"`
}

func (n subscriptionNode) redacted() subscriptionNodePublic {
	return subscriptionNodePublic{
		ID:       n.ID,
		Name:     n.Name,
		Host:     n.Host,
		Port:     n.Port,
		SNI:      n.SNI,
		Insecure: n.Insecure,
		HasObfs:  n.ObfsPass != "",
		HopRange: n.HopRange,
	}
}

func redactSubscriptionNodes(nodes []subscriptionNode) []subscriptionNodePublic {
	out := make([]subscriptionNodePublic, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.redacted())
	}
	return out
}

// ---------------------------------------------------------------------------
// Token management
// ---------------------------------------------------------------------------

func newSubscriptionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// subscriptionTokenState is the row the management API reports.
type subscriptionTokenState struct {
	Token          string     `json:"token"`
	Label          string     `json:"label"`
	Enabled        bool       `json:"enabled"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAccessedAt *time.Time `json:"last_accessed_at"`
	AccessCount    int64      `json:"access_count"`
}

// loadOrCreateSubscriptionToken returns the singleton subscription token,
// creating it on first use. The unique index on `(TRUE)` makes the singleton
// guarantee a database invariant rather than a convention, so a concurrent pair
// of requests cannot end up publishing two different links.
func (s *Server) loadOrCreateSubscriptionToken(ctx context.Context, userID string) (*subscriptionTokenState, error) {
	state, err := s.readSubscriptionToken(ctx)
	if err == nil {
		return state, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	token, err := newSubscriptionToken()
	if err != nil {
		return nil, err
	}
	var createdBy any
	if userID != "" {
		createdBy = userID
	}
	_, err = s.db.Exec(ctx, `INSERT INTO subscription_tokens (token, created_by, is_panel_merged) VALUES ($1, $2, true) ON CONFLICT DO NOTHING`, token, createdBy)
	if err != nil {
		return nil, err
	}
	// Re-read rather than trusting our own insert: a concurrent request may have
	// won the race, and both callers must observe the same token.
	return s.readSubscriptionToken(ctx)
}

// readSubscriptionToken 取「那条」面板自建的合并订阅。
//
// ⚠️ 这里曾经是 `SELECT ... FROM subscription_tokens LIMIT 1`，没有任何
// WHERE、也没有 ORDER BY —— 于是 PostgreSQL 返回哪一行完全不确定
// （取决于物理存储顺序），而且**会把软删除的行也算进来**。
//
// 实测踩到的后果：表里 22 行、其中 21 行是回归脚本软删除的残留，
// `LIMIT 1` 恰好返回了 `回归-1-真删`（enabled=false、deleted_at 非空），
// 于是 /api/subscription 把这条已撤销的 token 当成"当前合并订阅"下发，
// 客户端拿到的 /sub/<token> 直接 404 —— 而节点收录、订阅构建全都正常，
// 界面上看不出任何异常。
//
// 修法两条缺一不可：
//
//  1. 过滤 deleted_at IS NULL，否则软删除的行会参与竞争；
//
//  2. 显式 ORDER BY，让「选哪一条」是确定的而不是碰运气。
//
//  3. 只看面板自建的行（order_id 为空）。2026-11-03：迁移 009 去掉了单例索引后，
//     面板自建行被删、或商城订单早于首次打开订阅中心时，「最早一行」会是某个买家的订阅 ——
//     换新链接会让买家的链接 404，停用会把他从节点上停掉。
//
//  4. 2026-11-06：「没有 order_id」还不够 —— 订阅中心手工开给客户的订阅同样没有 order_id。改认显式标记
//     is_panel_merged（迁移 057，只有 loadOrCreateSubscriptionToken 会写 true）。
//
// 排序用 created_at ASC, id ASC：面板自建的这条是首次访问订阅中心时
// 插入的最早一行，取最早即取它；加 id 是为了在 created_at 相同时
// （同一事务里插入多行的极端情况）仍然稳定。
func (s *Server) readSubscriptionToken(ctx context.Context) (*subscriptionTokenState, error) {
	var state subscriptionTokenState
	err := s.db.QueryRow(ctx, `
SELECT token, label, enabled, created_at, last_accessed_at, access_count
FROM subscription_tokens
WHERE deleted_at IS NULL AND is_panel_merged
ORDER BY created_at ASC, id ASC
LIMIT 1`).
		Scan(&state.Token, &state.Label, &state.Enabled, &state.CreatedAt, &state.LastAccessedAt, &state.AccessCount)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// subscriptionPublicURL reconstructs the externally visible URL from the request.
// Behind the bundled Caddy the Host header is preserved and X-Forwarded-Proto is
// set, so this yields the address the operator should actually paste into a
// client. Only the path is authoritative on the server side.
func subscriptionPublicURL(r *http.Request, token string) string {
	if base := subscriptionBaseURL(r); base != "" {
		return base + "/sub/" + token
	}
	return "/sub/" + token
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GET /api/subscription — current link, plus what is included and excluded.
func (s *Server) subscriptionState(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	state, err := s.loadOrCreateSubscriptionToken(r.Context(), claims.Subject)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	build, err := s.subscriptionBuildFor(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-06：「包含的节点」与 /sub 实际下发同源（subscriptionNodesFor：开户、健康闸门、客户端参数读取）。
	// 原来直接用全站构建 build.Nodes：页面上 N 个，链接里实际更少，少的那几台也不会出现在「未包含」里。
	rec, err := s.loadSubscriptionByToken(r.Context(), state.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "合并订阅已被其他操作修改，请刷新后重试"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	included := s.subscriptionNodesFor(r.Context(), build.Nodes, rec)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":            state.Token,
		"url":              subscriptionPublicURL(r, state.Token),
		"path":             "/sub/" + state.Token,
		"label":            state.Label,
		"enabled":          state.Enabled,
		"created_at":       state.CreatedAt,
		"last_accessed_at": state.LastAccessedAt,
		"access_count":     state.AccessCount,
		"formats":          []string{subscriptionFormatClash, subscriptionFormatSingBox, subscriptionFormatURI},
		"included":         redactSubscriptionNodes(included),
		"excluded":         build.Excluded,
	})
}

// GET /api/subscription/preview?format=… — render the payload for the operator
// without needing to hand the token to a client.
func (s *Server) subscriptionPreview(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	format := negotiateSubscriptionFormat(r)
	build, err := s.subscriptionBuildFor(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-06：预览与 /sub/<合并订阅> 实际下发同源（见 subscriptionState）。原来直接渲染全站构建：
	// 预览里有的节点，客户端拉到的配置里可能没有（未开号、被健康闸门下线、读不到客户端参数）。
	state, err := s.loadOrCreateSubscriptionToken(r.Context(), claims.Subject)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	rec, err := s.loadSubscriptionByToken(r.Context(), state.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "合并订阅已被其他操作修改，请刷新后重试"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	nodes := s.subscriptionNodesFor(r.Context(), build.Nodes, rec)
	body, contentType, err := renderSubscription(nodes, format)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成订阅失败: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// POST /api/subscription/rotate — invalidate the old link and publish a new one.
func (s *Server) rotateSubscriptionToken(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "换新订阅链接需要 X-Confirm-Dangerous: yes"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	state, err := s.loadOrCreateSubscriptionToken(r.Context(), claims.Subject)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	rec, err := s.loadSubscriptionByToken(r.Context(), state.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "订阅已被其他操作修改，请刷新后重试"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	r, release, ok := s.lockSubscriptionRequest(w, r, rec)
	if !ok {
		return
	}
	defer release()
	token, err := newSubscriptionToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成令牌失败"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE subscription_tokens SET token=$1, last_accessed_at=NULL, access_count=0, active_ips='[]'::jsonb WHERE id=$2::uuid AND token=$3 AND deleted_at IS NULL`, token, rec.ID, rec.Token)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "订阅已被其他操作修改，请刷新后重试"})
		return
	}
	s.audit(r.Context(), r, "rotate_subscription_token", "subscription_token", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"url":   subscriptionPublicURL(r, token),
		"path":  "/sub/" + token,
	})
}

// PATCH /api/subscription — enable/disable the link or rename it.
func (s *Server) updateSubscription(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Enabled *bool   `json:"enabled"`
		Label   *string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	state, err := s.loadOrCreateSubscriptionToken(r.Context(), claims.Subject)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// Resolve the stable identity before locking; all mutable state is re-read under the lock.
	rec, err := s.loadSubscriptionByToken(r.Context(), state.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "订阅已被其他操作修改，请刷新后重试"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	r, release, ok := s.lockSubscriptionRequest(w, r, rec)
	if !ok {
		return
	}
	defer release()
	enabled, label := rec.Enabled, rec.Label
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.Label != nil {
		label = strings.TrimSpace(*input.Label)
		if label == "" {
			label = "全部节点"
		}
		if utf8.RuneCountInString(label) > 64 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "标签最长 64 个字符"})
			return
		}
	}
	sets, args := []string{}, []any{}
	if input.Enabled != nil {
		args = append(args, enabled)
		sets = append(sets, fmt.Sprintf("enabled=$%d", len(args)))
	}
	if input.Label != nil {
		args = append(args, label)
		sets = append(sets, fmt.Sprintf("label=$%d", len(args)))
	}
	if len(sets) > 0 {
		args = append(args, rec.ID)
		tag, err := s.db.Exec(r.Context(), "UPDATE subscription_tokens SET "+strings.Join(sets, ", ")+fmt.Sprintf(" WHERE id=$%d::uuid AND deleted_at IS NULL", len(args)), args...)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		if tag.RowsAffected() != 1 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "订阅已被其他操作修改，请刷新后重试"})
			return
		}
	}
	rec.Enabled, rec.Label = enabled, label
	auditCtx, stopAudit := subscriptionCompensationContext(r.Context())
	s.audit(auditCtx, r, "update_subscription", "subscription_token", "", map[string]any{"enabled": enabled, "label": label})
	stopAudit()
	resp := map[string]any{"enabled": enabled, "label": label}
	// Explicit same-value toggles retry unconfirmed node actions; label-only edits remain cheap.
	if input.Enabled != nil {
		active := enabled && !rec.expired(time.Now())
		outcome := s.applySubscriptionNodeStatus(r.Context(), rec, active)
		resp["node_state_message"] = describeSubscriptionStateOutcome(outcome, active)
		resp["node_state_ok"] = outcome.allOK()
		resp["node_state_failed"] = outcome.Failed
		if active {
			s.cancelPendingNodeAction(r.Context(), rec.ID)
			if len(rec.nodeAccounts) == 0 || (!outcome.allOK() && !outcome.onlyGone()) {
				if err := s.queueSubscriptionSyncBounded(r.Context(), rec.ID); err != nil {
					resp["node_state_message"] = fmt.Sprint(resp["node_state_message"]) + "。后台自动重试登记失败，请稍后重新启用一次"
				}
			}
		} else if !outcome.allOK() && !outcome.onlyGone() {
			if err := s.queuePendingNodeAction(r.Context(), rec.ID, pendingActionDisable, false, "停用合并订阅时部分节点未确认"); err != nil {
				resp["node_state_message"] = fmt.Sprint(resp["node_state_message"]) + "。后台自动重试登记失败，请稍后重新停用一次"
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// subscriptionFeed serves the public subscription itself.
//
// Registered on both /sub/{token} (the tidy external path, forwarded by Caddy)
// and /api/sub/{token} (so it still works when the API is reached directly).
// There is no JWT here by design: a subscription client cannot send one. The
// token in the path IS the credential.
func (s *Server) subscriptionFeed(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	// A malformed token is answered exactly like an unknown one: no oracle for
	// guessing, and no different timing path worth probing.
	if !plausibleSubscriptionToken(token) {
		subscriptionNotFound(w)
		return
	}
	if !s.enforcePublicLimit(w, "sub:"+token) {
		return
	}

	rec, err := s.loadSubscriptionByToken(r.Context(), token)
	if err != nil {
		// 2026-11-06：只有查无此行才是 404。原来任何错误都回 404：数据库抖一下，客户端与交付页
		// 都被告知「订阅不存在」—— 交付页随即把付费买家的页面换成「订阅已被回收」。
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Warn("subscription: 读取订阅失败", "error", err)
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取订阅，请稍后再试"})
			return
		}
		subscriptionNotFound(w)
		return
	}
	// Past the grace window the row is gone for good, and this endpoint must say
	// so the same way the delivery view does (404, not 403). Otherwise a client
	// holding a dead URL keeps retrying forever on a 403 it reads as "temporary",
	// while the delivery page it surely also opens says the subscription no
	// longer exists. One story, two endpoints.
	//
	// `!Before` (>=) rather than `After` (>) on purpose: with the zero grace
	// window the reclaim point is `expires_at` itself, and `expired()` already
	// treats `now == expires_at` as expired. Using `After` would leave one
	// instant where the row is considered expired but not yet reclaimed, and the
	// caller would get a 403 for a subscription that is about to vanish.
	if rec.expired(time.Now()) && rec.AutoDelete && rec.ExpiresAt != nil &&
		!time.Now().Before(rec.ExpiresAt.Add(subscriptionExpiryGrace)) {
		purgeErr := s.purgeSubscriptionOutcome(r.Context(), rec)
		if errors.Is(purgeErr, errRenewedDuringPurge) {
			fresh, rerr := s.loadSubscriptionByID(r.Context(), rec.ID)
			if rerr != nil {
				if errors.Is(rerr, pgx.ErrNoRows) {
					subscriptionNotFound(w)
					return
				}
				s.logger.Warn("subscription: 续费竞争后重读订阅失败", "subscription", shortID(rec.ID), "error", rerr)
				w.Header().Set("Retry-After", "30")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法读取订阅，请稍后再试"})
				return
			}
			rec = fresh
		} else {
			subscriptionNotFound(w)
			return
		}
	}
	// ⚠️ 2026-10-04 补的关键缺口：**过期就必须收回节点账号**，
	// 这与 auto_delete（是否删记录）是两件事，不能绑在一起。
	//
	// 上面的 purge 分支带了 `rec.AutoDelete` 条件，于是
	// **auto_delete=false 的订阅即使过期，节点账号也不会被收回** ——
	// 买家手里那份已下载的配置照常能用（实测：到期后 182 秒仍 204）。
	//
	// 这里补一条**幂等**的即时收回：只要已过期且还有未收回的账号，就地收回。
	//   * 判据用 node_accounts 非空 ⇒ 反复拉取不会重复动作；
	//   * 收回失败**不阻断**响应（下面照常按"已过期"拒绝服务），
	//     留给 sweeper 下一轮重试 —— 拒绝服务本身已经达成"不能再用"。
	//   * 放在 access 检查**之前**：无论这次拉取是否被允许，
	//     过期这个事实都该立刻反映到节点上。
	if rec.expired(time.Now()) && rec.ExpiresAt != nil &&
		len(rec.nodeAccounts) > 0 &&
		!time.Now().Before(rec.ExpiresAt.Add(subscriptionExpiryGrace)) {
		// 放到后台做：节点连不上时同步回收要等最长 10 秒并占一把订阅锁，
		// 客户端每次轮询都会卡住。拒绝服务在下面照常生效，回收由后台与 sweeper 保证。
		s.revokeExpiredInBackground(rec)
	}
	// Expiry and the device quota are enforced here rather than only by the
	// sweeper: this endpoint is polled continuously, so it is the authoritative
	// gate. A refused pull never touches the subscription build.
	if ok, reason := s.enforceSubscriptionAccess(r.Context(), rec, s.subscriptionClientIP(r)); !ok {
		writeSubscriptionRefusal(w, reason)
		return
	}

	build, err := s.subscriptionBuildFor(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "暂时无法生成订阅，请稍后再试"})
		return
	}

	// JIT Provisioning (即时补齐账号):
	// 如果管理员在买家下单后向控制台新增了节点，买家拉取订阅时绝不能下发共享主密码！
	// 检查当前节点列表中是否有尚未为该买家开立账号的节点，并在后台即时补齐开户并落库。
	//
	// ⚠️ 2026-10-30：这条路径是**买家同步等待**的。原来直接传 r.Context()，
	// 而 synchronizeSubscription 内部给开户留了 50 秒预算 —— 新节点刚上架、
	// 或某台节点响应慢时，买家拉一次订阅会卡近一分钟，客户端可能先超时。
	// 现在只给 8 秒：够完成正常的一次开户（节点往返几十毫秒到 2 秒），
	// 超时的部分由 synchronizeSubscription 自己登记的同步任务在后台续跑
	// （它在开头 queueSubscriptionSync、结束时按成败写回退）。
	// 拉取本身不受影响：没开号的节点本来就不会出现在订阅里。
	if s.needsAccountProvisioning(r.Context(), build.Nodes, rec) {
		provCtx, cancelProv := context.WithTimeout(r.Context(), 8*time.Second)
		s.provisionAndPersist(provCtx, rec)
		cancelProv()
	}

	format := negotiateSubscriptionFormat(r)
	// Layer this buyer's own credentials over the shared fleet build before
	// rendering — the config must carry a password whose account we can revoke.
	served, servedErr := s.subscriptionNodesForChecked(r.Context(), build.Nodes, rec, build.Excluded)
	if servedErr != nil {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": servedErr.Error()})
		return
	}
	body, contentType, err := renderSubscriptionWithInfo(served, format, subscriptionInfoLines(rec, time.Now()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成订阅失败"})
		return
	}

	// Usage counters, not an audit row: clients poll on a schedule and writing a
	// row per pull would drown the audit log in noise while telling us nothing.
	if _, err := s.db.Exec(r.Context(), `UPDATE subscription_tokens SET last_accessed_at=now(), access_count=access_count+1 WHERE token=$1`, token); err != nil {
		s.logger.Warn("subscription: update counters failed", "error", err)
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	// mihomo/Clash honour this to re-pull the profile periodically.
	w.Header().Set("profile-update-interval", "12")
	setSubscriptionUserinfo(w, rec)
	// 订阅显示名称：Clash Verge / Stash / Hiddify 读 profile-title，部分客户端用文件名当配置名。
	name := subscriptionDisplayName()
	w.Header().Set("profile-title", "base64:"+base64.StdEncoding.EncodeToString([]byte(name)))
	ext := map[string]string{subscriptionFormatClash: ".yaml", subscriptionFormatSingBox: ".json", subscriptionFormatURI: ".txt"}[format]
	if ext != "" {
		fallback := map[string]string{subscriptionFormatClash: "clash.yaml", subscriptionFormatSingBox: "sing-box.json", subscriptionFormatURI: "hy2-subscription.txt"}[format]
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, pctEncode(name+ext)))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// subscriptionClientIP resolves the address that counts against a subscription's
// device limit.
//
// Behind the bundled Caddy every request arrives from the proxy, so RemoteAddr
// alone would collapse the whole internet into one address and make a device
// limit meaningless. The panel therefore uses the configured trusted-proxy policy.
//
// 取哪一段：与登录/审计口径统一为 requestClientIP 的**最左段**。
//
// 这里原本取最右段，理由是「最左段可被客户端伪造」。在多层反代环境下
// 实测后纠正了这个判断：**只要链路最外层反代用覆盖语义设置该头，
// 客户端带进来的值在最外层就被丢弃了，最左段反而是唯一可信的真实来源**；
// 而多层链路里最右段会退化成内网地址（实测取到 172.19.0.2），
// 导致所有订阅者被算成同一个设备、设备限制彻底失效。
//
// 两个方向都会坏，但坏法不同：取最右段是**静默失效**（限制形同虚设，
// 但看着一切正常）；取最左段在最外层非覆盖时是**可伪造**。
// 本项目的两类部署形态（单机直连 / 多层反代）都保证最外层覆盖，
// 因此取最左段是正确选择。新增反代层时必须沿用覆盖语义。
//
// 2026-10-01：抽成 requestClientIP 后这里只做转发，
// 保证订阅限流与登录限流永远不可能取到不同的地址 ——
// 两处各写一份实现时，很容易只改一处（本项目已出现过一次）。
// subscriptionClientIP resolves the address that counts against a subscription's
// device limit.
func (s *Server) subscriptionClientIP(r *http.Request) string {
	// User-Agent is caller-controlled and is also shared by legitimate clients
	// (for example okhttp). Always resolve the address: limited subscriptions
	// reject an unknown IP, so classifying these clients as crawlers blocks pulls.
	return s.requestClientIP(r)
}

// plausibleSubscriptionToken 是公开订阅端点（/sub 与交付页）共用的令牌形状校验；不通过的一律按「不存在」回 404。
//
// 2026-11-12：加上 NUL 与非法 UTF-8。原来只查长度与空格、制表、斜杠，两个端点各写一遍：
// 路径里带 %00 或 %ff 的令牌通过校验后进数据库，被以「编码非法」拒绝 —— 那不是「查无此行」，
// 于是落进为数据库抖动准备的 503 分支并写一条 WARN，监控上看像数据库故障。
// 不收紧成固定字符集：存在非本面板生成的存量令牌（商城接管的订阅只校验长度）。
func plausibleSubscriptionToken(token string) bool {
	return len(token) >= 20 && len(token) <= 128 && !strings.ContainsAny(token, " \t/\x00") && utf8.ValidString(token)
}

// subscriptionNotFound answers without revealing whether the token exists.
func subscriptionNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("subscription not found\n"))
}
