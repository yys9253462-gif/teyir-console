package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nodeops/server/internal/auth"
	"nodeops/server/internal/executor"
	"nodeops/server/internal/geo"
	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/secrets"
	"nodeops/server/internal/speedtest"
	"nodeops/server/internal/sshclient"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// shutdownChan 是服务关闭信号的别名。
// 单独的别名是为了让 funcBlock 这类按 { 配对函数体的测试工具能正确解析
// 参数里带 chan struct{} 的函数签名（2026-10-30）。
type shutdownChan = <-chan struct{}

type Server struct {
	telegramControl             telegramControlState
	requestCanceled             atomic.Uint64
	requestDeadline             atomic.Uint64
	databaseCanceled            atomic.Uint64
	databaseDeadline            atomic.Uint64
	viewerRestrictSubscriptions bool
	trustedProxyCIDRs           []*net.IPNet
	// shutdownDone 在服务退出时关闭。告警通知等后台 goroutine 用它
	// 感知"正在关闭"，把按静默期睡眠的等待提前结束 —— 否则进程退出时
	// 正在 sleep 的告警会无声丢失（2026-10-30）。
	// 不配置时为 nil，对应 select 永不阻塞，行为与之前一致。
	shutdownDone     shutdownChan
	logger           *slog.Logger
	allowedOrigins   map[string]struct{}
	db               *pgxpool.Pool
	mux              *http.ServeMux
	signer           *auth.Signer
	sessionUser      func(context.Context, string) (string, string, int64, error) // 测试注入；生产始终读当前用户
	box              *secrets.Box
	collectorSeconds int
	executorSeconds  int
	// 配额按「请求会不会改变状态」和「需不需要认证」分成四桶。
	//
	// 这里原来只有一个 rateLimiter（10s/20 次），认证后的读写共用。问题在
	// 实测里暴露得很直白：节点运维页签每刷新一次就是一条 portal-status，
	// 而它内含七路并发探测；连点几下、再开一个标签页，就把写操作要用的
	// 配额一起用掉了。结果是「切换 Reality」这类真正有意义的操作返回 429，
	// 而它自己一次都还没发过 —— 操作者看到的提示也说不清是谁把配额吃掉的。
	//
	// 反过来，若为了照顾读操作把那个桶放大，写操作的保护就没了。所以拆开：
	//
	//	readLimiter   认证后的只读端点。宽。探测是幂等的，用户点得快是常态。
	//	writeLimiter  认证后的写端点。严。每一条都会下发到节点，误触代价真实。
	//	publicLimiter 无需认证的订阅端点（按 token 或交付会话计）。严。
	//	              与上面两桶分开还顺带解掉一个耦合：公开订阅的流量
	//	              不该影响管理端写操作的可用性。
	//	loginLimiter  登录，按 IP。最严。
	//
	// 数值选择见 New() 里的注释。
	readLimiter   *slidingLimiter
	writeLimiter  *slidingLimiter
	publicLimiter *slidingLimiter
	loginLimiter  *slidingLimiter
	// nodeWriteLocks 让「同一个节点」的写操作串行执行。
	//
	// 起因是一轮并发实测：4 路并发向同一节点下发写操作，两手拿到 502，
	// 日志里是 `unexpected EOF` 与 `connection refused` —— 节点侧的
	// portal 是单线程 Python http.server，并发连接会把它打崩，随后
	// 管理页面短暂失联。
	//
	// 那是别人机器上的程序，我们改不了；但面板完全可以不把并发打过去。
	// 写操作本来就是低频、要求明确的动作，串行化代价极小，收益是
	// 节点门户不再被自己人打挂。
	nodeWriteLocks sync.Map // map[nodeID]chan struct{}（容量 1 的信号量，可随 ctx 放弃等待）
	// nodeReadGates 限制「同一个节点」上并发的只读探测数 —— 上面那把写锁
	// 的另一半。写操作串行化之后，读侧却一直敞着：一次 portal-status 就
	// 并发打 7 路过去，两个标签页同时刷新就是 14 路。单线程的
	// http.server 正是这么被打出 unexpected EOF 的。
	//
	// 值与用法见 lockNodeRead。
	nodeReadGates sync.Map // map[nodeID]chan struct{}
	terminalGate  terminalSessionGate

	// geo 负责「从 IP 推断地区与 VPS 商家」，用于新增节点/面板时自动命名。
	//
	// 它允许为 nil（未配置时）：识别只是锦上添花，任何一条自动识别失败的
	// 路径都必须能退回「字段留空、由操作者手填」，绝不能让加节点这件事
	// 因为第三方 geo 服务抖动而失败。
	geo *geo.Resolver

	// purity 负责「这个 IP 干不干净」（连接类型/双ISP/风险评分），
	// 用于节点列表与详情页展示。
	//
	// 与 geo 同样的容错姿态：允许为 nil（未配置离线库与 API 密钥时），
	// 此时纯净度端点返回明确的「未启用」而不是报错，节点其余功能不受影响。
	purity     *ippurity.Resolver
	purityKeys *ippurity.KeyStore

	// prober 负责大陆三网测速（SSH 到节点、在节点本机跑探针脚本）。
	//
	// 允许为 nil：没有凭据加密密钥时解不开节点 SSH 凭据，测速整体不可用，
	// 但面板其余功能照常 —— 姿态与 geo 一致，附加能力不该拖垮主功能。
	prober *speedtest.Prober
	// alertNotifier 负责告警外发的节流（见 alertnotify.go）。
	// 挂在 Server 上而不是包级变量：包级状态会让测试互相污染。
	alertNotifier *alertNotifier
}

type Options struct {
	ViewerRestrictSubscriptions bool
	TrustedProxyCIDRs           []string
	JWTSecret                   string
	JWTTTL                      time.Duration
	CredentialBox               *secrets.Box
	CollectorSeconds            int
	ExecutorSeconds             int
	SeedAdminEmail              string
	SeedAdminPassword           string
	// Geo 是地区/商家识别器。为 nil 时所有自动识别静默跳过，
	// 界面上仍可手工填 region/provider（与加这个功能之前的行为一致）。
	Geo *geo.Resolver
	// Purity 是 IP 纯净度检测器。为 nil 时纯净度端点返回「未启用」，
	// 节点其余功能不受影响（姿态与 Geo 一致）。
	Purity *ippurity.Resolver
	// PurityKeys 是 IP 检测的第三方密钥，可在系统设置里热更新（见 puritykeys.go）。
	PurityKeys *ippurity.KeyStore
	// Prober 是三网测速执行器。为 nil 时测速端点返回 503 并说明原因，
	// 而不是静默失败。
	Prober *speedtest.Prober
}

var safeUnitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$`)

func New(logger *slog.Logger, allowedOrigins []string, db *pgxpool.Pool, opts Options) *Server {
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, value := range allowedOrigins {
		originSet[value] = struct{}{}
	}
	s := &Server{
		viewerRestrictSubscriptions: opts.ViewerRestrictSubscriptions,
		logger:                      logger,
		allowedOrigins:              originSet,
		db:                          db,
		mux:                         http.NewServeMux(),
		signer:                      auth.NewSigner(opts.JWTSecret, opts.JWTTTL),
		box:                         opts.CredentialBox,
		collectorSeconds:            opts.CollectorSeconds,
		executorSeconds:             opts.ExecutorSeconds,
		geo:                         opts.Geo,
		prober:                      opts.Prober,
		purity:                      opts.Purity,
		purityKeys:                  opts.PurityKeys,
		alertNotifier:               newAlertNotifier(),
		// 读：60 次 / 10s。一个人的手速上限大约 1-2 次请求/秒，60 次意味着
		// 「正常点」永远碰不到这个上限 —— 它挡的只是脚本或失控的轮询。
		// 注意它管的是「面板 → 服务端」这一段；「服务端 → 节点门户」那一段
		// 由 nodeReadGates 单独限并发，两者解决的不是同一件事。
		readLimiter: newSlidingLimiter(10*time.Second, 60),
		// 写：20 次 / 10s，沿用改造前的数值。每一条写都会真的下发到节点，
		// 而且节点侧只有一个单线程门户，防误触的价值就在这里。
		writeLimiter: newSlidingLimiter(10*time.Second, 20),
		// 公开：20 次 / 10s，按订阅 token 计。这些端点不需要认证，
		// 只能靠 token 的熵来兜底，所以从严。
		publicLimiter: newSlidingLimiter(10*time.Second, 20),
		// 登录：10 次 / 5 分钟，按 IP。最严，且与上面三桶完全无关 ——
		// 管理端被限流不该顺带把登录也锁死，反之亦然。
		loginLimiter: newSlidingLimiter(5*time.Minute, 10),
	}
	for _, raw := range opts.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			panic("invalid TRUSTED_PROXY_CIDRS entry")
		}
		s.trustedProxyCIDRs = append(s.trustedProxyCIDRs, network)
	}
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/login/config", s.loginConfig)
	s.mux.HandleFunc("POST /api/login", s.login)
	s.panelRoute("GET /api/servers", s.authenticated(s.servers))
	s.panelRoute("POST /api/servers", s.authenticated(s.requireAdmin(s.createServer)))
	s.panelRoute("GET /api/servers/suggest-name", s.authenticated(s.suggestServerName))
	s.panelRoute("GET /api/servers/{id}", s.authenticated(s.serverDetail))
	s.panelRoute("PATCH /api/servers/{id}", s.authenticated(s.updateServer))
	s.panelRoute("DELETE /api/servers/{id}", s.authenticated(s.requireAdmin(s.deleteServer)))
	s.panelRoute("GET /api/servers/{id}/metrics", s.authenticated(s.serverMetrics))
	// Batched metrics for the overview sparklines: one request instead of one per
	// host, which matters because the dashboard polls every 15 seconds.
	s.panelRoute("GET /api/metrics/recent", s.authenticated(s.recentMetrics))
	s.panelRoute("GET /api/nodes", s.authenticated(s.nodes))
	// 节点能力总览（2026-10-04 加）。
	//
	// ⚠️ 必须注册在任何 `/api/nodes/{id}` 路由**之前**：
	// Go ServeMux 的 {id} 占位符会匹配不含 "/" 的任意字符串，
	// "capabilities" 会被当成某个节点 id 而落到 detail handler 上。
	// 2026-10-03 在 /api/sub-ids 上踩过同类问题（当时靠换路径规避）。
	s.panelRoute("GET /api/nodes/capabilities", s.authenticated(s.nodeCapabilities))
	// 节点分类：既是分组，也是商城圈定节点范围的依据。
	s.panelRoute("GET /api/categories", s.authenticated(s.categoriesList))
	s.panelRoute("POST /api/categories", s.authenticated(s.requireAdmin(s.categoryCreate)))
	s.panelRoute("PATCH /api/categories/{id}", s.authenticated(s.requireAdmin(s.categoryUpdate)))
	s.panelRoute("DELETE /api/categories/{id}", s.authenticated(s.requireAdmin(s.categoryDelete)))
	s.panelRoute("PUT /api/nodes/{id}/categories", s.authenticated(s.requireAdmin(s.nodeSetCategories)))
	s.panelRoute("POST /api/nodes", s.authenticated(s.requireAdmin(s.createNode)))
	s.panelRoute("DELETE /api/nodes/{id}", s.authenticated(s.requireAdmin(s.deleteNode)))
	s.panelRoute("POST /api/nodes/preview", s.authenticated(s.previewNodeAPI))
	// 2026-11-06：与解绑一致，仅管理员。bindNodeAPI 会把调用方给的 api_url / api_key 落库，
	// 之后 provisionSubscriptionAccounts 会把买家的 sub_* 密码 POST 过去 —— operator 能把自己的「门户」
	// 绑成节点，就等于把这些凭据引到自己的机器上（与 updateServer 改地址同一种威胁）。
	s.panelRoute("POST /api/nodes/{id}/api", s.authenticated(s.requireAdmin(s.bindNodeAPI)))
	s.panelRoute("GET /api/nodes/{id}/api", s.authenticated(s.refreshNodeAPI))
	s.panelRoute("DELETE /api/nodes/{id}/api", s.authenticated(s.requireAdmin(s.unbindNodeAPI)))
	// Node-side control surface. The first block rides the Bearer api_key the
	// panel already stores, so it needs no extra credential — it is simply the
	// rest of what that key already unlocks.
	s.panelRoute("GET /api/nodes/{id}/overview", s.authenticated(s.portalOverview))
	s.panelRoute("GET /api/nodes/{id}/users", s.authenticated(s.portalUsers))
	s.panelRoute("POST /api/nodes/{id}/users", s.authenticated(s.portalCreateUser))
	s.panelRoute("POST /api/nodes/{id}/users/{uid}/renew", s.authenticated(s.portalRenewUser))
	s.panelRoute("POST /api/nodes/{id}/users/{uid}/status", s.authenticated(s.portalSetUserStatus))
	s.panelRoute("DELETE /api/nodes/{id}/users/{uid}", s.authenticated(s.requireAdmin(s.portalDeleteUser)))
	s.panelRoute("GET /api/nodes/{id}/proxy-services", s.authenticated(s.portalProxyServices))
	// 2026-11-08：在节点上开 SOCKS5/HTTP 出口与 portal-action 的 manage_proxy 同级，删除也已限管理员；原来创建对 operator 开放。
	s.panelRoute("POST /api/nodes/{id}/proxy-services", s.authenticated(s.requireAdmin(s.portalCreateProxyService)))
	s.panelRoute("DELETE /api/nodes/{id}/proxy-services/{pid}", s.authenticated(s.requireAdmin(s.portalDeleteProxyService)))
	// 跨节点聚合：一次返所有 in_store 节点上的 sub_* 账号（按 user_id → [node_name] 索引）。
	//
	// 用途：store-e2e / 其他端到端校验脚本 —— 之前要逐节点调 GET /api/nodes/{id}/users
	// （5 节点 × 2 次/轮 × 5 轮 = 50 次），聚合后降为 5 轮 × 1 次 = 5 次。
	//
	// 路径不在 /api/nodes/{id} 下是有意的：避免 Go ServeMux 的 {id} 占位符
	// 把 "sub-ids" 解释成某个节点的 id（2026-10-03 加）。
	// 2026-11-06：与 handler 注释里写的「admin-only」对齐。这个端点回的是各节点 /users/list 的全集
	// （每个 sub_* 账号在哪台节点上），原来只套了 authenticated —— 运维与只读账号都能拉走完整清单。
	s.panelRoute("GET /api/sub-ids", s.authenticated(s.requireAdmin(s.subIdsAggregate)))
	// IP 纯净度：GET 只读库（不联网，节点列表页按节点拉），refresh 才出网检测。
	// 「读缓存 / 显式检测」分开是为了不把第三方免费配额刷爆 —— 见 ippurity.go。
	s.panelRoute("GET /api/nodes/{id}/ip-purity", s.authenticated(s.nodeIPPurity))
	// 全量快照：专用「IP 纯净度」页面一次拉所有节点（只读库，不联网）。
	s.panelRoute("GET /api/ip-purity", s.authenticated(s.listIPPurity))
	s.panelRoute("POST /api/nodes/{id}/ip-purity/refresh", s.authenticated(s.refreshNodeIPPurity))
	// 批量检测是重动作（逐节点出网），归管理员。
	s.panelRoute("POST /api/ip-purity/refresh-all", s.authenticated(s.requireAdmin(s.refreshAllIPPurity)))
	s.panelRoute("POST /api/ip-probe", s.authenticated(s.requireAdmin(s.probeIP)))
	// 流媒体 / AI 解锁检测：在节点本机经 portal /api/v1/unlock-check 跑（见 unlock.go）。
	// 批量由前端逐台驱动 —— 一次全量检测会超过反代的 60s 读超时。
	s.panelRoute("GET /api/unlock", s.authenticated(s.listUnlock))
	s.panelRoute("POST /api/nodes/{id}/unlock/refresh", s.authenticated(s.refreshNodeUnlock))
	// The second block rides the portal's *web session* credential (HTTP Basic),
	// which reaches everything the first block cannot: version/upgrade, BBR,
	// Reality, WARP, AmneziaWG, gost install, and reboot. It is a strictly
	// larger capability set, so these handlers require portal_basic_user to be
	// bound (migration 013) and refuse with a "please bind it" message when not.
	s.panelRoute("GET /api/nodes/{id}/portal-status", s.authenticated(s.portalNodeStatus))
	s.panelRoute("POST /api/nodes/{id}/portal-action", s.authenticated(s.portalNodeAction))
	// 节点出口设置：读列表与探测出口 IP；写操作走 portal-action 的 manage_outbounds。
	s.panelRoute("GET /api/nodes/{id}/outbounds", s.authenticated(s.nodeOutbounds))
	// 一键安装 hysteria2-installer 并自动接入（见 installhy2.go）。会在主机上执行安装脚本，归管理员。
	s.panelRoute("POST /api/servers/{id}/install-hy2", s.authenticated(s.requireAdmin(s.startInstallHy2)))
	// 主机实用功能（见 hostextras.go）：连接测试、到期与成本、换 IP 同步、资源趋势。
	s.panelRoute("POST /api/servers/{id}/test-ssh", s.authenticated(s.requireAdmin(s.testServerSSH)))
	// 登录安全、定时巡检、商城签名（见 loginsecurity.go / scheduler.go / storesign.go）。
	s.panelRoute("GET /api/auth/logins", s.authenticated(s.myLogins))
	s.panelRoute("GET /api/schedules", s.authenticated(s.listSchedules))
	s.panelRoute("PUT /api/schedules", s.authenticated(s.requireAdmin(s.setSchedule)))
	s.panelRoute("POST /api/schedules/{key}/run", s.authenticated(s.requireAdmin(s.runScheduleNow)))
	s.panelRoute("GET /api/settings/store-signature", s.authenticated(s.requireAdmin(s.getStoreSignature)))
	s.panelRoute("PUT /api/settings/store-signature", s.authenticated(s.requireAdmin(s.setStoreSignature)))
	s.panelRoute("GET /api/settings/delivery-domain", s.authenticated(s.requireAdmin(s.getDeliveryDomain)))
	s.panelRoute("GET /api/cloudflare/zones", s.authenticated(s.requireAdmin(s.cloudflareZones)))
	s.panelRoute("PUT /api/settings/delivery-domain", s.authenticated(s.requireAdmin(s.setDeliveryDomain)))
	// 故障节点自动下线、Telegram、SSH 指纹变化（见 healthgate.go / telegram.go）。
	s.panelRoute("GET /api/nodes/auto-hide", s.authenticated(s.getAutoHide))
	s.panelRoute("PUT /api/nodes/auto-hide", s.authenticated(s.requireAdmin(s.setAutoHide)))
	s.panelRoute("GET /api/settings/telegram", s.authenticated(s.requireAdmin(s.getTelegramSettings)))
	s.panelRoute("PUT /api/settings/telegram", s.authenticated(s.requireAdmin(s.setTelegramSettings)))
	s.panelRoute("POST /api/settings/telegram/test", s.authenticated(s.requireAdmin(s.testTelegram)))
	s.panelRoute("POST /api/servers/{id}/fingerprint/accept", s.authenticated(s.requireAdmin(s.acceptServerFingerprint)))
	// 第一梯队：节点迁移、被墙检测、利润报表、账号共享检测。
	s.panelRoute("POST /api/nodes/{id}/migrate", s.authenticated(s.requireAdmin(s.startMigrateNode)))
	s.panelRoute("GET /api/block-checks", s.authenticated(s.listBlockChecks))
	s.panelRoute("POST /api/block-checks/run", s.authenticated(s.requireAdmin(s.runBlockChecksNow)))
	s.panelRoute("PUT /api/settings/cn-probe", s.authenticated(s.requireAdmin(s.setCNProbe)))
	s.panelRoute("GET /api/reports/profit", s.authenticated(s.requireAdmin(s.profitReport)))
	s.panelRoute("PUT /api/subscriptions/{id}/revenue", s.authenticated(s.requireAdmin(s.setSubscriptionRevenue)))
	s.panelRoute("GET /api/subscriptions/sharing", s.authenticated(s.listSharing))
	s.panelRoute("GET /api/servers/billing", s.authenticated(s.listServerBilling))
	s.panelRoute("PUT /api/servers/{id}/billing", s.authenticated(s.requireAdmin(s.putServerBilling)))
	s.panelRoute("POST /api/servers/{id}/change-ip", s.authenticated(s.requireAdmin(s.changeServerIP)))
	s.panelRoute("GET /api/servers/{id}/metrics/trend", s.authenticated(s.serverMetricsTrend))
	s.panelRoute("GET /api/install-jobs/{id}", s.authenticated(s.requireAdmin(s.getInstallJob)))
	s.panelRoute("GET /api/nodes/{id}/outbounds/probe", s.authenticated(s.nodeOutboundProbe))
	// 批量升级门户 = 在最多 20 台节点上以 root 覆盖并重启 portal，仅限管理员（2026-11-03）。
	s.panelRoute("POST /api/portal-update-batches", s.authenticated(s.requireAdmin(s.startPortalBatch)))
	s.panelRoute("GET /api/portal-update-batches/{id}", s.authenticated(s.getPortalBatch))
	s.panelRoute("GET /api/portal-update-batches", s.authenticated(s.listPortalBatches))
	s.panelRoute("GET /api/portal-update-status", s.authenticated(s.getPortalUpdateStatus))
	s.panelRoute("POST /api/portal-update-batches/{id}/cancel", s.authenticated(s.requireAdmin(s.cancelPortalBatch)))
	// Merged subscription. The feed is registered on two paths and is the only
	// route in this server with no JWT: a subscription client cannot present one,
	// so the random token in the path is the credential. /sub/… is the tidy
	// external address (Caddy forwards it); /api/sub/… keeps it reachable when the
	// API is addressed directly.
	s.mux.HandleFunc("GET /sub/{token}", s.subscriptionFeed)
	s.mux.HandleFunc("GET /api/sub/{token}", s.subscriptionFeed)
	s.panelRoute("GET /api/subscription", s.authenticated(s.subscriptionState))
	// 预览渲染的是「套用买家账号之前」的构建结果，里面是各节点的**共享主口令**；
	// 拿到它等于可以绕过订阅的到期 / 设备限制无限使用全部节点，所以只给管理员。
	s.panelRoute("GET /api/subscription/preview", s.authenticated(s.requireAdmin(s.subscriptionPreview)))
	s.panelRoute("POST /api/subscription/rotate", s.authenticated(s.requireAdmin(s.rotateSubscriptionToken)))
	// 2026-11-06：与订阅中心的增删改、合并订阅的换新链接同一门槛（requireAdmin）。停用会联动所有节点的账号。
	s.panelRoute("PATCH /api/subscription", s.authenticated(s.requireAdmin(s.updateSubscription)))
	// Multi-subscription storefront. /api/sub/view/… is public (the token in the
	// path is the credential, exactly like /sub/…) and only ever exposes node
	// hosts and capacities — never a password. /api/subscriptions* is the
	// operator's management surface; /api/store/* is the shop's machine API,
	// gated by a shared secret instead of a JWT.
	s.mux.HandleFunc("GET /api/sub/view/{token}", s.subscriptionView)
	s.panelRoute("GET /api/subscriptions", s.authenticated(s.listSubscriptionsHandler))
	s.panelRoute("POST /api/subscriptions", s.authenticated(s.requireAdmin(s.createSubscriptionHandler)))
	s.panelRoute("PATCH /api/subscriptions/{id}", s.authenticated(s.requireAdmin(s.updateSubscriptionHandler)))
	s.panelRoute("DELETE /api/subscriptions/{id}", s.authenticated(s.requireAdmin(s.deleteSubscriptionHandler)))
	s.panelRoute("POST /api/subscriptions/{id}/rotate", s.authenticated(s.requireAdmin(s.rotateSubscriptionHandler)))
	s.panelRoute("POST /api/subscriptions/store-key", s.authenticated(s.requireAdmin(s.rotateStoreKeyHandler)))
	s.mux.HandleFunc("POST /api/store/orders", s.requireStoreKey(s.storeCreateOrder))
	s.mux.HandleFunc("GET /api/store/orders", s.requireStoreKey(s.storeGetOrder))
	s.mux.HandleFunc("POST /api/store/renew", s.requireStoreKey(s.storeRenewOrder))
	s.mux.HandleFunc("POST /api/store/revoke", s.requireStoreKey(s.storeRevokeOrder))
	// 手工订阅接入商城续费（store_adopt.go）。
	s.mux.HandleFunc("GET /api/store/adopt-info", s.requireStoreKey(s.storeAdoptInfo))
	s.mux.HandleFunc("POST /api/store/adopt", s.requireStoreKey(s.storeAdopt))
	s.panelRoute("GET /api/tasks", s.authenticated(s.tasks))
	s.panelRoute("POST /api/tasks", s.authenticated(s.createTask))
	s.panelRoute("POST /api/tasks/{id}/cancel", s.authenticated(s.cancelTask))
	s.panelRoute("GET /api/tasks/{id}/logs", s.authenticated(s.taskLogs))
	s.panelRoute("GET /api/alerts", s.authenticated(s.alerts))
	s.panelRoute("GET /api/alerts/{id}/events", s.authenticated(s.alertEvents))
	s.panelRoute("POST /api/alerts/{id}/ack", s.authenticated(s.acknowledgeAlert))
	// 告警外发配置（2026-10-04 加）。
	//
	// ⚠️ 注册顺序在 `/api/alerts/{id}/...` 之前/之后都不影响：
	// `{id}` 只匹配**一段**路径，而 "webhook" 与 "{id}/events" 的形状不同，
	// ServeMux 不会混淆。但读的端点放在这里更直观。
	s.panelRoute("GET /api/alerts/webhook", s.authenticated(s.alertWebhookGet))
	s.panelRoute("PUT /api/alerts/webhook", s.authenticated(s.requireAdmin(s.alertWebhookPut)))
	// IP 检测密钥（ipapi.is / IPQS）：系统设置里填，加密存库，保存即生效。
	s.panelRoute("GET /api/settings/purity-keys", s.authenticated(s.requireAdmin(s.purityKeysGet)))
	s.panelRoute("PUT /api/settings/purity-keys", s.authenticated(s.requireAdmin(s.purityKeysPut)))
	s.panelRoute("POST /api/servers/{id}/credential", s.authenticated(s.requireAdmin(s.uploadCredential)))
	s.panelRoute("GET /api/servers/{id}/credential", s.authenticated(s.credentialMeta))
	s.panelRoute("POST /api/servers/{id}/write-ops", s.authenticated(s.requireAdmin(s.setWriteOps)))
	// Live systemd state for this host's whitelisted units. Read-only and served
	// synchronously (not via the task queue) so the UI gets an immediate answer.
	s.panelRoute("GET /api/servers/{id}/services", s.authenticated(s.serviceStatus))
	s.panelRoute("GET /api/servers/{id}/docker/containers", s.authenticated(s.requireAdmin(s.dockerContainers)))
	s.panelRoute("GET /api/servers/{id}/docker/compose", s.authenticated(s.requireAdmin(s.dockerCompose)))
	s.panelRoute("GET /api/servers/{id}/files", s.authenticated(s.requireAdmin(s.remoteFiles)))
	s.panelRoute("GET /api/servers/{id}/files/preview", s.authenticated(s.requireAdmin(s.remoteFilePreview)))
	s.panelRoute("POST /api/auth/terminal-session", s.authenticated(s.requireAdmin(s.terminalCookie)))
	s.mux.HandleFunc("DELETE /api/auth/terminal-session", s.clearTerminalCookie)
	s.mux.HandleFunc("GET /api/servers/{id}/terminal", s.terminalAuthenticated(s.requireAdmin(s.remoteTerminal)))
	s.panelRoute("GET /api/traffic/hourly", s.authenticated(s.trafficHourly))
	s.panelRoute("GET /api/audit", s.authenticated(s.requireAdmin(s.auditLogs)))
	s.panelRoute("GET /api/users", s.authenticated(s.requireAdmin(s.users)))
	s.panelRoute("POST /api/users", s.authenticated(s.requireAdmin(s.createUser)))
	s.panelRoute("PATCH /api/users/{id}", s.authenticated(s.requireAdmin(s.updateUser)))
	s.panelRoute("POST /api/users/{id}/password", s.authenticated(s.requireAdmin(s.resetUserPassword)))
	s.panelRoute("DELETE /api/users/{id}", s.authenticated(s.requireAdmin(s.deleteUser)))
	s.panelRoute("PATCH /api/nodes/{id}", s.authenticated(s.updateNode))
	s.panelRoute("PUT /api/nodes/{id}/traffic-limit", s.authenticated(s.requireAdmin(s.updateNodeTrafficLimit)))
	s.panelRoute("DELETE /api/servers/{id}/credential", s.authenticated(s.requireAdmin(s.deleteCredential)))
	s.panelRoute("POST /api/tasks/{id}/retry", s.authenticated(s.retryTask))
	s.panelRoute("POST /api/auth/change-password", s.authenticated(s.changePassword))
	s.panelRoute("GET /api/runtime", s.authenticated(s.runtimeStatus))
	// 3X-UI 面板对接：登记外部 3X-UI，用其管理员凭据登录后代理其 API。
	// 读与写照旧分桶限流；删除类操作需要确认头。
	// 同时兼容新旧两代面板（旧版 Vue / 新版 React 的 API 互不兼容），
	// 差异收敛在 xuipanel.go 与 xuihandlers.go 内部，路由与前端无需分支。
	s.panelRoute("GET /api/xui/panels", s.authenticated(s.xuiPanels))
	s.panelRoute("POST /api/xui/panels/preview", s.authenticated(s.xuiPreviewPanel))
	s.panelRoute("POST /api/xui/panels", s.authenticated(s.requireAdmin(s.xuiCreatePanel)))
	s.panelRoute("DELETE /api/xui/panels/{id}", s.authenticated(s.requireAdmin(s.xuiDeletePanel)))
	s.panelRoute("POST /api/xui/panels/{id}/refresh", s.authenticated(s.xuiRefreshPanel))
	s.panelRoute("GET /api/xui/panels/{id}/inbounds", s.authenticated(s.xuiInbounds))
	s.panelRoute("POST /api/xui/panels/{id}/clients", s.authenticated(s.xuiAddClient))
	s.panelRoute("DELETE /api/xui/panels/{id}/inbounds/{inboundID}/clients/{clientId}", s.authenticated(s.requireAdmin(s.xuiDeleteClient)))
	s.panelRoute("POST /api/xui/panels/{id}/inbounds/{inboundID}/clients/{email}/reset", s.authenticated(s.xuiResetClientTraffic))

	// 节点监控（延迟 + 大陆三网测速）。
	s.panelRoute("GET /api/monitoring/nodes", s.authenticated(s.nodeMonitoring))
	s.panelRoute("GET /api/monitoring/nodes/{id}/quality", s.authenticated(s.nodeChinaQuality))
	s.panelRoute("GET /api/speedtest/targets", s.authenticated(s.speedtestTargets))
	s.panelRoute("POST /api/speedtest/run", s.authenticated(s.speedtestRun))

	// 待补删的孤儿账号：删除节点时未能从节点侧注销的买家账号。
	// 见迁移 023 与 manage.go 的 recordOrphanAccounts —— 没有这两条出口，
	// 登记下来的线索就没人能用上。
	s.panelRoute("GET /api/orphans", s.authenticated(s.listOrphans))
	// 2026-11-04：补删 = 用留存的节点 API 密钥在节点上删除买家账号，与 DELETE /api/nodes/{id}/users/{uid}
	// 同一类不可逆操作，那条早就是 requireAdmin；结案（resolve）也是。只有补删原来对运维账号开放。
	s.panelRoute("POST /api/orphans/{id}/retry", s.authenticated(s.requireAdmin(s.retryOrphan)))
	s.panelRoute("POST /api/orphans/{id}/resolve", s.authenticated(s.requireAdmin(s.resolveOrphan)))
	// 2026-11-08：已删除节点的机器确认下线后，整台的待补删一次结案（见 orphan_machine_offline.go）。
	s.panelRoute("POST /api/orphans/nodes/{nodeID}/resolve-machine-offline", s.authenticated(s.requireAdmin(s.resolveOrphansMachineOffline)))

	// 订阅账号对账：把库里记的确定性密码重新下发到各节点。
	// 修的是"新逻辑修好了、但存量订阅仍不一致"这个盲区
	// （见 provisioning.go 的 reconcileSubscriptionAccounts）。
	s.panelRoute("POST /api/subscriptions/{id}/reconcile", s.authenticated(s.requireAdmin(s.reconcileSubscriptionHandler)))
	s.panelRoute("POST /api/subscriptions/reconcile-all", s.authenticated(s.requireAdmin(s.reconcileAllSubscriptionsHandler)))

	// 到期订阅的结案出口：节点永久下线时，让订阅行能被真正删掉，
	// 而不是永远卡在「到期未删除」并反复重拨死节点。
	// 见 expired_closeout.go 的说明。
	s.panelRoute("POST /api/subscriptions/{id}/close-expired", s.authenticated(s.requireAdmin(s.closeExpiredSubscriptionHandler)))
	s.panelRoute("POST /api/subscriptions/close-expired-batch", s.authenticated(s.requireAdmin(s.closeExpiredSubscriptionBatchHandler)))
	s.panelRoute("GET /api/install-jobs", s.authenticated(s.requireAdmin(s.listInstallJobs)))
	s.panelRoute("POST /api/install-jobs/{id}/resolve", s.authenticated(s.requireAdmin(s.resolveInstallJob)))
	s.restoreInstallJobs()
	s.loadDeliveryBase(context.Background())
	s.sortTelegramRoutes()
	s.interruptPortalBatches()
	return s
}

// requireAdmin is the single admin-only gate. Every admin route is registered
// through it so the check cannot be lost in a refactor, and a denied attempt is
// written to the audit log instead of vanishing.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := claimsFrom(r.Context())
		if !ok || claims.Role != "admin" {
			s.auditDenied(r, "forbidden_admin_only")
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要管理员角色"})
			return
		}
		next.ServeHTTP(w, r)
	}
}

func (s *Server) Handler() http.Handler {
	return s.requestLog(s.securityHeaders(s.cors(s.mux)))
}

// SetShutdownNotify 让后台 goroutine 能感知服务正在关闭。
//
// 用途（2026-10-30）：告警通知按静默期 sleep 后再推送；进程退出时
// 正在睡眠的告警会被无声丢弃，只留下"未送达"的记录。main 在收到
// SIGTERM/SIGINT 后把 shutdownCtx.Done() 传进来，等待中的 goroutine
// 立刻退出并记日志，运维能看到"是被关闭打断的"。
// 必须在 Server 启动前调用；之后调用是非法的（后台 goroutine 已经开始读它）。
func (s *Server) SetShutdownNotify(ch <-chan struct{}) {
	s.shutdownDone = ch
}

func (s *Server) authenticate(r *http.Request) (auth.Claims, error) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return auth.Claims{}, errors.New("missing bearer token")
	}
	claims, err := s.signer.Verify(header[len(prefix):])
	if err != nil {
		return auth.Claims{}, err
	}
	var role, email string
	var version int64
	if s.sessionUser != nil {
		role, email, version, err = s.sessionUser(r.Context(), claims.Subject)
	} else {
		err = s.db.QueryRow(r.Context(), `SELECT role,email,token_version FROM users WHERE id=$1::uuid`, claims.Subject).Scan(&role, &email, &version)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// 2026-11-06：读用户失败（数据库不可用）不是「会话已撤销」。原来一律 401：数据库抖一下，
		// 前端收到 401 就清掉本地会话，所有人被踢回登录页；终端的定期复核也会断开正在用的 SSH 会话。
		return auth.Claims{}, errSessionCheckUnavailable{err}
	}
	if err != nil || claims.Version != version {
		return auth.Claims{}, errors.New("会话已撤销或账号不存在")
	}
	claims.Role, claims.Email = role, email
	return claims, nil
}

// errSessionCheckUnavailable：令牌本身有效，但此刻无法向数据库核对会话（503，可重试）。
type errSessionCheckUnavailable struct{ err error }

func (e errSessionCheckUnavailable) Error() string {
	return "暂时无法核对登录状态，请稍后重试"
}
func (e errSessionCheckUnavailable) Unwrap() error { return e.err }

// writeAuthError 把 authenticate 的错误写成响应：数据库不可用回 503，其余（缺令牌、签名错、已撤销）回 401。
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	var unavailable errSessionCheckUnavailable
	if errors.As(err, &unavailable) {
		s.logger.Warn("auth: 核对会话时数据库不可用", "error", unavailable.err)
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": unavailable.Error()})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
}

func viewerMayUseRequest(method, path string) bool {
	// Viewer is read-only, but must retain the ability to rotate their own password.
	if method == http.MethodPost && path == "/api/auth/change-password" {
		return true
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	// This legacy GET refreshes and writes node metadata; it is not a read-only GET.
	// 2026-11-03：HEAD 也要拦 —— Go 1.22+ 的 GET 路由同样匹配 HEAD，原来 viewer 发 HEAD 就能触发这次写库。
	if (method == http.MethodGet || method == http.MethodHead) && strings.HasPrefix(path, "/api/nodes/") && strings.HasSuffix(path, "/api") {
		return false
	}
	return true
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := s.authenticate(r)
		if err != nil {
			s.writeAuthError(w, err)
			return
		}
		if claims.Role != "admin" && claims.Role != "operator" && claims.Role != "viewer" {
			s.auditDenied(r.WithContext(context.WithValue(r.Context(), claimsKey, claims)), "forbidden_invalid_role")
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "用户角色无效"})
			return
		}
		if claims.Role == "viewer" && s.viewerRestrictSubscriptions && subscriptionCredentialPath(r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "当前部署未授权只读角色访问订阅凭证"})
			return
		}
		if claims.Role == "viewer" && !viewerMayUseRequest(r.Method, r.URL.Path) {
			s.auditDenied(r.WithContext(context.WithValue(r.Context(), claimsKey, claims)), "forbidden_viewer_write")
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "只读角色不能执行此操作"})
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// limitScope 说明被限的是哪一类请求。前端不需要认识它 —— 429 的文案已经
// 按 scope 分好了 —— 但把它放进响应体，出问题时一眼能看出是哪一桶满了。
type limitScope string

const (
	scopeRead   limitScope = "read"
	scopeWrite  limitScope = "write"
	scopePublic limitScope = "public"
)

// enforceReadLimit / enforceWriteLimit 检查配额；超限时把 429 写好并返回 false。
//
// 为什么把「写响应」收进来，而不是像原来那样只回一个 bool、让 43 个调用点各写
// 一遍 writeJSON：429 的语义要三样东西一起给 —— 状态码、Retry-After 头、以及
// 一个能说清「哪一类请求被限、还要等多久」的响应体。散在 43 处就没法保证一致，
// 事实上改造前的响应体里连「哪一类」都没有，只有一句「请求过于频繁」。
func (s *Server) enforceReadLimit(w http.ResponseWriter, subject string) bool {
	return s.enforceLimit(w, s.readLimiter, scopeRead, subject)
}

func (s *Server) enforceWriteLimit(w http.ResponseWriter, subject string) bool {
	return s.enforceLimit(w, s.writeLimiter, scopeWrite, subject)
}

// enforcePublicLimit 管的是不需要认证的订阅端点。它们单独一桶，是为了让
// 「公开流量」和「管理端操作」互不挤占：一个订阅客户端轮询得再凶，也不该
// 让运维那边点不动按钮。
func (s *Server) enforcePublicLimit(w http.ResponseWriter, subject string) bool {
	return s.enforceLimit(w, s.publicLimiter, scopePublic, subject)
}

func (s *Server) enforceLimit(w http.ResponseWriter, limiter *slidingLimiter, scope limitScope, subject string) bool {
	if limiter == nil {
		return true
	}
	ok, retryAfter := limiter.allowWithRetry(subject)
	if ok {
		return true
	}
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))

	// 文案按 scope 分开，是因为两种「被限」对操作者的含义完全不同：
	// 读被限只是「点太快了」，重试即可；写被限则意味着**刚才那个操作没有
	// 下发**，操作者必须知道这一点，否则会以为开关已经切过去了。
	message := fmt.Sprintf("请求过于频繁，请 %d 秒后再试", seconds)
	if scope == scopeWrite {
		message = fmt.Sprintf("操作过于频繁，请 %d 秒后再试；本次操作未下发到节点", seconds)
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":               message,
		"limit_scope":         string(scope),
		"retry_after_seconds": seconds,
	})
	return false
}

// lockNodeWrite 取到该节点的写锁，返回释放函数。
//
// 用 sync.Map 而不是 map+mutex：节点数量少、生命周期长，但读多写少，
// sync.Map 免去了每次取锁时的全局互斥。锁本身常驻——一个节点的写锁
// 内存开销可以忽略，不值得为回收它引入引用计数。
func (s *Server) lockNodeWrite(nodeID string) func() {
	release, _ := s.lockNodeWriteCtx(context.Background(), nodeID)
	return release
}

// lockNodeWriteCtx 与 lockNodeWrite 同一把锁，但等待受 ctx 约束：ctx 结束仍没拿到就返回错误。
//
// 2026-11-03：批量更新门户会把一台节点的写锁持有很久（两轮各「下发 + 最长 3 分钟核验」）。
// 原来交互式的节点操作直接 mu.Lock()，客户端断开后 goroutine 和连接照样挂着等锁，
// 没有 WriteTimeout 兜底，会越积越多。交互请求用这个版本，等不到就回「节点正在更新」。
func (s *Server) lockNodeWriteCtx(ctx context.Context, nodeID string) (func(), error) {
	if norm, ok := canonicalUUID(nodeID); ok {
		nodeID = norm
	}
	value, _ := s.nodeWriteLocks.LoadOrStore(nodeID, make(chan struct{}, 1))
	sem := value.(chan struct{})
	select {
	case sem <- struct{}{}:
		// 与读闸门一致用 sync.Once 保障释放幂等，防止意外重复调用抽空通道造成永久死锁。
		var once sync.Once
		return func() {
			once.Do(func() { <-sem })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// nodeReadConcurrency 是同一节点上允许并存的只读探测数。
//
// 为什么不设成 1：探测是只读、幂等的，串行会把 portal-status 的延迟从
// 「一次往返」拉成「七次往返」。取 4 让七路探测排成两轮，而闸门缺席时它
// 能飙到十几路（两个标签页 × 七路）。
const nodeReadConcurrency = 4

// lockNodeRead 取该节点读闸门的一个槽位，返回释放函数。
//
// ctx 结束前没排到就返回错误 —— 调用方在超时预算内等不到槽位时应当如实
// 报「门户忙」，而不是无限期挂着占住一个 HTTP 连接。
//
// 为什么与 nodeWriteLocks 分开而不是共用一把：读和写的时间尺度差太远。
// 一次重启下发之后节点要几十秒才回来，若读请求排在写锁后面，操作者点一次
// 刷新就要等几十秒、然后撞上 15s 超时 —— 那不是保护，是把一次计划内的
// 重启放大成一片红色报错。
func (s *Server) lockNodeRead(ctx context.Context, nodeID string) (func(), error) {
	if norm, ok := canonicalUUID(nodeID); ok {
		nodeID = norm
	}
	value, _ := s.nodeReadGates.LoadOrStore(nodeID, make(chan struct{}, nodeReadConcurrency))
	slots := value.(chan struct{})
	select {
	case slots <- struct{}{}:
		// 释放包成幂等的。信号量用「发送即获取、接收即释放」，多接收一次
		// 会把这个通道抽空，之后所有释放都会阻塞 —— 也就是这个节点的读闸门
		// 永久卡死，而且只在释放路径上出现，极难定位。
		//
		// 这不是假想：写单测时就因为一处「手动释放后又让 defer 再释放一次」
		// 把测试挂死在释放闭包上，栈顶正是这里。sync.Once 的代价是一个
		// 十几字节的结构体，而探测本身是网络往返，完全不成比例。
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type ctxKey string

const claimsKey ctxKey = "claims"

func claimsFrom(ctx context.Context) (auth.Claims, bool) {
	value := ctx.Value(claimsKey)
	claims, ok := value.(auth.Claims)
	return claims, ok
}

type slidingLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	limit    int
	attempts map[string][]time.Time
	// lastSweep 上次全量清理时刻。
	//
	// ⚠️ 2026-10-30：原来只在 `len(attempts) > limiterSweepThreshold` 时清理。
	// 一旦超过阈值（分布式爆破正好会把它推上去），**每一次请求**都触发一次
	// O(n) 全表扫描，而且是在持锁状态下做的 —— 所有登录请求被串行化，
	// CPU 随 n 线性上升。这正是"为了防爆破而加的机制反而放大了攻击"。
	// 现在限制清理频率：5 秒最多一次；超过 4 倍阈值时立即清理。
	lastSweep time.Time
	// sweepRuns 是实际做过全量清理的次数（测试断言「洪水下不能每请求都全扫」用）。
	sweepRuns int
}

// limiterSweepInterval 是全量清理的最小间隔。
const limiterSweepInterval = 5 * time.Second

// limiterHardSweepInterval 是「超过 4 倍阈值」分支的最小清理间隔（2026-11-06）。
//
// 原来这个分支不看 lastSweep，超过 4 倍阈值后**每个请求**都做一次 O(n) 全表扫描并持锁。
// 公开订阅端点用 `sub:<token>` 作为限流主体，随便发 2 万个不存在的 token 就能把 map 推到阈值
// 以上且这些记录在窗口内都不会过期 —— sweep 删不掉它们，条件持续成立 ⇒ 每请求全扫 + 串行化，
// 防滥用机制反过来成了 DoS 放大器。
const limiterHardSweepInterval = time.Second

// limiterHardResetLimit 是内存硬上限：达到后整表清空（丢掉全部计数）。只在随机 key 洪水时触达，
// 正常使用远低于此；清空的代价是「当前窗口内的限流重新开始计数」，比内存无界增长好。
const limiterHardResetLimit = 65536

// limiterSweepThreshold 是触发全量清理的 subject 数上限。
//
// 这个 map 的键是「见过的主体」：认证用户是 UUID，公开订阅端点是每个订阅
// 各自的 token。两者都不会重复，所以不清理就是一份只增不减的内存账。
// 逐次清理只能回收「又出现了的主体」，回收不了那些再也不出现的，
// 于是需要一个按规模触发的全量清理。
const limiterSweepThreshold = 4096

func newSlidingLimiter(window time.Duration, limit int) *slidingLimiter {
	return &slidingLimiter{window: window, limit: limit, attempts: map[string][]time.Time{}}
}

func (l *slidingLimiter) allow(subject string) bool {
	ok, _ := l.allowWithRetry(subject)
	return ok
}

// allowWithRetry 与 allow 同义，但超限时额外返回「还要等多久才有位置」，
// 用来填 Retry-After。
//
// 用最老那次尝试的过期时刻，而不是「最后一次 + window」：滑动窗口里空出
// 位置的时刻就是最老那条记录滑出去的时刻。写成后者会给出偏长的等待时间，
// 操作者照着一个虚高的数字等，等完发现其实早就可以重试了。
func (l *slidingLimiter) allowWithRetry(subject string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 在锁内采样当前时间，确保每个主体的历史时间戳严格单调非递减，Retry-After 不出现负偏。
	now := time.Now()
	cutoff := now.Add(-l.window)

	// 限频清理：超过 4 倍阈值立即清（内存优先）；否则 5 秒最多清一次，
	// 避免每次请求都做 O(n) 扫描。
	switch {
	case len(l.attempts) >= limiterHardResetLimit:
		// 优先清理过期条目，避免无辜清空活跃主体计数；仅当全为窗口内洪水仍达硬上限时整表复位。
		l.sweepLocked(cutoff)
		if len(l.attempts) >= limiterHardResetLimit {
			l.attempts = map[string][]time.Time{}
		}
		l.lastSweep = now
		l.sweepRuns++
	case len(l.attempts) > limiterSweepThreshold*4:
		if now.Sub(l.lastSweep) >= limiterHardSweepInterval {
			l.lastSweep = now
			l.sweepLocked(cutoff)
			l.sweepRuns++
		}
	case len(l.attempts) > limiterSweepThreshold && now.Sub(l.lastSweep) >= limiterSweepInterval:
		l.lastSweep = now
		l.sweepLocked(cutoff)
		l.sweepRuns++
	}

	history := l.attempts[subject]
	kept := history[:0]
	for _, ts := range history {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= l.limit {
		l.attempts[subject] = kept
		// kept 的顺序即写入顺序，也就是时间顺序，kept[0] 最老。
		return false, kept[0].Sub(cutoff)
	}
	if len(kept) == 0 {
		// 窗口内一条记录都不剩，整个键可以丢掉。kept 复用的是 history 的
		// 底层数组，删键之后它就不再被引用，随后 append 会重新分配。
		delete(l.attempts, subject)
		kept = nil
	}
	l.attempts[subject] = append(kept, now)
	return true, 0
}

// sweepLocked 丢掉窗口内已经没有任何记录的主体。调用方必须持 l.mu。
func (l *slidingLimiter) sweepLocked(cutoff time.Time) {
	for subject, history := range l.attempts {
		alive := false
		for _, ts := range history {
			if ts.After(cutoff) {
				alive = true
				break
			}
		}
		if !alive {
			delete(l.attempts, subject)
		}
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	// ⚠️ 2026-10-24 审计 P2-4：Ping 之外还要真的执行一条查询。
	//
	// Ping 只验证「能从池里拿到一条连接并完成握手」。连接池里已有的空闲
	// 连接若已被服务端悄悄断开（PG 重启、防火墙 idle timeout、max_connections
	// 被挤掉），Ping 可能仍然成功，因为 pgx 拿到的是池里那条「看起来还活着」
	// 的连接的元数据。而任何真实查询会立刻失败。
	//
	// 用 `SELECT 1` 而不是查业务表：它不需要任何权限、不扫任何页，
	// 代价接近零，但能真正走到「服务端执行并返回」这一步。
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	var one int
	if err := s.db.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Turnstile token（见 turnstile.go）；未启用 Turnstile 时忽略。
	TurnstileToken string `json:"turnstile_token"`
	ServiceKey     string `json:"service_key"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.requestClientIP(r)
	// 连续失败封禁（见 loginsecurity.go）：封禁期内连正确密码也拒绝。
	if until := loginGuardState.bannedUntil(loginKey(ip), time.Now()); !until.IsZero() {
		w.Header().Set("X-Login-Result", "cooldown")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("登录失败次数过多，此 IP 已被封禁到 %s", until.In(time.FixedZone("UTC+8", 8*3600)).Format("15:04"))})
		return
	}
	if !s.loginLimiter.allow(loginKey(ip)) {
		w.Header().Set("X-Login-Result", "rate_limited")
		if s.logger != nil {
			s.logger.Warn("login rate limited", "ip", ip)
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "登录尝试过于频繁"})
		return
	}
	var input loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil || input.Email == "" || input.Password == "" {
		w.Header().Set("X-Login-Result", "invalid_payload")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credentials payload"})
		return
	}
	// 人机验证放在查库/校验密码之前：没过验证的请求连「账号是否存在」都试探不到，
	// 且不写入 audit_logs 数据库表（防止无令牌分布式爆破脚本写爆数据库），仅在内存中静默计次封禁。
	if !validLoginService(input.Email, input.ServiceKey) && !s.verifyTurnstile(r.Context(), input.TurnstileToken, ip) {
		w.Header().Set("X-Login-Result", "turnstile_failed")
		if !s.isTrustedProxyAddress(ip) { // 同 onLoginFailure：不把反代地址计入封禁
			loginGuardState.fail(loginKey(ip), time.Now())
		}
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "人机验证失败，请刷新页面重试"})
		return
	}
	var id, hash, role, storedEmail string
	var version int64
	// 2026-11-03：建用户时邮箱按小写存，登录却按原样比较 —— 输入 Bob@X.com 必然 401，
	// 且每次都计入同一 IP 的失败次数。按不区分大小写匹配（种子管理员按配置原样存，也能对上）。
	err := s.db.QueryRow(r.Context(), `SELECT id, password_hash, role, token_version, email FROM users WHERE lower(email) = lower($1) ORDER BY (email = $1) DESC, created_at LIMIT 1`, strings.TrimSpace(input.Email)).Scan(&id, &hash, &role, &version, &storedEmail)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// 2026-11-06：数据库错误不是「密码错」。原来一律按账号不存在处理：库短暂不可用时管理员看到
		// 「邮箱或密码错误」，每次重试都记一次失败，几次之后自己的 IP 被封禁；审计里也全是假的登录失败。
		w.Header().Set("X-Login-Result", "unavailable")
		s.logDatabaseError(r, err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "登录服务暂时不可用，请稍后再试"})
		return
	}
	if err != nil {
		auth.BurnPasswordCheck(input.Password) // 与「账号存在、密码错」耗时一致，防按响应时间枚举账号
		w.Header().Set("X-Login-Result", "invalid_credentials")
		s.auditLoginFailure(r, ip, input.Email)
		s.onLoginFailure(ip, input.Email)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "邮箱或密码错误"})
		return
	}
	if !auth.VerifyPassword(hash, input.Password) {
		w.Header().Set("X-Login-Result", "invalid_credentials")
		s.auditLoginFailure(r, ip, input.Email)
		s.onLoginFailure(ip, input.Email)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "邮箱或密码错误"})
		return
	}
	// 令牌与登录记录里用库中保存的邮箱，不用本次输入的大小写形式。
	token, err := s.signer.IssueVersion(id, storedEmail, role, version)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE users SET last_login_at = now() WHERE id = $1`, id); err != nil {
		s.logger.Warn("update last_login_at failed", "error", err)
	}
	w.Header().Set("X-Login-Success", role)
	s.onLoginSuccess(r.Context(), id, storedEmail, ip, r.UserAgent())
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "email": storedEmail, "role": role})
}

func (s *Server) audit(ctx context.Context, r *http.Request, action, resourceType, resourceID string, details map[string]any) {
	if s.db == nil {
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	resourceID, details = auditResource(resourceID, details)
	claims, ok := claimsFrom(ctx)
	var userID any
	if ok {
		userID = claims.Subject
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		detailsJSON = []byte(`{}`)
	}
	ip := s.requestClientIP(r)
	if _, err := s.db.Exec(ctx, `INSERT INTO audit_logs (user_id, action, resource_type, resource_id, ip_address, details) VALUES ($1, $2, $3, NULLIF($4,'')::uuid, NULLIF($5,'')::inet, $6::jsonb)`, userID, action, resourceType, resourceID, ip, string(detailsJSON)); err != nil {
		s.logger.Warn("audit log insert failed", "action", action, "error", err)
	}
}

// auditResource 把资源标识拆成「能进 resource_id 列的 UUID」和「进不了的标识」（2026-11-12）。
//
// audit_logs.resource_id 是 UUID 列，而 system_setting 这类资源的标识是配置键（如 "schedule_config"）。
// 原来不管传进来的是什么都直接转成 uuid：非 UUID 的标识让整条 INSERT 以 22P02 失败，只留一条 Warn ——
// 轮换商城密钥、改强制签名开关、改告警外发地址、改 Telegram 绑定、改定时巡检这些操作在审计里一条都没有。
// 现在非 UUID 的标识记进 details.resource_key，resource_id 留空，审计行照常写入。
// 不改调用方传入的 map（复制后再加键）。
func auditResource(resourceID string, details map[string]any) (string, map[string]any) {
	if resourceID == "" || uuidPattern.MatchString(resourceID) {
		return resourceID, details
	}
	withKey := make(map[string]any, len(details)+1)
	for k, v := range details {
		withKey[k] = v
	}
	if _, taken := withKey["resource_key"]; !taken {
		withKey["resource_key"] = resourceID
	}
	return "", withKey
}

// auditLoginFailure records a rejected login. A successful login is not audited —
// only failures matter for detecting brute force — and the attempted password is
// never written to the log.
func (s *Server) auditLoginFailure(r *http.Request, ip, email string) {
	if s.db == nil {
		return
	}
	details, err := json.Marshal(map[string]any{"email": email})
	if err != nil {
		details = []byte(`{}`)
	}
	if _, err := s.db.Exec(r.Context(),
		`INSERT INTO audit_logs (user_id, action, resource_type, resource_id, ip_address, details) VALUES (NULL, 'login_failed', 'session', NULL, NULLIF($1,'')::inet, $2::jsonb)`,
		ip, string(details)); err != nil {
		s.logger.Warn("audit: login failure insert failed", "error", err)
	}
}

// auditDenied records an authenticated request that was rejected for insufficient
// privileges, which is how a privilege-escalation attempt shows up in the audit trail.
func (s *Server) auditDenied(r *http.Request, action string) {
	if s.db == nil {
		return
	}
	claims, _ := claimsFrom(r.Context())
	var userID any
	if claims.Subject != "" {
		userID = claims.Subject
	}
	details, err := json.Marshal(map[string]any{"path": r.URL.Path, "method": r.Method, "role": claims.Role})
	if err != nil {
		details = []byte(`{}`)
	}
	if _, err := s.db.Exec(r.Context(),
		`INSERT INTO audit_logs (user_id, action, resource_type, resource_id, ip_address, details) VALUES ($1, $2, 'session', NULL, NULLIF($3,'')::inet, $4::jsonb)`,
		userID, action, s.requestClientIP(r), string(details)); err != nil {
		s.logger.Warn("audit: denied request insert failed", "action", action, "error", err)
	}
}

func (s *Server) servers(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT s.id, s.name, s.host, s.ssh_port, s.ssh_user, s.region, s.provider, s.tags, s.status, s.created_at, s.updated_at,
       COALESCE(s.latency_ms, -1) AS latency_ms,
       s.allow_write_ops,
       COALESCE(s.write_units, '{}'::text[]) AS write_units,
       EXISTS (SELECT 1 FROM server_credentials sc WHERE sc.server_id = s.id) AS has_credential,
       COALESCE(s.traffic_api_url, '') AS traffic_api_url,
       COALESCE(s.traffic_secret_file, '') AS traffic_secret_file,
       COALESCE(m.cpu_percent, 0) AS cpu_percent,
       COALESCE(m.memory_used_bytes, 0) AS memory_used_bytes,
       COALESCE(m.memory_total_bytes, 0) AS memory_total_bytes,
       COALESCE(m.disk_used_bytes, 0) AS disk_used_bytes,
       COALESCE(m.disk_total_bytes, 0) AS disk_total_bytes,
       m.sampled_at AS metrics_sampled_at,
       COALESCE(tr.tx_bytes, 0) AS tx_bytes,
       COALESCE(tr.rx_bytes, 0) AS rx_bytes,
       COALESCE(tr.live_bytes, 0) AS measured_bytes_24h,
       COALESCE(tr.portal_delta, 0) AS portal_bytes_24h
FROM servers s
LEFT JOIN LATERAL (
    -- ⚠️ 2026-10-30：把 sampled_at 一并带出。原来只给数值、不给时间，
    -- SSH 停采一周的机器在「主机详情」里仍显示一周前的 CPU，与刚采到的一样。
    -- 数值仍然返回（趋势图要用历史），但是否当作"当前指标"由前端按时间判断。
    SELECT cpu_percent, memory_used_bytes, memory_total_bytes, disk_used_bytes, disk_total_bytes, sampled_at
    FROM server_metrics
    WHERE server_id = s.id
    ORDER BY sampled_at DESC LIMIT 1
) m ON true
LEFT JOIN LATERAL (
    -- 2026-11-02 审计：原来是 4 个各扫一遍 24h traffic_samples 的 LATERAL，
    -- 合并成一次扫描 + FILTER 聚合，结果逐字段与旧查询一致。
    --   tx / rx：除 portal_used 外的所有来源；
    --   live：hysteria 自己的 trafficStats（按周期清零的真实速率，24h 求和即实测用量）；
    --   portal_delta：portal 的 used_bytes 只在客户端重新认证时前进，取 24h 内的变化量作兜底。
    -- 2026-11-03：tx/rx 每台节点只取一种来源。node_iface 是整张网卡的增量，已经包含
    -- Hysteria 的流量；原来与 hysteria_traffic 一起相加，有整机计量的节点被算两遍
    -- （生产实测哥伦布 24h：网卡 7.3 GB + Hysteria 3.2 GB）。有 node_iface 就只用它。
    SELECT SUM(ts.tx_bytes) FILTER (WHERE ts.source <> 'portal_used' AND (ts.source = 'node_iface' OR NOT n.has_iface))::bigint AS tx_bytes,
           SUM(ts.rx_bytes) FILTER (WHERE ts.source <> 'portal_used' AND (ts.source = 'node_iface' OR NOT n.has_iface))::bigint AS rx_bytes,
           COALESCE(SUM(ts.rx_bytes + ts.tx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic'), 0)::bigint AS live_bytes,
           -- portal_delta：逐节点只累加正向增量（删用户会让累计和下降，max-min 会把下降算成流量）。
           COALESCE((SELECT SUM(GREATEST(d.delta, 0)) FROM (
                SELECT p.tx_bytes - LAG(p.tx_bytes) OVER (PARTITION BY p.node_id ORDER BY p.sampled_at) AS delta
                FROM traffic_samples p JOIN hysteria_nodes pn ON pn.id = p.node_id AND pn.deleted_at IS NULL
                WHERE pn.server_id = s.id AND p.source = 'portal_used' AND p.sampled_at > now() - interval '24 hours'
           ) d), 0)::bigint AS portal_delta
    FROM traffic_samples ts
    -- 每台节点判断一次「有没有整机计量」，而不是对每一行样本做 EXISTS。
    JOIN (SELECT hn.id, hn.traffic_sampled_at > now() - interval '24 hours' IS TRUE AS has_iface
          FROM hysteria_nodes hn WHERE hn.server_id = s.id AND hn.deleted_at IS NULL) n ON n.id = ts.node_id
    WHERE ts.sampled_at > now() - interval '24 hours'
) tr ON true
WHERE s.deleted_at IS NULL
ORDER BY s.created_at DESC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, host, sshUser, region, provider, status string
		var sshPort int
		var tags []string
		var createdAt, updatedAt time.Time
		var cpu float64
		var memUsed, memTotal, diskUsed, diskTotal, txBytes, rxBytes, measured24h, portal24h int64
		var latencyMs int
		var metricsAt *time.Time
		var allowWrite, hasCredential bool
		var writeUnits []string
		var trafficURL, trafficFile string
		if err := rows.Scan(&id, &name, &host, &sshPort, &sshUser, &region, &provider, &tags, &status, &createdAt, &updatedAt, &latencyMs, &allowWrite, &writeUnits, &hasCredential, &trafficURL, &trafficFile, &cpu, &memUsed, &memTotal, &diskUsed, &diskTotal, &metricsAt, &txBytes, &rxBytes, &measured24h, &portal24h); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{
			"id": id, "name": name, "host": host, "ssh_port": sshPort, "ssh_user": sshUser,
			"region": region, "provider": provider, "tags": tags, "status": status,
			"created_at": createdAt, "updated_at": updatedAt,
			"cpu_percent":         cpu,
			"memory_percent":      percent(memUsed, memTotal),
			"disk_percent":        percent(diskUsed, diskTotal),
			"metrics_sampled_at":  metricsAt,
			"tx_bytes_24h":        txBytes,
			"rx_bytes_24h":        rxBytes,
			"measured_bytes_24h":  measured24h,
			"portal_bytes_24h":    portal24h,
			"latency_ms":          latencyMs,
			"allow_write_ops":     allowWrite,
			"write_units":         writeUnits,
			"has_credential":      hasCredential,
			"traffic_api_url":     trafficURL,
			"traffic_secret_file": trafficFile,
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createServerRequest struct {
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	SSHPort     int      `json:"ssh_port"`
	SSHUser     string   `json:"ssh_user"`
	Region      string   `json:"region"`
	Provider    string   `json:"provider"`
	Tags        []string `json:"tags"`
	TrafficURL  string   `json:"traffic_api_url"`
	TrafficFile string   `json:"traffic_secret_file"`
}

var (
	trafficEndpointURLPattern  = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost)(:[0-9]{1,5})$`)
	trafficSecretFilePattern   = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
)

// The hysteria trafficStats endpoint drains counters on read and its secret is a
// bearer-style value, so it must never be reachable off-box: only loopback URLs are
// accepted, and the secret path is restricted to a plain absolute file path because
// both are interpolated into a remote shell command.
func validTrafficEndpoint(rawURL, secretFile string) bool {
	if rawURL == "" && secretFile == "" {
		return true
	}
	if !trafficEndpointURLPattern.MatchString(rawURL) {
		return false
	}
	// 严格校验端口处于 1-65535 范围，防止 :0 或 :99999 通过正则放行。
	if idx := strings.LastIndex(rawURL, ":"); idx >= 0 {
		port, err := strconv.Atoi(rawURL[idx+1:])
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	if !trafficSecretFilePattern.MatchString(secretFile) {
		return false
	}
	// 禁止路径中出现 ".." 穿越段，防止越出目标目录读取任意文件。
	for _, seg := range strings.Split(secretFile, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func (s *Server) createServer(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input createServerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server payload"})
		return
	}
	// 2026-11-06：先 trim 再判空。原来只比对 ""：{"name":"   ","host":"   "} 能建出一台「无名机器 +
	// 地址是三个空格」的主机，采集器每 30 秒朝 "   " 拨 SSH 一直失败 —— 正是 uploadCredential 注释里
	// 记的「把面板 IP 送进 fail2ban」那类循环。createUser / updateServer 早就 trim 过，这里是漏网的第三个。
	input.Name = strings.TrimSpace(input.Name)
	input.Host = strings.TrimSpace(input.Host)
	input.SSHUser = strings.TrimSpace(input.SSHUser)
	if input.Name == "" || input.Host == "" || input.SSHUser == "" || input.SSHPort < 1 || input.SSHPort > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server payload"})
		return
	}
	// 地址里不能有空白：它会被拼进 SSH 拨号与命令，空白只会让采集器反复失败。
	if strings.ContainsAny(input.Host, " \t\r\n/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机地址只能是 IP 或域名，不能含空格或路径"})
		return
	}
	if input.Tags == nil {
		input.Tags = []string{}
	}
	if !validTrafficEndpoint(input.TrafficURL, input.TrafficFile) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "流量接口地址只能是回环 http（如 http://127.0.0.1:29862），密钥路径需为绝对路径"})
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `INSERT INTO servers (name, host, ssh_port, ssh_user, region, provider, tags, traffic_api_url, traffic_secret_file) VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,''), NULLIF($9,'')) RETURNING id`, input.Name, input.Host, input.SSHPort, input.SSHUser, input.Region, input.Provider, input.Tags, input.TrafficURL, input.TrafficFile).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "已登记过同地址同 SSH 端口的主机"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_server", "server", id, map[string]any{"name": input.Name, "host": input.Host})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	// 节点的分类归属，一次查完（避免每个节点一次查询）。
	// 读失败不致命：分类是展示与圈定用的增强信息，
	// 让整个节点列表失败会因"分类表暂时读不到"而看不到任何节点。
	nodeCategoryMap, catErr := s.loadNodeCategoryMap(r.Context())
	if catErr != nil {
		s.logger.Warn("nodes: 读取节点分类失败，按未分类处理", "error", catErr)
		nodeCategoryMap = map[string][]nodeCategory{}
	}

	rows, err := s.db.Query(r.Context(), `
SELECT n.id, n.server_id, n.name, n.version, n.listen_port, n.domain, n.status, n.last_checked_at, n.created_at, n.updated_at,
       COALESCE(tr.total_bytes, 0) AS used_bytes_total,
       COALESCE(tr.live_bytes, 0) AS used_bytes_24h,
       COALESCE(n.api_url, '') AS api_url,
       (n.api_key IS NOT NULL) AS has_api,
       (n.portal_basic_user IS NOT NULL AND n.portal_basic_password IS NOT NULL) AS has_basic,
       COALESCE(n.users_count, -1) AS users_count,
       n.api_checked_at,
       n.in_store,
       COALESCE(n.status_error, '') AS status_error,
       n.traffic_limit_bytes, n.traffic_reset_day, n.traffic_cycle_start, n.traffic_cycle_bytes,
       n.traffic_sampled_at, n.traffic_error, n.traffic_suspended,
       COALESCE(tr.iface_bytes, 0) AS iface_bytes_24h
FROM hysteria_nodes n
LEFT JOIN LATERAL (
    -- 合并原 3 次 24h traffic_samples 侧向扫描为单次索引扫描 + FILTER 聚合，结果口径逐字段一致。
    SELECT COALESCE(SUM(ts.rx_bytes + ts.tx_bytes) FILTER (WHERE ts.source = 'node_iface'), 0)::bigint AS iface_bytes,
           MAX(ts.tx_bytes) FILTER (WHERE ts.source = 'portal_used')::bigint AS total_bytes,
           COALESCE(SUM(ts.rx_bytes + ts.tx_bytes) FILTER (WHERE ts.source = 'hysteria_traffic'), 0)::bigint AS live_bytes
    FROM traffic_samples ts
    WHERE ts.node_id = n.id AND ts.sampled_at > now() - interval '24 hours'
) tr ON true
WHERE n.deleted_at IS NULL
ORDER BY n.created_at DESC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, serverID, name, version, domain, status string
		var listenPort int
		var lastCheckedAt *time.Time
		var createdAt, updatedAt time.Time
		var usedTotal, used24h int64
		var apiURL string
		var hasAPI, hasBasic bool
		var usersCount int
		var apiCheckedAt *time.Time
		var inStore bool
		var statusError string
		var trafficLimit *int64
		var trafficResetDay int
		var trafficCycleStart, trafficSampledAt *time.Time
		var trafficCycleBytes, iface24h int64
		var trafficError string
		var trafficSuspended bool
		if err := rows.Scan(&id, &serverID, &name, &version, &listenPort, &domain, &status, &lastCheckedAt, &createdAt, &updatedAt, &usedTotal, &used24h, &apiURL, &hasAPI, &hasBasic, &usersCount, &apiCheckedAt, &inStore, &statusError,
			&trafficLimit, &trafficResetDay, &trafficCycleStart, &trafficCycleBytes, &trafficSampledAt, &trafficError, &trafficSuspended, &iface24h); err != nil {
			s.databaseError(w, r, err)
			return
		}
		healthOK, healthReason := nodeHealth(status, statusError, lastCheckedAt, time.Now())
		if trafficSuspended {
			healthOK, healthReason = false, "本月流量已超过限额，节点服务已自动停止（重置日或调高限额后自动恢复）"
		}
		// 整机计量一旦有数据，24h 用量改用它：旧来源只覆盖 SSH 采集的节点，
		// 且不含 Reality / GOST 流量。
		if trafficSampledAt != nil {
			used24h = iface24h
		}
		result = append(result, map[string]any{"id": id, "server_id": serverID, "name": name, "version": version, "listen_port": listenPort, "domain": domain, "status": status, "last_checked_at": lastCheckedAt, "created_at": createdAt, "updated_at": updatedAt, "used_bytes_total": usedTotal, "used_bytes_24h": used24h, "api_url": apiURL, "has_api": hasAPI, "has_basic": hasBasic, "users_count": usersCount, "api_checked_at": apiCheckedAt, "in_store": inStore, "categories": nodeCategoryMap[id], "status_error": statusError, "health_ok": healthOK, "health_reason": healthReason,
			"traffic_limit_bytes": trafficLimit, "traffic_reset_day": trafficResetDay, "traffic_cycle_start": trafficCycleStart,
			"traffic_cycle_bytes": trafficCycleBytes, "traffic_sampled_at": trafficSampledAt, "traffic_error": trafficError,
			"traffic_suspended": trafficSuspended})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createNodeRequest struct {
	ServerID   string `json:"server_id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	ListenPort int    `json:"listen_port"`
	Domain     string `json:"domain"`
	APIURL     string `json:"api_url"`
	APIKey     string `json:"api_key"`
	InStore    *bool  `json:"in_store"`
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input createNodeRequest
	// server_id is optional *only* when the node is imported through its portal
	// API: in that case the host is derived from the node's own public IP, so the
	// operator can add a node with nothing but the URL and its api_key.
	// listen_port may also be omitted — the portal supplies the authoritative value.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid node payload"})
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.ServerID = strings.TrimSpace(input.ServerID)
	input.Version = strings.TrimSpace(input.Version)
	input.Domain = strings.TrimSpace(input.Domain)
	apiURL := strings.TrimSpace(input.APIURL)
	if input.Name == "" || (apiURL == "" && (input.ServerID == "" || input.ListenPort < 1 || input.ListenPort > 65535)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid node payload"})
		return
	}
	var id string
	var apiCiphertext []byte
	var portalHost, portalName string
	apiUsers := -1
	if apiURL != "" {
		baseURL, err := normalizePortalURLWithToken(apiURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		meta, err := fetchPortalMeta(r.Context(), baseURL, strings.TrimSpace(input.APIKey))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if s.box == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "凭据加密未启用"})
			return
		}
		if apiCiphertext, err = s.box.Seal([]byte(strings.TrimSpace(input.APIKey))); err != nil {
			s.databaseError(w, r, err)
			return
		}
		apiURL = baseURL
		apiUsers = meta.UsersCount
		// The node itself reports its public address and hostname. Those are the
		// best available identity for the auto-created host row.
		portalHost = strings.TrimSpace(meta.Meta.PublicIP)
		portalName = strings.TrimSpace(meta.Meta.ServerName)
		if input.ListenPort <= 0 {
			input.ListenPort = meta.Meta.ListenPort
		}
		if input.Domain == "" {
			input.Domain = meta.Meta.ServerName
		}
	}
	if input.ListenPort < 1 || input.ListenPort > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "监听端口需为 1-65535"})
		return
	}
	// A node imported purely through its portal API does not need a hand-enrolled
	// SSH host: the panel can drive it entirely over the REST API it already
	// holds a key for. Rather than forcing the operator to leave the page and
	// register a host (with an SSH port and user they may not even have), the
	// node's own public IP is recorded as a lightweight host automatically, so
	// the fleet view stays consistent. When an operator later uploads real SSH
	// credentials for that address, metrics collection simply starts working.
	if input.ServerID == "" {
		if apiURL == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择所属主机，或提供节点 API 地址以自动登记"})
			return
		}
		autoID, err := s.ensureHostForImportedNode(r.Context(), portalHost, portalName)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		input.ServerID = autoID
	}
	var hostExists bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM servers WHERE id=$1::uuid AND deleted_at IS NULL)`, input.ServerID).Scan(&hostExists); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server_id 不是合法的 UUID"})
		return
	}
	if !hostExists {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "所属主机不存在或已删除"})
		return
	}
	inStore := true
	if input.InStore != nil {
		inStore = *input.InStore
	}
	// ⚠️ 2026-10-31：没有节点 API 的节点不能进商城。
	//
	// 这条路径（手填名称/端口 + 必选主机）产出的节点没有 api_key，
	// 而买家交付必须先在该节点上开专属账号 —— 开不了。
	// 一旦被标成 in_store，购物车里的订单会永远停在 pending：
	// 每条订阅的 needsAccountProvisioning 恒为 true、每小时重排一次同步，
	// 而节点页看起来一切正常。
	// 产品口径是「节点必须接 API 才能全功能可用」，所以这里直接把
	// in_store 降为 false（自用），而不是拒绝建节点 —— 手工登记仍需可用。
	storeAutoDisabled := false
	if apiURL == "" && inStore {
		inStore = false
		storeAutoDisabled = true
	}
	err := s.db.QueryRow(r.Context(), `INSERT INTO hysteria_nodes (server_id, name, version, listen_port, domain, api_url, api_key, users_count, in_store, api_checked_at) VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), $7, NULLIF($8,-1), $9, CASE WHEN NULLIF($6,'') IS NULL THEN NULL ELSE now() END) RETURNING id`, input.ServerID, input.Name, input.Version, input.ListenPort, input.Domain, apiURL, apiCiphertext, apiUsers, inStore).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机上已存在同端口节点，请先编辑已有节点或换一台主机"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	InvalidateSubscriptionCache()
	s.audit(r.Context(), r, "create_node", "hysteria_node", id, map[string]any{"name": input.Name, "server_id": input.ServerID})
	if inStore {
		// 新节点要给存量订阅都开户，不能等买家下次拉订阅（见 provision_gapfill.go）。
		s.queueProvisioningGapsAsync("create_node")
	}
	payload := map[string]any{"id": id, "in_store": inStore}
	if storeAutoDisabled {
		payload["warning"] = "该节点没有节点 API 密钥，已自动设为「自用」而不是上架商城；要给买家使用请先用「接入节点」绑定 portal_access.json"
	}
	writeJSON(w, http.StatusCreated, payload)
}

type uploadCredentialRequest struct {
	AuthType string `json:"auth_type"`
	Secret   string `json:"secret"`
	// PrivateKey is the legacy field name and still accepted.
	PrivateKey string `json:"private_key_pem"`
	// Username lets the caller correct the placeholder ssh_user written by
	// ensureHostForImportedNode. Empty means "keep it simple: root".
	Username string `json:"username"`
}

func (s *Server) uploadCredential(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	serverID := r.PathValue("id")
	var input uploadCredentialRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential payload"})
		return
	}
	secret := input.Secret
	if secret == "" {
		secret = input.PrivateKey
	}
	switch input.AuthType {
	case "ssh_key":
		if !strings.Contains(secret, "PRIVATE KEY") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "私钥内容不合法（需为 PEM 格式的 SSH 私钥）"})
			return
		}
		// 2026-11-06：用连接时同一个解析器先试一次。原来只查「PRIVATE KEY」字样：带口令的私钥、
		// 复制时截断的私钥都能保存成功，之后采集器每 30 秒连一次都失败 —— 节点的 fail2ban
		// 最终会把面板自己的 IP 封掉（见下方 ssh_user 的说明）。
		if err := sshclient.ValidatePrivateKey([]byte(secret)); err != nil {
			msg := "私钥无法解析（可能复制不完整），请重新粘贴完整的 PEM 私钥"
			if strings.Contains(err.Error(), "passphrase") {
				msg = "不支持带口令的私钥：请先用 ssh-keygen -p -N '' -f <私钥文件> 去掉口令再上传"
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
	case "password":
		if len(secret) < 6 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "SSH 密码至少 6 个字符"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "auth_type 需为 ssh_key 或 password"})
		return
	}
	if s.box == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "凭据加密未启用"})
		return
	}
	sealed, err := s.box.Seal([]byte(secret))
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	var exists string
	if err := s.db.QueryRow(r.Context(), `SELECT id FROM servers WHERE id=$1 AND deleted_at IS NULL`, serverID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	// 自动登记的主机（见 ensureHostForImportedNode）会填入占位 ssh_user
	// `api-only` 并留下空 username。现在既然真的上传了凭据，那个占位符必须被
	// 换成真实用户，否则会出现「凭据有了、却仍然连不上」的诡异状态。
	//
	// 这个坑的代价很大：采集器会每 30 秒用 api-only@<host> 重试一次，节点上的
	// fail2ban 会因此把面板自己的 IP 封掉（实测 ph 累计上万次失败后封禁
	// 198.51.100.32），于是面板与节点彻底失联，而界面上只显示「主机不可达」，
	// 完全看不出是被 fail2ban 封了。
	//
	// username 由调用方给出；给空则退回 root —— 这是本项目所有节点的实际用户，
	// 也符合「不写一个不可能登录的假名」这一底线。
	//
	// 2026-11-03：但「给空就写 root」只该用于替换占位符。界面上传凭据从不带 username，
	// 而编辑弹窗是先 PATCH ssh_user=admin 再上传密钥 —— 原来这一步又把它改回 root，
	// AWS/Ubuntu 镜像拒绝 root 登录，于是每 30 秒失败一次，正是上面说的被 fail2ban 封掉。
	// 现在：给了 username 就用；没给时只在当前是占位符（api-only）或为空时才填 root，其余保留。
	sshUser := strings.TrimSpace(input.Username)
	if _, err := s.db.Exec(r.Context(), `UPDATE servers
SET ssh_user = CASE WHEN $1 <> '' THEN $1
                    WHEN COALESCE(ssh_user, '') IN ('', 'api-only') THEN 'root'
                    ELSE ssh_user END,
    updated_at = now()
WHERE id=$2 AND deleted_at IS NULL`, sshUser, serverID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	var credentialID string
	if err := s.db.QueryRow(r.Context(), `INSERT INTO server_credentials (server_id, auth_type, ciphertext, key_version) VALUES ($1, $2, $3, $4) RETURNING id`, serverID, input.AuthType, sealed, s.box.Version).Scan(&credentialID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "upload_credential", "server", serverID, map[string]any{"credential_id": credentialID, "auth_type": input.AuthType})
	writeJSON(w, http.StatusCreated, map[string]any{"id": credentialID, "auth_type": input.AuthType, "key_version": s.box.Version})
}

func (s *Server) credentialMeta(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	serverID := r.PathValue("id")
	rows, err := s.db.Query(r.Context(), `SELECT id, auth_type, key_version, created_at FROM server_credentials WHERE server_id=$1 ORDER BY created_at DESC LIMIT 10`, serverID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, authType, version string
		var createdAt time.Time
		if err := rows.Scan(&id, &authType, &version, &createdAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{"id": id, "auth_type": authType, "key_version": version, "created_at": createdAt})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) setWriteOps(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "开启写操作需要 X-Confirm-Dangerous: yes"})
		return
	}
	serverID := r.PathValue("id")
	var input struct {
		Allow bool     `json:"allow"`
		Units []string `json:"units"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	units := make([]string, 0, len(input.Units))
	for _, unit := range input.Units {
		unit = strings.TrimSpace(unit)
		if unit == "" {
			continue
		}
		if !safeUnitName.MatchString(unit) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法的服务名: " + unit})
			return
		}
		units = append(units, unit)
	}
	if input.Allow && len(units) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "开启写操作必须给出允许重启的服务白名单（units）"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE servers SET allow_write_ops=$1, write_units=$2, updated_at=now() WHERE id=$3 AND deleted_at IS NULL`, input.Allow, units, serverID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	s.audit(r.Context(), r, "set_write_ops", "server", serverID, map[string]any{"allow": input.Allow, "units": units})
	writeJSON(w, http.StatusOK, map[string]any{"allow_write_ops": input.Allow, "write_units": units})
}

func (s *Server) taskLogs(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：任务日志是 root 命令的原始输出（security_audit 会打出 UID 0 账号与 SSH 配置、
	// uninstall_hy2 打出安装细节），不给只读账号。运维需要它排障，所以只挡 viewer。
	claims, ok := claimsFrom(r.Context())
	if ok && claims.Role == "viewer" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "任务日志含主机内部信息，只读账号不可查看；需要时请让管理员或运维账号打开"})
		return
	}
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	taskID := r.PathValue("id")
	rows, err := s.db.Query(r.Context(), `SELECT sequence, level, message, logged_at FROM task_logs WHERE task_id=$1 ORDER BY sequence ASC LIMIT 200`, taskID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var sequence int
		var level, message string
		var loggedAt time.Time
		if err := rows.Scan(&sequence, &level, &message, &loggedAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{"sequence": sequence, "level": level, "message": message, "logged_at": loggedAt})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// recentMetrics returns a downsampled 24h series for every live host in a single
// response, keyed by server id. The dashboard only needs enough points to draw a
// sparkline, so the query buckets by 10 minutes instead of returning every 30s
// sample — for a fleet of hosts that is the difference between a few hundred rows
// and tens of thousands. It is bounded by the 24h window rather than an explicit row
// limit, since the 10-minute bucketing already caps the row count per host at 144.
//
// 2026-11-02 审计 M2：原来是 server_metrics JOIN servers 再按 sampled_at 过滤，
// 而 sampled_at 不是任何索引的前导列，每 15 秒全表扫 56 万行、把页缓存撑满。
// 改为按主机 LATERAL，走 (server_id, sampled_at) 唯一索引只读 24h 内的行。
// 不新建 sampled_at 单列索引：实测它会让规划器在
// `WHERE server_id=$1 ORDER BY sampled_at DESC LIMIT 1` 上改走该索引反向扫描，
// /api/servers 从 40ms 退化到 500ms（采样稀疏的主机还会更差）。
func (s *Server) recentMetrics(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT s.id, b.bucket, b.cpu_percent, b.memory_percent, b.disk_percent
FROM servers s
CROSS JOIN LATERAL (
    SELECT date_trunc('hour', m.sampled_at) + floor(extract(minute FROM m.sampled_at) / 10) * interval '10 minutes' AS bucket,
           AVG(m.cpu_percent)::float8 AS cpu_percent,
           CASE WHEN SUM(m.memory_total_bytes) > 0
                THEN SUM(m.memory_used_bytes)::float8 * 100 / SUM(m.memory_total_bytes)
                ELSE 0 END AS memory_percent,
           CASE WHEN SUM(m.disk_total_bytes) > 0
                THEN SUM(m.disk_used_bytes)::float8 * 100 / SUM(m.disk_total_bytes)
                ELSE 0 END AS disk_percent
    FROM server_metrics m
    WHERE m.server_id = s.id AND m.sampled_at > now() - interval '24 hours'
    GROUP BY 1
) b
WHERE s.deleted_at IS NULL
ORDER BY s.id, b.bucket ASC`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	grouped := make(map[string][]map[string]any)
	for rows.Next() {
		var serverID string
		var bucket time.Time
		var cpu, memory, disk float64
		if err := rows.Scan(&serverID, &bucket, &cpu, &memory, &disk); err != nil {
			s.databaseError(w, r, err)
			return
		}
		grouped[serverID] = append(grouped[serverID], map[string]any{
			"sampled_at":     bucket,
			"cpu_percent":    clampPercent(cpu),
			"memory_percent": clampPercent(memory),
			"disk_percent":   clampPercent(disk),
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, grouped)
}

// clampPercent keeps aggregate ratios inside 0-100 for the same reason percent()
// does: overlay filesystems can report used above total.
func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (s *Server) serverMetrics(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	serverID := r.PathValue("id")
	rows, err := s.db.Query(r.Context(), `SELECT sampled_at, cpu_percent, memory_used_bytes, memory_total_bytes, disk_used_bytes, disk_total_bytes FROM server_metrics WHERE server_id = $1 AND sampled_at > now() - interval '24 hours' ORDER BY sampled_at ASC`, serverID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var sampledAt time.Time
		var cpu float64
		var memUsed, memTotal, diskUsed, diskTotal int64
		if err := rows.Scan(&sampledAt, &cpu, &memUsed, &memTotal, &diskUsed, &diskTotal); err != nil {
			s.databaseError(w, r, err)
			return
		}
		result = append(result, map[string]any{
			"sampled_at":     sampledAt,
			"cpu_percent":    cpu,
			"memory_percent": percent(memUsed, memTotal),
			"disk_percent":   percent(diskUsed, diskTotal),
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这几个是本页最贵的查询（recentMetrics 是 24h × 全主机
	// LATERAL、servers 有双 LATERAL），原来一个都没调 enforceReadLimit —— 跑飞的轮询或只读令牌可以无限打。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	// 刻意**不过滤** s.deleted_at：任务历史是审计记录，主机被软删除之后，
	// 它上面跑过什么任务恰恰是最该留着的（排查"删之前那台机做了什么"）。
	// 所以这里 JOIN 到已删主机是预期行为，不是遗漏。
	// 2026-11-06：?ids=a,b,c 只取这些任务（批量预设任务的进度轮询用）。原来只能拿最近 100 条：
	// 批量执行期间别处又建了任务，本批的任务被挤出前 100，界面上永远显示「执行中」。
	var ids []string
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, ok := canonicalUUID(strings.TrimSpace(part))
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids 必须是逗号分隔的任务 ID"})
				return
			}
			ids = append(ids, id)
		}
		if len(ids) > 200 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ids 一次最多 200 个"})
			return
		}
	}
	rows, err := s.db.Query(r.Context(), `SELECT t.id, t.server_id, t.node_id, t.operation_type, t.status, t.parameters, t.result_summary, t.error_code, t.created_at, t.started_at, t.finished_at, s.name FROM operation_tasks t JOIN servers s ON s.id = t.server_id
WHERE ($1::uuid[] IS NULL OR t.id = ANY($1::uuid[]))
ORDER BY t.created_at DESC LIMIT CASE WHEN $1::uuid[] IS NULL THEN 100 ELSE 200 END`, ids)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id, serverID, serverName, operationType, status, summary string
		var nodeID, errorCode *string
		var parameters []byte
		var createdAt time.Time
		var startedAt, finishedAt *time.Time
		if err := rows.Scan(&id, &serverID, &nodeID, &operationType, &status, &parameters, &summary, &errorCode, &createdAt, &startedAt, &finishedAt, &serverName); err != nil {
			s.databaseError(w, r, err)
			return
		}
		var decodedParameters any
		if err := json.Unmarshal(parameters, &decodedParameters); err != nil {
			s.databaseError(w, r, err)
			return
		}
		var durationMs int
		if startedAt != nil && finishedAt != nil {
			durationMs = int(finishedAt.Sub(*startedAt) / time.Millisecond)
		}
		result = append(result, map[string]any{
			"id": id, "server_id": serverID, "server_name": serverName, "node_id": nodeID,
			"operation_type": operationType, "status": status, "parameters": decodedParameters,
			"result_summary": summary, "error_code": errorCode, "created_at": createdAt,
			"started_at": startedAt, "finished_at": finishedAt, "duration_ms": durationMs,
		})
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type createTaskRequest struct {
	ServerID      string          `json:"server_id"`
	NodeID        *string         `json:"node_id"`
	OperationType string          `json:"operation_type"`
	Parameters    json.RawMessage `json:"parameters"`
}

// supportedOperations is the single source of truth for what the executor can
// actually carry out. It must stay in sync with executor.dispatch.
//
// The database CHECK constraint on operation_tasks.operation_type allows a wider
// set ('deploy', 'config_apply', 'upgrade' are still listed there). Those are
// intentionally NOT accepted here: the executor has no implementation for them,
// so queueing one produced a task that was guaranteed to fail at execution time
// while the API reported success. Rejecting up front is the honest behaviour.
//
// The CHECK constraint is left as-is rather than narrowed, so that historical
// task rows and any future implementation do not require a migration.
var supportedOperations = map[string]bool{
	"health_check": true,
	"logs":         true,
	"restart":      true,
	"start":        true,
	"stop":         true,
	"reload":       true,
	"enable":       true,
	"disable":      true,
	// 主机预设任务：脚本写死在 executor/presets.go，parameters 只带 preset 名。
	"system_preset": true,
}

// readOnlyOperations can run without allow_write_ops. Everything else mutates
// the host and is gated both here (the confirm header) and in the executor.
var readOnlyOperations = map[string]bool{
	"health_check": true,
	"logs":         true,
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input createTaskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || input.ServerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid task payload"})
		return
	}
	if !supportedOperations[input.OperationType] {
		// 422 rather than 400: the payload is well-formed but names a capability
		// this build does not provide. Callers should not retry.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("本版本不支持操作 %q，可用：health_check、logs、restart、start、stop、reload、enable、disable、system_preset", input.OperationType),
		})
		return
	}
	// 2026-11-06：写类任务（含 system_preset）仅限管理员。原来只校验危险确认头，而 operator 也能带 ——
	// 于是一个 operator 账号就能以 root 跑 system_preset（kernel_reboot / uninstall_hy2），
	// 而主机白名单 write_units 对预设完全不起作用（预设不带 unit）。这与节点侧「重启、升级内核」
	// 已经要求管理员是同一种动作，主机侧漏了。只读任务（health_check / logs）对 operator 照常可用。
	if !readOnlyOperations[input.OperationType] && claims.Role != "admin" {
		s.auditDenied(r, "forbidden_admin_only")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "下发写类主机任务（重启服务、系统预设等）仅限管理员；只读任务（健康检查、拉取日志）不受限制"})
		return
	}
	if !readOnlyOperations[input.OperationType] {
		confirmed := r.Header.Get("X-Confirm-Dangerous")
		if confirmed != "yes" {
			writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "危险操作需要 X-Confirm-Dangerous: yes"})
			return
		}
	}
	if len(input.Parameters) == 0 {
		input.Parameters = json.RawMessage(`{}`)
	}
	if input.OperationType == "system_preset" {
		var p struct {
			Preset string `json:"preset"`
		}
		_ = json.Unmarshal(input.Parameters, &p)
		if _, ok := executor.PresetNames()[p.Preset]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知的预设任务"})
			return
		}
		input.Parameters, _ = json.Marshal(map[string]string{"preset": p.Preset})
	}
	if !json.Valid(input.Parameters) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parameters must be valid JSON"})
		return
	}
	var id string
	// 2026-11-04：只给未删除的主机建任务。执行器只领取 deleted_at IS NULL 的主机（2026-11-03），
	// 原来对已删除主机照样入队、回 201「queued」—— 任务永远排着，不会执行，也不会失败。
	err := s.db.QueryRow(r.Context(), `INSERT INTO operation_tasks (server_id, node_id, operation_type, parameters)
SELECT s.id, $2, $3, $4 FROM servers s WHERE s.id = $1::uuid AND s.deleted_at IS NULL RETURNING id`, input.ServerID, input.NodeID, input.OperationType, input.Parameters).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在或已删除"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_task", "operation_task", id, map[string]any{"operation_type": input.OperationType, "server_id": input.ServerID})
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "queued"})
}

func (s *Server) databaseError(w http.ResponseWriter, r *http.Request, err error) {
	s.logDatabaseError(r, err)
	// 客户端已断开（切页、刷新）导致查询被取消：不是服务端故障，
	// 按 nginx 惯例记 499，访问日志据此降为 INFO。服务端超时
	// （DeadlineExceeded）与独立的数据库错误仍是 500。
	if isClientCanceled(r, err) {
		writeJSON(w, statusClientClosedRequest, map[string]string{"error": "client closed request"})
		return
	}
	// 2026-11-04：22P02（invalid_text_representation）是调用方传了格式错误的值 —— 几乎总是路径里的
	// 畸形 UUID 被 ::uuid 转换。这是客户端错误，回 400；原来一律 500「database request failed」，
	// 前端与告警都把它当成服务端故障。错误原文仍只进日志。
	if isInvalidTextRepresentation(err) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数格式不正确"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database request failed"})
}

func isInvalidTextRepresentation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation 是外键冲突（SQLSTATE 23503）。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// isUniqueViolationOn 判断唯一约束冲突且约束名匹配（SQLSTATE 23505 + ConstraintName）。
func isUniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// isLockNotAvailable 是 lock_timeout / NOWAIT 等不到锁（SQLSTATE 55P03）。
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// percent renders a 0-100 ratio. The result is clamped because overlayfs and bind
// mounts can legitimately report used > total, and an unclamped value would render
// as a >100% meter in the UI.
func percent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	value := float64(used) * 100 / float64(total)
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// Only explicitly trusted peers may supply forwarding information. Walk from
// the nearest proxy toward the client; never accept attacker-controlled left hops.
func requestClientIP(r *http.Request) string { return clientIP(r.RemoteAddr) }

func (s *Server) requestClientIP(r *http.Request) string {
	peer := net.ParseIP(clientIP(r.RemoteAddr))
	if peer == nil {
		return ""
	}
	trusted := func(ip net.IP) bool {
		for _, network := range s.trustedProxyCIDRs {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return peer.String()
		}
		if !trusted(ip) {
			return ip.String()
		}
	}
	return peer.String()
}

// Opt-in gate covers both legacy merged feeds/previews and managed subscriptions.
func subscriptionCredentialPath(path string) bool {
	return path == "/api/subscription" || strings.HasPrefix(path, "/api/subscription/") ||
		path == "/api/subscriptions" || strings.HasPrefix(path, "/api/subscriptions/")
}

// clientIP extracts the caller's address, tolerating both "host:port" and a bare
// host. net.SplitHostPort already handles bracketed IPv6 correctly, so the
// hand-rolled scan that used to live here is gone.
func clientIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 2026-11-06：补上 PUT 与 X-Confirm-Dangerous。原来是 "GET, POST, PATCH, DELETE, OPTIONS" +
		// "Content-Type, Authorization"：12 条 PUT 路由（定时开关、节点分类、主机计费、系统设置…）在跨源部署下
		// 预检直接被拒；而删节点 / 删主机 / 删用户 / 下发任务 / 开写操作都要带 X-Confirm-Dangerous，
		// 浏览器预检失败后操作者只看到一句 CORS 报错。Vary 也要无条件发，否则中间缓存可能把
		// 「给 A 源的响应」复用到 B 源。
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := s.allowedOrigins[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Confirm-Dangerous")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HSTS 只在确认这一跳是 HTTPS 时才发。直接看 r.TLS 是不够的：
		// 生产拓扑里 TLS 在 Caddy 终止，到这个进程已经是明文 HTTP，所以
		// 还要认 X-Forwarded-Proto。若两者都没有就说明确实是明文请求，
		// 此时绝不能发 HSTS —— 浏览器不会在 http 响应上接受它（规范要求
		// 必须走安全通道），但更糟的是会给人一种"已经开了"的错觉。
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			w.Header().Set("Strict-Transport-Security", "max-age=7776000; includeSubDomains")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func redactRequestPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if !strings.EqualFold(part, "sub") {
			continue
		}
		j := i + 1
		for j < len(parts) && parts[j] == "" {
			j++
		}
		if j < len(parts) && strings.EqualFold(parts[j], "view") {
			j++
		}
		for j < len(parts) && parts[j] == "" {
			j++
		}
		if j < len(parts) {
			parts[j] = "[redacted]"
		}
	}
	return strings.Join(parts, "/")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// ⚠️ 2026-10-31：所有 API 响应默认不可缓存。
	//
	// 面板用的是 Bearer token（不是 cookie），浏览器缓存按 URL 存 ——
	// 同一台机器上换人登录后，上一任的 /api/nodes、/api/subscriptions
	// 响应仍可能从磁盘缓存里被读出来（浏览器对没有缓存头的 JSON 会做启发式缓存）。
	// 这些响应里有订阅链接、节点清单、审计与告警，没有一个是"可以留给下一个人看"的。
	// 只在调用方尚未设置时才写，保留个别端点显式覆盖的能力。
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
