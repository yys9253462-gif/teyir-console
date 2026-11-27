const API_BASE = import.meta.env.VITE_API_URL ?? ''

// needBasic 由后端在 /portal-status 上以结构化字段返回（HTTP 400 + need_basic:true）。
// 不要在调用处嗅探错误文案 —— 文案改一个字就会静默失效，这一点上一版就踩过。
export type ApiError = Error & { status: number; needBasic?: boolean; body?: Record<string, unknown> }

// 删除节点 / 把节点移出商城的返回。
//
// 成功时 status='deleted'|'updated'；若买家账号没能从节点侧注销掉，后端返回
// HTTP 207 且 status='*_with_revoke_failures'，并带上 failed 明细。
//
// 注意 207 落在 Response.ok 的 2xx 区间里，request() 会当作成功直接返回 —
// 所以**调用方必须显式判断 status**，否则会对着「账号还在节点上能用」的结果
// 弹一句「已删除」。
export type DeleteNodeResult = {
  status: string
  revoked_count?: number
  failed_count?: number
  failed?: { user_id: string; sub_id: string; reason: string }[]
  error?: string
}

export type ServerStatus = 'unknown' | 'online' | 'warning' | 'offline'
export type NodeStatus = 'unknown' | 'online' | 'warning' | 'offline'
export type TaskStatus = 'queued' | 'running' | 'success' | 'failed' | 'cancelled'

export type Server = {
  id: string
  name: string
  host: string
  ssh_port: number
  ssh_user: string
  region: string
  provider: string
  tags: string[]
  status: ServerStatus
  created_at: string
  updated_at: string
  cpu_percent: number
  memory_percent: number
  disk_percent: number
  // metrics_sampled_at 是上面三个指标的采样时刻（可能为 null = 从未采到）。
  // ⚠️ 2026-10-30：主机详情原来无条件显示 cpu_percent，一台 SSH 停采一周的
  // 机器仍然把一周前的数字当作"当前 CPU 使用率"。现在前端按这个时间判断
  // 新鲜度（超过 15 分钟显示 — 并说明采集已停），不再拿旧值冒充现在。
  metrics_sampled_at?: string | null
  tx_bytes_24h: number
  rx_bytes_24h: number
  // measured_bytes_24h comes from hysteria's own trafficStats (a true rate summed
  // over the window); portal_bytes_24h is the installer portal's bookkeeping
  // counter, which only advances when a client re-authenticates. They are kept
  // apart on purpose — never present one as the other.
  measured_bytes_24h: number
  portal_bytes_24h: number
  latency_ms: number
  allow_write_ops: boolean
  write_units: string[]
  has_credential: boolean
  traffic_api_url: string
  traffic_secret_file: string
}

export type CredentialMeta = {
  id: string
  auth_type: string
  key_version: string
  created_at: string
}

export type TaskLog = {
  sequence: number
  level: 'debug' | 'info' | 'warn' | 'error'
  message: string
  logged_at: string
}

export type Hy2Node = {
  id: string
  server_id: string
  name: string
  version: string
  listen_port: number
  domain: string
  status: NodeStatus
  last_checked_at: string | null
  created_at: string
  updated_at: string
  used_bytes_total: number
  used_bytes_24h: number
  api_url: string
  has_api: boolean
  // Whether this node has the portal's web-session credential (username/password
  // from portal-access.json) bound. has_api alone only reaches /api/v1/; the
  // web-session half — version, BBR, Reality, WARP, AWG, installs, reboot — needs
  // this. The UI must gate those controls on has_basic, not on has_api.
  has_basic: boolean
  users_count: number
  api_checked_at: string | null
  in_store: boolean
  // 节点健康结论（服务端算）：状态非在线，或超过 5 分钟没有一次成功检测，都算失效。
  // 后者是为了兜住「主机 SSH 不可达时节点状态停在旧的在线」这种静默失效。
  status_error?: string
  health_ok?: boolean
  health_reason?: string
  // 整机流量与月度限额（nodetraffic.go）。口径：上传+下载，每月 traffic_reset_day 号北京时间重置。
  traffic_limit_bytes?: number | null
  traffic_reset_day?: number
  traffic_cycle_start?: string | null
  traffic_cycle_bytes?: number
  traffic_sampled_at?: string | null
  traffic_error?: string
  traffic_suspended?: boolean
  // 该节点所属的分类（AI 专用 / 跨境专用…）。
  // 一个节点可以属于多个 —— 这两个用途不互斥。
  // 分类同时决定「商城商品选它时能拿到哪些节点」，所以不只是展示标签。
  categories?: NodeCategory[]
}

// 节点 IP 纯净度（ippurity 包 + /api/nodes/{id}/ip-purity）。
//
// ⚠️ 三个可空布尔的意义：**"未判定"（null）与"判定为否"（false）必须区分**。
// 第三方 API 常常不提供 is_dual_isp，把它当 false 显示成"不是双 ISP"是在骗用户，
// 所以界面上 null 一律显示"未判定"（灰色），只有明确的 true/false 才上色。
export type IPPurity = {
  ip: string
  // residential 家宽 / datacenter 机房 / mobile 移动 / business 专线 / unknown 未知
  connection_type: 'residential' | 'datacenter' | 'mobile' | 'business' | 'unknown'
  is_dual_isp: boolean | null
  // 纯净度风险分 0-100，越大越脏。null = 未判定。
  fraud_score: number | null
  is_proxy: boolean | null
  is_vpn: boolean | null
  is_abuser: boolean | null
  asn: string
  asn_org: string
  raw?: Record<string, unknown>
  // offline 本地判定 / api 第三方 API / manual 人工
  source: string
  // 最近一次检测失败原因。与 checked_at（上次成功）独立。
  error?: string
  checked_at: string | null
  // 存下来的 IP 与当前节点 IP 不一致 → 建议重测。
  stale: boolean
  // 原生 IP：reg_country 注册国（RDAP）、geo_country 所在国（ISO 两位，空串=未查到）。
  // is_native：相同=原生，不同=广播，null=未判定。
  // 评分来源：ipqs = 单 IP 欺诈分（IPQualityScore）；ipapi = 整个 ASN 的滥用比例。
  score_source: 'ipqs' | 'ipapi' | ''
  reg_country: string
  reg_registry: string
  geo_country: string
  is_native: boolean | null
}

// 尚未检测时的响应：checked=false，并给出可读原因（如"该节点没有可检测的公网 IP"）。
export type IPPurityState =
  | { checked: false; ip: string; reason: string }
  | { checked: true; purity: IPPurity }

// 全量快照里的一行（专用「IP 纯净度」页面用）。
export type IPPurityListItem = {
  node_id: string
  node_name: string
  server_name: string
  // 该节点当前应检测的公网 IP；空串表示无可检测 IP。
  host: string
  checked: boolean
  // checked=false 时说明原因（尚未检测 / 无可检测公网 IP）。
  reason?: string
  purity?: IPPurity
}

// 流媒体 / AI 解锁检测（节点本机经 portal 2.8 /api/v1/unlock-check 测得）。
// status：yes 可用 / partial 部分可用 / no 不可用 / unknown 检测失败。
export type UnlockResult = {
  key: string
  name: string
  status: 'yes' | 'partial' | 'no' | 'unknown'
  region: string
  note: string
}

export type NodeUnlock = {
  node_id: string
  results: UnlockResult[]
  // 最近一次检测失败的原因；checked_at 仍是上次成功的时间。
  error: string
  checked_at: string | null
}

// 节点出口设置（自定义出站）。密码不回传，只有 has_password。
export type NodeOutbound = {
  name: string
  type: 'socks5' | 'http' | 'direct' | string
  addr?: string
  url?: string
  username?: string
  has_password: boolean
  insecure?: boolean
  mode?: string
}

export type NodeOutboundState = {
  // global = 所有流量走第一个自定义出站；rules = 默认直连，按域名分流
  mode: 'global' | 'rules' | string
  applying: boolean
  applied: boolean
  apply_error: string
  outbounds: NodeOutbound[]
  rules: { domain: string; outbound: string }[]
  builtin: string[]
}

// 一键安装 HY2 的后台任务（服务端内存，面板重启后不保留）。
// 交付域名：发给买家的订阅 / 交付页链接用的域名。
export type DeliveryDomainState = { value: string; env: string; effective: string; source: 'setting' | 'env' | 'request'; name: string }

export type IPProbeResult = {
  query: string
  ip: string
  resolved_ips?: string[]
  location?: string
  purity?: {
    ip: string
    connection_type: string
    is_dual_isp: boolean | null
    fraud_score: number | null
    is_proxy: boolean | null
    is_vpn: boolean | null
    is_abuser: boolean | null
    asn?: string
    asn_org?: string
    source?: string
    score_source?: string
  }
  purity_error?: string
  reg_country?: string
  geo_country?: string
  native: boolean | null
  node: { id: string; name: string } | null
  unlock?: UnlockResult[]
  unlock_error?: string
  unlock_note?: string
  // 非空 = 没能确定是否为本面板节点（数据库问题），与「不是你的节点」不是一回事。
  node_lookup_error?: string
}

export type InstallJob = {
  id: string
  server_id: string
  status: 'running' | 'success' | 'failed' | 'interrupted'
  step: string
  log: string[]
  error?: string
  node_id?: string
  node_name?: string
  started_at: string
  ended_at?: string
  // 新节点后台地址与登录账号：只在本次安装窗口返回，不落库
  access?: { url: string; username: string; password: string }
}

// 主机到期与费用（server_billing）。
export type ServerBilling = {
  server_id: string
  expires_on: string | null
  monthly_cost: number | null
  currency: string
  auto_renew: boolean
  note: string
}

export type ServerTrendPoint = { t: string; cpu: number | null; mem: number | null; disk: number | null }

export type SSHTestResult = {
  ok: boolean
  ssh_user?: string
  switched_from?: string
  latency_ms: number
  fingerprint: string
  enrolled: boolean
  os: string
  kernel: string
  is_root: boolean
  passwordless_sudo: boolean
}

// 利润报表 / 账号共享 / 被墙检测（2026-10-15）
export type Money = Record<string, number>

export type ProfitReport = {
  nodes: {
    node_id: string; node_name: string; server_id: string; server_name: string
    revenue: Money; cost: Money; profit: Money; subscribers: number; traffic_bytes: number; cost_shared_by: number
  }[]
  totals: { revenue: Money; cost: Money; profit: Money }
  subscriptions: { id: string; label: string; order_id: string; revenue_monthly: number | null; revenue_currency: string; expires_at: string | null }[]
  subscriptions_with_revenue: number
  subscriptions_total: number
  generated_at: string
  // 2026-11-06：分不到在役节点的主机成本 / 订阅收入（已计入 totals）。
  unallocated_cost?: Record<string, number>
  unallocated_revenue?: Record<string, number>
  unallocated_subscription_ids?: string[]
}

export type SharingItem = {
  subscription_id: string; label: string; order_id: string; ip_limit: number
  live_networks: number; day_networks: number; live_ips: string[] | null
  level: 'likely' | 'suspected'; reason: string
}

export type BlockChecks = {
  probe: { server_id: string; name: string; host: string } | null
  // 配置的探测点主机已被删除（probe 为 null）。
  probe_missing?: boolean
  items: {
    node_id: string; node_name: string; node_status: string; target_ip: string; tcp_port: number | null
    tcp_ok: boolean | null; ping_ok: boolean | null; blocked: boolean | null; detail: string; checked_at: string | null
  }[]
}

export type LoginRecord = { ip: string; user_agent: string; first_seen_at: string; last_seen_at: string; login_count: number; current: boolean }
// last_error / last_attempt：后端一直在返回（失败重试、重试用尽的说明都写在 last_error 里），
// 原来类型里没有、界面也不显示 —— 每次都失败的巡检看起来一切正常（2026-11-04）。
export type ScheduleItem = { key: string; label: string; interval: string; enabled: boolean; last_run: string | null; next_run: string | null; running: boolean; last_attempt?: string | null; last_error?: string }

export type TelegramSettings = {
  control_private_only?: boolean
  control_routes?: number
  menu_commands?: { command: string; description: string }[]
  token_configured: boolean
  chat_id: string
  bot?: { bot_username: string; last_poll_ok: string | null; last_error: string; last_error_at: string | null; last_from_chat: string; last_from_at: string | null; last_reply_err: string }
}

export type AutoHideState = { enabled: boolean; after_minutes: number; hidden: { node_id: string; name: string; reason: string; since: string }[] }

export type PurityKeyName = 'ipapi_is' | 'ipqs' | 'cloudflare' | 'telegram'

// IP 检测密钥状态：永远不含明文，只有是否配置、来源与末四位。
export type PurityKeysState = Record<PurityKeyName, { configured: boolean; source: '' | 'web' | 'env'; masked?: string }>

export type IPPurityList = {
  items: IPPurityListItem[]
  count: number
}

// NodeCategory 是节点分类。既是分组，也是商城圈定节点范围的依据。
export type NodeCategory = {
  id: string
  // slug 是商城侧的引用键（product_node_plans.node_category 存的就是它）。
  // 改了会让商城已有的配置失配，所以后台改它时要提醒。
  slug: string
  name: string
  description: string
  sort_order: number
  enabled: boolean
  // 当前在役节点里属于该分类的数量。由后端现算而不是存列 ——
  // 存一份计数必然与实际情况漂移。
  node_count: number
  created_at?: string
  updated_at?: string
}

// 单个节点的能力快照（来自节点侧 /api/v1/capabilities）。
//
// ⚠️ 区分三种「没有能力」的情形，它们的处理动作完全不同：
//   supported=false            → 节点门户太旧（没这个端点），要去升级节点
//   reachable=false + error    → 网络/密钥问题，要去查节点
//   reachable=true 但项为 false → 节点就是没装那个东西，正常状态
export type NodeCapabilities = {
  node_id: string
  node_name: string
  // false means the panel could not decrypt the configured API credential.
  available?: boolean
  reachable: boolean
  error?: string
  supported: boolean
  portal_version?: string
  // 节点侧协议段是动态 map：老门户可能没有某个键，未来也可能新增未知协议。
  // 前端必须按「可能缺失 / 可能不是对象」读取，不能假设字段齐全。
  protocols?: Record<string, {
    installed?: boolean
    active?: boolean
    configured?: boolean
    port?: number
    domain?: string
    sni?: string
    state_source?: string
    state_error?: string
    // VLESS Encryption：capabilities 不含客户端密钥，只有 Basic 会话能确认。
    // delivery_error 非空 ⇒ 该通道不可交付（不生成客户端配置）。
    client_encryption_present?: boolean
    delivery_error?: string
  }>
  extras?: Record<string, Record<string, unknown>>
  node?: Record<string, unknown>
}

export type NodeCapabilitiesResult = {
  nodes: NodeCapabilities[]
  fetched: number
  reachable: number
  outdated: number
}

export type PortalPreview = {
  listen_port: number
  domain: string
  public_ip: string
  hop_port_range: string
  cert_type: string
  users_count: number
  suggested_name: string
  server_id: string
  server_name: string
  api_url: string
  // True when the pasted URL carried a /{token}/ segment, so the web-session
  // endpoints can be addressed at all.
  has_path_token: boolean
  // Outcome of a live check-version call with the supplied Basic credential.
  // basic_ok false with basic_error empty simply means none was supplied.
  basic_ok: boolean
  basic_error: string
  // True when no enrolled host matches this node's address, so the importer may
  // submit without picking one — the server records a host from the node's IP.
  auto_host: boolean
}

// ---- Node-side user management (portal /api/v1/users/*) ----
//
// traffic_limit_bytes / traffic_used_bytes come from the portal's own
// bookkeeping, which only advances when a client re-authenticates. They are
// quota accounting, NOT a live traffic rate — never render them as one.
export type NodeUser = {
  online_devices?: number | null
  measured_bytes?: number | null
  measurement_ok?: boolean
  measurement_started_at?: string
  measurement_sources?: Record<string, unknown>
  measurement_gaps?: unknown[]
  user_id: string
  expires_at: number
  active: boolean
  traffic_limit_bytes: number
  traffic_used_bytes: number
  ip_limit: number
  created_at: number
  status: string
}

export type NodeUserList = {
  node_id: string
  node_name: string
  count: number
  users: NodeUser[]
}

// The portal returns a ready-to-use subscription bundle on create. The password
// and URIs are shown once; the panel does not store them.
export type NodeUserCreated = {
  node_id: string
  user_id: string
  password: string
  expires_at: number
  ip_limit: number
  traffic_gb: number
  uri: string
  reality_uri: string
  clash: string
  sing_box: string
}

export type NodeUserRenewed = {
  node_id: string
  user_id: string
  expires_at: number
  limit_bytes: number
}

// ---- Node-side gost proxy services (portal /api/v1/proxy-services/*) ----
export type ProxyProtocol = 'socks5' | 'http' | 'https'

export type NodeProxyService = {
  id: string
  type: string
  host?: string
  port: number
  username: string
  password: string
  note: string
  url?: string
  format?: string
  created_at?: number
  gost_active?: boolean
}

export type NodeProxyList = {
  node_id: string
  node_name: string
  count: number
  gost_installed?: boolean
  gost_active?: boolean
  host?: string
  services: NodeProxyService[]
}

// ---- Node overview (what the portal key can actually reach) ----
//
// portal.py serves exactly three GET subpaths under /api/v1/: node/meta,
// users/list, proxy-services/list. Everything else under that prefix is a 404 —
// notably check-version / reality-status / bbr-status / warp-status, which live
// behind the portal's web-session prefix and cannot be read with the Bearer key.
// So this overview is built from those three reads only.
//
// auth_password / obfs_password are NOT part of this payload: auth_password is
// the node's master credential and never leaves the server.
export type PortalMeta = {
  public_ip: string
  server_name: string
  listen_port: number
  subscription_port: number
  hop_port_range: string
  cert_type: string
  is_insecure: boolean
  has_obfs: boolean
  has_pin: boolean
}

export type NodeOverviewSummary = {
  users_count: number
  users_active: number
  users_unlimited: number
  traffic_used_bytes: number
  traffic_limit_bytes: number
  proxy_services_count: number
  has_obfs: boolean
  is_insecure: boolean
  hop_port_range: string
  subscription_port: number
  cert_type: string
}

// One read's outcome. `available:false` means that endpoint could not be read;
// the UI must show the reason rather than an empty value.
export type ProbeState = { available: boolean; error?: string }

export type NodeOverview = {
  node_id: string
  node_name: string
  meta: PortalMeta
  summary: NodeOverviewSummary
  users: NodeUser[]
  proxy_services: NodeProxyService[]
  probes: { meta: ProbeState; users: ProbeState; proxy_services: ProbeState }
  // Human-readable explanation of what the key cannot reach, rendered verbatim.
  unreachable: string
}

// ---- Node status via the portal's web session (HTTP Basic) ----
//
// This is the half the Bearer key cannot reach. Every endpoint below lives under
// the portal's /{token}/ prefix and is guarded by is_authenticated(), which
// accepts an HTTP Basic header built from portal-access.json's username/password.
// With that credential bound on the node, all of these become readable — this is
// what makes "manage a node purely over the API" actually true.
export type PortalProbe<T> = {
  state: 'healthy' | 'warn' | 'unknown' | 'unsupported'
  error?: string
  payload?: T
}

export type PortalVersionInfo = {
  core_current: string
  core_latest: string
  core_has_update: boolean
  portal_current: string
  portal_latest: string
  portal_has_update: boolean
  awg_installed: boolean
  awg_current: string
  awg_latest: string
  awg_has_update: boolean
}

export type PortalBBRInfo = {
  current: string
  qdisc: string
  kernel: string
  configured: string
  need_reboot: boolean
}

export type PortalRealityInfo = {
  installed: boolean
  active: boolean
  // 管理员的 uri / uuid / short_id / 二维码不再下发（2026-11-04），只保留描述通道状态的字段。
  config: {
    flow: string
    sni: string
    port: number
  }
}

export type PortalWarpInfo = {
  installed: boolean
  enabled: boolean
  connected: boolean
  ip: string
  rules: string[]
}

export type PortalAWGInfo = {
  installed: boolean
  active: boolean
  line: string
  port: string
  endpoint: string
  peer_count: number
  ctl: boolean
  peers: { name: string }[]
}

export type PortalGostInfo = {
  gost_installed: boolean
  gost_active: boolean
  host: string
  services: { id: string; type: string; port: number }[]
}

// portal's own 10-second sliding window. Unlike the collector's trafficStats
// path this is a true rate that needs no ?clear=true draining, so it is safe to
// read on demand and never disturbs quota accounting.
export type PortalSpeedInfo = {
  node_tx: number
  node_rx: number
  users: Record<string, { tx: number; rx: number }>
}

export type PortalNodeStatus = {
  node_id: string
  node_name: string
  origin: string
  checked_at: string
  probes: {
    version: PortalProbe<PortalVersionInfo>
    bbr: PortalProbe<PortalBBRInfo>
    reality: PortalProbe<PortalRealityInfo>
    vmess?: PortalProbe<{ installed: boolean; active: boolean; config?: { domain?: string; port?: number; ws_path?: string; sni?: string } }>
    // vlessenc 是 2026-10-24 新增端点：老门户没有 ⇒ unsupported（按钮保留并说明）。
    vlessenc?: PortalProbe<{ installed: boolean; active: boolean; config?: { domain?: string; port?: number; sni?: string } }>
    // anytls 是 2026-10-30 新增端点（独立 sing-box 进程）：老门户没有 ⇒ unsupported。
    anytls?: PortalProbe<{ installed: boolean; enabled: boolean; active: boolean; version?: string }>
    warp: PortalProbe<PortalWarpInfo>
    awg: PortalProbe<PortalAWGInfo>
    gost: PortalProbe<PortalGostInfo>
    speed: PortalProbe<PortalSpeedInfo>
  }
}

// Verbs the panel is willing to forward. Deliberately a closed set rather than a
// passthrough: the portal also exposes install-* endpoints that run remote
// scripts, and those must be spelled out server-side, not assembled by the UI.
export type PortalVerb =
  | 'upgrade_core'
  | 'upgrade_portal'
  | 'upgrade_awg'
  | 'set_bbr'
  | 'reality_toggle'
  | 'reality_reset'
  | 'vmess_enable'
  | 'vmess_disable'
  | 'vlessenc_enable'
  | 'vlessenc_disable'
  | 'anytls_enable'
  | 'anytls_disable'
  | 'install_xray'
  | 'install_warp'
  | 'warp_toggle'
  | 'warp_add_rule'
  | 'warp_del_rule'
  | 'warp_reset_rules'
  | 'install_gost'
  | 'manage_proxy'
  | 'install_awg'
  | 'manage_awg'
  | 'manage_outbounds'
  | 'reboot'

export type PortalActionRequest = {
  verb: PortalVerb
  params?: Record<string, string>
}

// 每个节点门户是否已是 GitHub 最新（逐文件比对指纹）。up_to_date 为 null = 无法判断。
export type PortalUpdateNodeStatus = {
  node_id: string
  name: string
  portal_version: string
  up_to_date: boolean | null
  outdated_files: string[]
  error?: string
}
export type PortalUpdateStatus = { error?: string; nodes: PortalUpdateNodeStatus[]; checked_at: string }

export type PortalUpdateBatch = {
  id: string
  cancel_requested?: boolean
  status: 'running' | 'completed' | 'interrupted'
  results: { node_id: string; name: string; status: string; stage: string; error?: string }[]
  expected: Record<string, string>
  created_at: string
  updated_at: string
}

export type PortalActionResult = {
  status: string
  verb: string
  label: string
  result: unknown
}

// ---- Merged subscription ----
//
// One link carrying every node, built from each node's portal master password.
// The link is therefore as sensitive as those passwords, and `included` /
// `excluded` carry routing facts only — never a credential.
export type SubscriptionFormat = 'clash' | 'singbox' | 'uri'

export type SubscriptionIncludedNode = {
  id: string
  name: string
  host: string
  port: number
  sni: string
  insecure: boolean
  has_obfs: boolean
  hop_range: string
}

export type SubscriptionExcludedNode = {
  node_id: string
  name: string
  reason: string
}

export type SubscriptionState = {
  token: string
  url: string
  path: string
  label: string
  enabled: boolean
  created_at: string
  last_accessed_at: string | null
  access_count: number
  formats: SubscriptionFormat[]
  included: SubscriptionIncludedNode[]
  excluded: SubscriptionExcludedNode[]
}

// ---- Multi-subscription storefront ----
//
// The panel sells more than its own single link: each shop order gets its own
// token with its own expiry and device quota. `expired` is computed server-side
// so the UI never has to trust the browser clock for an access decision.
export type ManagedSubscription = {
  hysteria_online_devices?: number | null
  online_count_nodes?: number
  online_count_scope?: string
  id: string
  token: string
  label: string
  enabled: boolean
  expired: boolean
  expires_at: number
  expires_at_text: string
  // Whole days remaining. -1 means "no expiry" (perpetual), which is NOT the same
  // as 0 ("expires today") — guard with `>= 0` before showing a countdown.
  days_left: number
  ip_limit: number
  active_ip_count: number
  // 在线 IP 明细（当前配置的滑窗内拉取过订阅的客户端地址）。
  // 与 active_ip_count 同源，所以两者永远一致。
  active_ips: { ip: string; seen_at: string }[]
  // 最近鉴权 IP（节点侧 180 秒窗）；不是实时连接清单。
  online_ips: { ip: string; last_seen: number; node_id: string; node_name?: string }[]
  online_ip_count: number
  // 读表是否成功。false 时上面的空数组意为"未知"而不是"确实没人在线"，
  // 界面必须区分 —— 把"没采到"显示成"0 人在线"是本项目反复要避开的坑。
  current_online_ips?: { ip: string; protocol: string; last_seen: number; node_id: string; node_name: string }[]
  current_online_ips_readable?: boolean
  current_online_ips_complete?: boolean
  current_online_hy2_nodes?: number
  current_online_reality_nodes?: number
  current_online_expected_nodes?: number
  current_online_errors?: string[]
  current_online_sampled_at?: string | null
  online_ips_readable: boolean
  auto_delete: boolean
  order_id: string
  user_email: string
  note: string
  created_at: string
  last_accessed_at: string | null
  access_count: number
  delivery_url: string
  subscription_url: string
  // 用量只计算真实计量；旧口径估算单列展示，不参与合计。
  // used_bytes 为 **null** 表示从未采到过 —— 与"用量确实是 0"不同，
  // 界面必须区分（否则会显示成「0 B」，让人以为客户没在用）。
  used_bytes: number | null
  limit_bytes: number | null
  // 旧估算与真实计量分开返回，防止重复累计。
  usage_legacy_bytes: number
  usage_metered_bytes: number
  usage_current_ok?: boolean
  usage_history_gaps?: number
  usage_ok: boolean
  usage_reason: string
  usage_sampled_at: string | null
  usage_scope: 'since_measurement_started'
  usage_started_at: string | null
  usage_started_at_latest: string | null
  usage_expected_nodes: number
  usage_measured_nodes: number
  // 分类圈定：这条订阅只包含哪个分类的节点。
  // category_id 为空 = 不圈定（含全部已上架节点）。
  // 手工开通时自选的节点（非空时优先于分类）
  node_ids?: string[]
  // category_name 由后端一并给出，前端不必再拉一次分类列表去对照。
  category_id: string | null
  category_name: string
}

export type StoreIntegration = {
  enabled: boolean
  key_hint: string
  create_endpoint: string
  renew_endpoint: string
  revoke_endpoint: string
  query_endpoint: string
  auth_header: string
  ip_window_hours: number
  expiry_grace_hours: number
}

export type SubscriptionCenter = {
  subscriptions: ManagedSubscription[]
  // 订阅覆盖的节点池。订阅按节点**实时生成**，所以这不是"某条订阅的节点数"，
  // 而是"所有订阅共享的节点池"：node_count = 在役节点，
  // node_in_store = 其中进了商城、会推给买家的。
  fleet: {
    node_count: number
    node_in_store: number
    // 非空 = 节点数没读到（此时两个计数都不可信）。
    error?: string
  }
  store: StoreIntegration
}

// What the public delivery page is allowed to know. No credential ever appears
// here — only routing facts and capacities.
export type DeliveryView = {
  label: string
  enabled: boolean
  expired: boolean
  expires_at: number
  expires_at_text: string
  // Whole days remaining. -1 means "no expiry" (perpetual), which is NOT the same
  // as 0 ("expires today") — guard with `>= 0` before showing a countdown.
  days_left: number
  ip_limit: number
  active_ip_count: number
  ip_window_hours: number
  // 跨节点统一设备上限：此刻被拦截的网络数（只在拦截模式下非零）。
  device_limit_blocked?: number
  node_count: number
  nodes: SubscriptionIncludedNode[]
  excluded: SubscriptionExcludedNode[]
  renew_url: string
  subscription_url?: string
  clash_url?: string
  singbox_url?: string
  uri_url?: string
  // Same token on other domains, for when the primary domain is blocked.
  backup_subscription_urls?: string[]
}

// Service control verbs the executor actually implements. Kept as a union so the
// compiler catches any UI that tries to dispatch something the backend rejects.
//
// Note: the database CHECK constraint still lists 'deploy' / 'config_apply' /
// 'upgrade', but the executor has no implementation for them and the API now
// rejects them with 422. They are intentionally omitted here so the UI cannot
// offer a button that is guaranteed to fail.
export type ServiceOperation =
  | 'health_check'
  | 'logs'
  | 'restart'
  | 'start'
  | 'stop'
  | 'reload'
  | 'enable'
  | 'disable'
  // 主机预设任务：脚本写死在服务端（executor/presets.go），parameters.preset 选哪一个。
  | 'system_preset'

export type OperationTask = {
  id: string
  server_id: string
  server_name: string
  node_id: string | null
  // Historical rows may carry a legacy operation_type that is no longer
  // dispatchable, so the read path stays permissive while writes are strict.
  operation_type: ServiceOperation | (string & {})
  status: TaskStatus
  parameters: Record<string, unknown>
  result_summary: string
  error_code: string | null
  created_at: string
  started_at: string | null
  finished_at: string | null
  duration_ms: number
}

export type Health = { status: 'ok' | 'unavailable'; time?: string }

// Live state of one systemd unit on a host, as reported by `systemctl show`.
export type ServiceUnitStatus = {
  unit: string
  active: string
  enabled: string
  since: string
  error?: string
}

export type ServiceStateResponse = {
  units: ServiceUnitStatus[]
  // Set when the whole read could not be attempted (no credential, no whitelist).
  note?: string
  error?: string
}

export type Alert = {
  id: string
  type: string
  severity: 'info' | 'warning' | 'critical'
  status: 'open' | 'acknowledged' | 'resolved'
  server_name: string
  host: string
  node_name: string
  node_port: number
  // 2026-11-08：节点类告警带节点 ID，告警页「确认机器已下线」按它删除节点。
  node_id: string
  message: string
  resolved_at: string | null
  first_seen_at: string
  last_seen_at: string
}

// One row of an alert's timeline. `event_type` is the machine state, the panel
// turns it into a sentence — an operator should never have to read `reopened`.
export type AlertEvent = {
  id: number
  event_type: 'triggered' | 'repeated' | 'reopened' | 'acknowledged' | 'resolved'
  details: Record<string, unknown>
  created_at: string
}

export type AuditEntry = {
  id: number
  action: string
  resource_type: string
  resource_id: string
  actor: string
  ip: string
  created_at: string
}

// 待补删的孤儿节点账号（删订阅/节点时没能从节点侧注销的）。
//
// ⚠️ has_credential 是本类型的核心字段：
//   删除那一刻若留到了凭据快照，就能一键补删；
//   没留到就**物理上无解**，只能人工上机删。
//   前端必须据此分开展示 —— 把"只能人工"混进"可一键补删"会让运维白点很多次。
export type OrphanAccount = {
  id: string
  node_id: string
  node_name: string
  // 删除那一刻记下的 portal 地址，供人工上机时定位。
  node_api_url: string
  user_id: string
  subscription_id: string | null
  has_credential: boolean
  reason: string
  attempts: number
  last_attempt_at: string | null
  resolved_at: string | null
  resolved_note: string
  created_at: string
  // 节点已从控制台删除时为 true：只有这类才允许「确认机器已下线，整台结案」。
  node_deleted: boolean
}

// GET /api/orphans 的完整响应。
// note / how_it_happens 是后端写给运维看的说明文案，直接展示即可 ——
// 它们是"这个功能为什么会存在"的最好解释，比前端自己造一段更准。
export type OrphanListResponse = {
  orphans: OrphanAccount[]
  count: number
  pending: number
  note?: string
  how_it_happens?: string
}

// 账号对账（reconcile）的返回值。
// failed 里带每个失败节点的名字与原因 —— 部分成功时必须让运维看到是**哪一台**没刷上，
// 否则「ok_nodes: 4」看着像全好了。
export type ReconcileResult = {
  ok_nodes: number
  failed_nodes: number
  failed?: { node: string; error: string }[]
  message?: string
}

// 全量对账的返回值：在单条结果之上多两条汇总。
// ⚠️ 字段名是 subscriptions（不是 total）—— 按后端实际返回写的，
// 第一版我按 total 猜，TypeScript 当场报错才改过来。
export type ReconcileAllResult = ReconcileResult & {
  subscriptions: number
  details?: { label: string; ok_nodes: number; failed_nodes: number }[]
}

export type UserAccount = {
  id: string
  email: string
  role: 'admin' | 'operator' | 'viewer'
  created_at: string
  last_login_at: string | null
}

export type RuntimeStatus = {
  servers: number
  nodes: number
  servers_with_credential: number
  queued_tasks: number
  last_sample_at: string | null
  collector_interval: number
  executor_interval: number
  jwt_ttl_minutes: number
}

export type TrafficBucket = {
  bucket: string
  rx_bytes: number
  tx_bytes: number
}

export type MetricSample = {
  sampled_at: string
  cpu_percent: number
  memory_percent: number
  disk_percent: number
}

export type Session = {
  token: string
  email: string
  role: string
}

// ---- 3X-UI 面板对接 ----
//
// 3X-UI 是独立的 Xray 面板，控制台用其管理员用户名/密码登录后代理其 API。
// 密码只在添加时上行一次、之后只在服务端密文里存在，任何读接口都不回显 ——
// 所以这里没有 password 字段，只有 has 语义上的“已登记”。
export type XuiPanel = {
  id: string
  name: string
  base_url: string
  username: string
  // ok / error / unknown。error 时 last_error 带原因。
  last_status: string
  last_error: string
  last_checked_at: string | null
  inbounds_count: number
  clients_count: number
  online_count: number
  created_at: string
  updated_at: string
  // 地区与商家：添加面板时按面板地址里的公网 IP 自动识别填入。
  // 手工在界面上改过之后，来源会变成 manual，此后不再被自动覆盖。
  region: string
  provider: string
  name_source: GeoSource
  region_source: GeoSource
  provider_source: GeoSource
}

// GeoSource 说明一个字段的值是谁定的：
//   auto   由 IP 识别自动填入，后续识别可以更新它
//   manual 人工填写/修改过，自动识别永不覆盖
export type GeoSource = 'auto' | 'manual'

export type XuiPreview = {
  base_url: string
  inbounds_count: number
  clients_count: number
  online_count: number
}

// 3X-UI 各协议字段名不统一，clients[] 原样透传成字典。
export type XuiClientRaw = Record<string, unknown>

export type XuiInbound = {
  id: number
  port: number
  protocol: string
  remark: string
  enable: boolean
  up: number
  down: number
  total: number
  expiryTime: number
  clients: XuiClientRaw[]
}

// 服务端展开后的客户端视图（一个客户端 = 一个订阅账号）。
export type XuiClientStatus = {
  email: string
  client_id: string
  inbound_id: number
  inbound_name: string
  protocol: string
  online: boolean
  up: number
  down: number
  total: number
  expiry_time: number
}

export type XuiInboundsResponse = {
  panel_id: string
  panel_name: string
  inbounds: XuiInbound[]
  clients: XuiClientStatus[]
  onlines: string[]
  // 2026-11-06：在线列表没取到时给出原因；此时各客户端的「在线」都是未知，不是离线。
  onlines_error?: string
}

const TOKEN_KEY = 'nodeops.session'

/** 破坏性操作只对管理员开放（服务端 requireAdmin），前端同步禁用按钮，避免 operator 点了才收到 403。 */
export function isAdminSession(): boolean {
  return loadSession()?.role === "admin"
}

// portalActionAdminOnly 与后端 portalActionAdminOnly（server/internal/httpapi/portalaction.go）保持同一条规则：
// 危险动词、install_*、出口管理、代理管理以及除只读/增删节点客户端外的 AmneziaWG 操作只限管理员。
export function portalActionAdminOnly(verb: string, dangerous?: boolean, params?: Record<string, string>): boolean {
  const cleanVerb = verb.trim()
  if (Boolean(dangerous) || cleanVerb.startsWith('install_') || cleanVerb === 'manage_outbounds' || cleanVerb === 'manage_proxy') {
    return true
  }
  if (cleanVerb === 'manage_awg') {
    const action = (params?.action ?? '').trim()
    return action !== '' && action !== 'state' && action !== 'peer_add' && action !== 'peer_del'
  }
  return false
}
export const ADMIN_ONLY_HINT = '仅管理员可操作（会在节点上以 root 执行脚本）'

export function loadSession(): Session | null {
  try {
    const raw = window.localStorage.getItem(TOKEN_KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw) as Session
    if (!parsed?.token) return null
    return parsed
  } catch {
    return null
  }
}

export function saveSession(session: Session) {
  try {
    window.localStorage.setItem(TOKEN_KEY, JSON.stringify(session))
  } catch {
    // 浏览器禁用本地存储时不中断登录流程
  }
}

export function clearSession() {
  try {
    window.localStorage.removeItem(TOKEN_KEY)
  } catch {
    // 忽略存储受限环境异常
  }
}

function authHeaders(extra: Record<string, string> = {}): Record<string, string> {
  const session = loadSession()
  if (!session) return extra
  return { ...extra, Authorization: `Bearer ${session.token}` }
}

// 会话失效统一处理（2026-10-31）：任何带会话的请求拿到 401，就清掉本地会话并通知 App 回到登录页。
//
// 以前只在 App 的整体刷新里判断 401；刷新改成「每一路各失败各的」（.catch(() => null)）之后，
// 401 被逐个吞掉，再也传不到那段判断 —— 令牌过期后页面不跳登录，只一直显示「读取失败 401」
// 和「主机、运维任务、节点未更新」。放在这里，所有页面、所有请求都生效，不依赖调用方。
// 例外：修改密码时「当前密码不正确」也是 401，那是用户输错，不是会话失效。
export const SESSION_EXPIRED_EVENT = 'nodeops:session-expired'
const NON_SESSION_401_PATHS = new Set(['/api/auth/change-password', '/api/login'])

// 默认请求超时上限：常规接口 120 秒（兼容批量纯净度检测、门户操作等耗时请求），公开轻接口 30 秒。
const DEFAULT_REQUEST_TIMEOUT_MS = 120_000
const PUBLIC_REQUEST_TIMEOUT_MS = 30_000

async function fetchWithTimeout(input: string, init?: RequestInit, timeoutMs = DEFAULT_REQUEST_TIMEOUT_MS): Promise<Response> {
  const controller = new AbortController()
  let timedOut = false
  const timer = window.setTimeout(() => {
    timedOut = true
    controller.abort()
  }, timeoutMs)
  const external = init?.signal
  const onExternalAbort = () => controller.abort()
  if (external) {
    if (external.aborted) {
      window.clearTimeout(timer)
      controller.abort()
    } else {
      external.addEventListener('abort', onExternalAbort, { once: true })
    }
  }
  try {
    return await fetch(input, { ...init, signal: controller.signal })
  } catch (err) {
    if (timedOut) {
      throw new Error('请求超时，请检查网络或稍后重试')
    }
    throw err
  } finally {
    window.clearTimeout(timer)
    external?.removeEventListener('abort', onExternalAbort)
  }
}

function normalizeHeaders(headers: HeadersInit | undefined, body: BodyInit | null | undefined): Record<string, string> {
  const merged: Record<string, string> = { ...authHeaders() }
  if (headers) {
    if (headers instanceof Headers) {
      headers.forEach((value, key) => { merged[key] = value })
    } else if (Array.isArray(headers)) {
      for (const [key, value] of headers) merged[key] = value
    } else {
      Object.assign(merged, headers)
    }
  }
  if (typeof body === 'string') {
    const hasContentType = Object.keys(merged).some((key) => key.toLowerCase() === 'content-type')
    if (!hasContentType) merged['Content-Type'] = 'application/json'
  }
  return merged
}

// 与后端 viewerMayUseRequest（server/internal/httpapi/server.go）保持一致的只读角色前置校验。
export function viewerMayUseRequest(method: string, path: string): boolean {
  const upper = method.toUpperCase()
  const cleanPath = path.split('?')[0]
  if (upper === 'POST' && cleanPath === '/api/auth/change-password') return true
  if (!['GET', 'HEAD', 'OPTIONS'].includes(upper)) return false
  // 历史 GET /api/nodes/{id}/api 会刷新并写回节点元数据，属于写副作用请求。
  if ((upper === 'GET' || upper === 'HEAD') && cleanPath.startsWith('/api/nodes/') && cleanPath.endsWith('/api')) {
    return false
  }
  return true
}

function parseJsonText<T>(text: string): T {
  try {
    return JSON.parse(text) as T
  } catch {
    throw new Error('服务端返回格式异常（非预期 JSON），请检查反向代理或后端服务状态')
  }
}

async function parseJsonResponse<T>(response: Response): Promise<T> {
  const text = await response.text()
  if (!text.trim()) {
    throw new Error('服务端返回空响应，请检查反向代理或后端服务状态')
  }
  return parseJsonText<T>(text)
}

function handleUnauthorized(path: string, status: number, sentToken: string) {
  if (status !== 401 || NON_SESSION_401_PATHS.has(path.split('?')[0])) return
  const current = loadSession()
  // 仅当 401 对应的请求令牌仍是当前生效会话令牌时才清理，防止旧会话迟到的 401 误踢刚重新登录的新会话。
  if (!current || (sentToken && current.token !== sentToken)) return
  terminalAuthEpoch++
  clearSession()
  void queueTerminalCookie(clearTerminalCookie).catch(() => undefined)
  window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT))
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const method = (init?.method || 'GET').toUpperCase()
  const session = loadSession()
  if (session?.role === 'viewer' && !viewerMayUseRequest(method, path)) {
    throw Object.assign(new Error('只读账号不能执行写操作'), { status: 403 })
  }
  const sentToken = session?.token ?? ''
  const base = API_BASE.endsWith('/') ? API_BASE.slice(0, -1) : API_BASE
  const headers = normalizeHeaders(init?.headers, init?.body)
  const response = await fetchWithTimeout(`${base}${path}`, { ...init, headers })
  if (!response.ok) {
    handleUnauthorized(path, response.status, sentToken)
    const error = new Error(`请求失败 (${response.status})`) as ApiError
    error.status = response.status
    try {
      const body = await response.json()
      if (body?.error) error.message = `${body.error} (${response.status})`
      // 后端用这个字段区分「还没绑凭据」（设置状态）与真正的失败。
      if (body?.need_basic) error.needBasic = true
      // 保留完整的错误体：部分接口带结构化字段（如 SSH 指纹变化时的新旧指纹）。
      if (body && typeof body === 'object') error.body = body
    } catch {
      // ignore
    }
    throw error
  }
  if (response.status === 204) {
    return undefined as T
  }
  const text = await response.text()
  if (!text) return undefined as T
  return parseJsonText<T>(text)
}

// Subscription previews are returned as text in whichever format was asked for,
// so they bypass the JSON decoder.
async function requestText(path: string): Promise<string> {
  const sentToken = loadSession()?.token ?? ''
  const base = API_BASE.endsWith('/') ? API_BASE.slice(0, -1) : API_BASE
  const response = await fetchWithTimeout(`${base}${path}`, { headers: authHeaders() })
  if (!response.ok) {
    handleUnauthorized(path, response.status, sentToken)
    const error = new Error(`请求失败 (${response.status})`) as ApiError
    error.status = response.status
    // 2026-11-06：与 request() 一样带上后端的错误原文。原来只有「请求失败 (429)」，
    // 预览被限流、节点构建失败时看不出原因。
    try {
      const body = await response.json()
      if (body?.error) error.message = `${body.error} (${response.status})`
    } catch {
      // 非 JSON 错误体：保留状态码文案
    }
    throw error
  }
  return response.text()
}

// Operations that mutate the host. These require the confirm header; everything
// else (health_check, logs) is read-only.
const DANGEROUS_OPS = new Set<string>(['restart', 'start', 'stop', 'reload', 'enable', 'disable', 'system_preset'])

export type HostContainer = { id: string; name: string; image: string; state: string; status: string; ports: string }
export type HostComposeProject = { name: string; status: string; config_files: string }
export type HostFileEntry = { name: string; path: string; type: 'directory' | 'file' | 'symlink' | 'other'; size: number; mode: string; modified_at: string }
export type HostFileList = { path: string; entries: HostFileEntry[]; truncated: boolean }
export type HostFilePreview = { path: string; content: string; truncated: boolean }

export function hostTerminalURL(serverId: string): string {
  const base = API_BASE.replace(/\/$/, '')
  const url = new URL(`${base}/api/servers/${encodeURIComponent(serverId)}/terminal`, window.location.href)
  if (url.origin !== window.location.origin || url.username || url.password || !['http:', 'https:'].includes(url.protocol)) {
    throw new Error('SSH 终端仅支持同源 API，请配置同源代理后连接。')
  }
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.toString()
}

let terminalAuthEpoch = 0
let terminalCookieQueue: Promise<unknown> = Promise.resolve()

function queueTerminalCookie<T>(operation: () => Promise<T>): Promise<T> {
  const result = terminalCookieQueue.then(operation, operation)
  terminalCookieQueue = result.catch(() => undefined)
  return result
}

const TERMINAL_COOKIE_CLEAR_TIMEOUT_MS = 10_000

async function clearTerminalCookie(): Promise<void> {
  const base = API_BASE.replace(/\/$/, '')
  // 必须带超时上限，避免断网或反代挂起时永久阻塞串行队列 terminalCookieQueue，导致重登录后无法建立终端会话。
  const response = await fetchWithTimeout(
    `${base}/api/auth/terminal-session`,
    { method: 'DELETE', credentials: 'include', keepalive: true },
    TERMINAL_COOKIE_CLEAR_TIMEOUT_MS,
  )
  if (!response.ok) throw new Error(`清理终端认证失败 (${response.status})`)
}

function establishTerminalCookie(): Promise<{ ready: boolean }> {
  // Validate origin before sending the Bearer-to-cookie bridge request.
  hostTerminalURL('origin-check')
  const epoch = terminalAuthEpoch
  return queueTerminalCookie(async () => {
    if (epoch !== terminalAuthEpoch || !loadSession()) throw new Error('登录状态已变更，请重新连接终端。')
    const result = await request<{ ready: boolean }>('/api/auth/terminal-session', { method: 'POST', credentials: 'include' })
    if (epoch !== terminalAuthEpoch || !loadSession()) {
      await clearTerminalCookie()
      throw new Error('登录状态已变更，已清理终端认证。')
    }
    return result
  })
}

const seg = encodeURIComponent

export const api = {
  hostContainers: (id: string, signal?: AbortSignal) => request<{ containers: HostContainer[] }>(`/api/servers/${seg(id)}/docker/containers`, { signal }),
  hostCompose: (id: string, signal?: AbortSignal) => request<{ projects: HostComposeProject[] }>(`/api/servers/${seg(id)}/docker/compose`, { signal }),
  hostFiles: (id: string, path: string, signal?: AbortSignal) => request<HostFileList>(`/api/servers/${seg(id)}/files?path=${seg(path)}`, { signal }),
  hostFilePreview: (id: string, path: string, signal?: AbortSignal) => request<HostFilePreview>(`/api/servers/${seg(id)}/files/preview?path=${seg(path)}`, { signal }),
  terminalCookie: establishTerminalCookie,
  health: () => request<Health>('/api/health'),
  // 登录页配置（公开）：Turnstile site key 为空 = 后端未启用人机验证。
  loginConfig: async (): Promise<{ turnstile_site_key: string }> => {
    const base = API_BASE.endsWith('/') ? API_BASE.slice(0, -1) : API_BASE
    const response = await fetchWithTimeout(`${base}/api/login/config`, { cache: 'no-store' }, PUBLIC_REQUEST_TIMEOUT_MS)
    if (!response.ok) throw new Error(`登录配置加载失败 (${response.status})`)
    return parseJsonResponse<{ turnstile_site_key: string }>(response)
  },
  login: async (email: string, password: string, turnstileToken = ''): Promise<Session> => {
    const base = API_BASE.endsWith('/') ? API_BASE.slice(0, -1) : API_BASE
    const response = await fetchWithTimeout(`${base}/api/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password, turnstile_token: turnstileToken }),
    }, PUBLIC_REQUEST_TIMEOUT_MS)
    if (!response.ok) {
      const body = await response.json().catch(() => ({}))
      const message = body?.error ?? `登录失败 (${response.status})`
      const error = new Error(message) as ApiError
      error.status = response.status
      throw error
    }
    const body = await parseJsonResponse<{ token: string; email: string; role: string }>(response)
    if (!body?.token || typeof body.token !== 'string') {
      throw new Error('登录响应缺少有效令牌，请检查后端服务状态')
    }
    const session: Session = { token: body.token, email: body.email, role: body.role }
    saveSession(session)
    return session
  },
  logout: () => {
    terminalAuthEpoch++
    clearSession()
    // Serialize behind in-flight bridges: a late POST must not restore the cookie.
    void queueTerminalCookie(clearTerminalCookie).catch((error) => console.error('终端认证清理失败', error))
  },
  servers: () => request<Server[]>('/api/servers'),
  suggestServerName: (host: string) => request<{ name: string; region?: string; provider?: string; ip?: string; error?: string }>(`/api/servers/suggest-name?host=${encodeURIComponent(host)}`),
  createServer: (payload: {
    name: string
    host: string
    ssh_port: number
    ssh_user: string
    region?: string
    provider?: string
    tags?: string[]
    traffic_api_url?: string
    traffic_secret_file?: string
  }) =>
    request<{ id: string }>('/api/servers', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        region: '',
        provider: '',
        tags: [],
        ...payload,
      }),
    }),
  nodes: () => request<Hy2Node[]>('/api/nodes'),
  // server_id may be omitted when api_url is given: the server registers a host
  // from the node's own public IP so a node can be added with just URL + key.
  // 返回体里的 in_store / warning 是服务端对「无 API ⇒ 强制自用」的如实告知：
  // 没有节点 API 的节点开不出买家账号，服务端会把它降为自用并回一条 warning。
  // 类型里必须带上，否则调用方会静默丢掉这句话（2026-10-31）。
  createNode: (payload: { server_id?: string; name: string; listen_port: number; version?: string; domain?: string; api_url?: string; api_key?: string; in_store?: boolean }) =>
    request<{ id: string; in_store?: boolean; warning?: string }>('/api/nodes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ version: '', domain: '', api_url: '', api_key: '', ...payload }),
    }),
  previewNodeAPI: (input: { url?: string; api_key?: string; basic_user?: string; basic_password?: string; access_json?: string }) =>
    request<PortalPreview>('/api/nodes/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    }),
  refreshNodeAPI: (id: string) =>
    request<{ status: string; users_count: number; listen_port: number; domain: string; cert_type: string; hop_range: string }>(`/api/nodes/${seg(id)}/api`),
  bindNodeAPI: (id: string, input: { url?: string; api_key?: string; basic_user?: string; basic_password?: string; access_json?: string }) =>
    request<{ status: string; users_count: number; listen_port: number; has_basic: boolean; caps: Record<string, unknown> }>(`/api/nodes/${seg(id)}/api`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    }),
  unbindNodeAPI: (id: string) =>
    request<{ status: string }>(`/api/nodes/${seg(id)}/api`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  // Node-side control, all riding the stored Bearer api_key.
  nodeOverview: (id: string) => request<NodeOverview>(`/api/nodes/${seg(id)}/overview`),
  // The web-session half. Requires the node to have Basic credentials bound;
  // when it does not, the server answers 400 with need_basic:true.
  portalUpdateStatus: (refresh = false) =>
    request<PortalUpdateStatus>(`/api/portal-update-status${refresh ? '?refresh=1' : ''}`),
  portalUpdateBatchHistory: () => request<Pick<PortalUpdateBatch, 'id' | 'status' | 'created_at' | 'updated_at'>[]>('/api/portal-update-batches'),
  cancelPortalUpdateBatch: (id: string) => request<{ status: string; message: string }>(`/api/portal-update-batches/${seg(id)}/cancel`, {
    method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' },
  }),
  startPortalUpdateBatch: (node_ids: string[]) => request<PortalUpdateBatch>('/api/portal-update-batches', {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Confirm-Dangerous': 'yes' },
    body: JSON.stringify({ node_ids }),
  }),
  portalUpdateBatch: (id: string) => request<PortalUpdateBatch>(`/api/portal-update-batches/${seg(id)}`),
  nodePortalStatus: (id: string) => request<PortalNodeStatus>(`/api/nodes/${seg(id)}/portal-status`),
  nodePortalAction: (id: string, payload: PortalActionRequest, dangerous = false) =>
    request<PortalActionResult>(`/api/nodes/${seg(id)}/portal-action`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(dangerous ? { 'X-Confirm-Dangerous': 'yes' } : {}),
      },
      body: JSON.stringify(payload),
    }),
  nodeUsers: (id: string) => request<NodeUserList>(`/api/nodes/${seg(id)}/users`),
  // IP 纯净度：读缓存（不联网）与显式检测分开。列表页只调 ipPurity（快），
  // 检测由用户点按钮触发 —— 否则刷一次页面就把第三方免费配额刷爆。
  ipPurity: (id: string) => request<IPPurityState>(`/api/nodes/${seg(id)}/ip-purity`),
  // 全量快照：专用页面一次拉所有节点（只读库，不联网）。
  ipPurityList: () => request<IPPurityList>('/api/ip-purity'),
  // forceApi=1 → 深度检测：即使离线库已判出连接类型也走 API 拿评分/双ISP。
  refreshIPPurity: (id: string, forceApi = false) =>
    request<{ status: string; purity: IPPurity }>(`/api/nodes/${seg(id)}/ip-purity/refresh${forceApi ? '?force_api=1' : ''}`, {
      method: 'POST',
    }),
  nodeOutbounds: (id: string) => request<NodeOutboundState>(`/api/nodes/${seg(id)}/outbounds`),
  nodeOutboundProbe: (id: string, name: string) =>
    request<{ name: string; kind: string; ip: string }>(`/api/nodes/${seg(id)}/outbounds/probe?name=${seg(name)}`),
  myLogins: () => request<{ items: LoginRecord[]; ban_policy: string }>('/api/auth/logins'),
  // telegram_configured = null 表示没读到绑定状态（数据库暂时不可用），与「未配置」不是一回事。
  schedules: () => request<{ items: ScheduleItem[]; telegram_configured: boolean | null; telegram_error?: string }>('/api/schedules'),
  setSchedule: (key: string, enabled: boolean) => request<{ items: ScheduleItem[]; telegram_configured: boolean | null; telegram_error?: string }>('/api/schedules', { method: 'PUT', body: JSON.stringify({ key, enabled }) }),
  runSchedule: (key: string) => request<{ status: string }>(`/api/schedules/${seg(key)}/run`, { method: 'POST' }),
  storeSignature: () => request<{ required: boolean }>('/api/settings/store-signature'),
  setStoreSignature: (required: boolean) => request<{ required: boolean }>('/api/settings/store-signature', { method: 'PUT', body: JSON.stringify({ required }) }),
  autoHide: () => request<AutoHideState>('/api/nodes/auto-hide'),
  setAutoHide: (enabled: boolean) => request<AutoHideState>('/api/nodes/auto-hide', { method: 'PUT', body: JSON.stringify({ enabled }) }),
  telegramSettings: () => request<TelegramSettings>('/api/settings/telegram'),
  setTelegramChat: (chatId: string) => request<{ token_configured: boolean; chat_id: string }>('/api/settings/telegram', { method: 'PUT', body: JSON.stringify({ chat_id: chatId }) }),
  testTelegram: () => request<{ status: string }>('/api/settings/telegram/test', { method: 'POST' }),
  acceptFingerprint: (serverId: string, expectedOld: string, next: string) =>
    request<{ fingerprint: string }>(`/api/servers/${seg(serverId)}/fingerprint/accept`, { method: 'POST', body: JSON.stringify({ expected_old: expectedOld, new: next }) }),
  profitReport: () => request<ProfitReport>('/api/reports/profit'),
  setSubscriptionRevenue: (id: string, monthly: number | null, currency: string) =>
    request<unknown>(`/api/subscriptions/${seg(id)}/revenue`, { method: 'PUT', body: JSON.stringify({ revenue_monthly: monthly, revenue_currency: currency }) }),
  sharing: () => request<{ items: SharingItem[] }>('/api/subscriptions/sharing'),
  blockChecks: () => request<BlockChecks>('/api/block-checks'),
  runBlockChecks: () => request<{ checked: number }>('/api/block-checks/run', { method: 'POST' }),
  setCNProbe: (serverId: string) => request<unknown>('/api/settings/cn-probe', { method: 'PUT', body: JSON.stringify({ server_id: serverId }) }),
  migrateNode: (nodeId: string, payload: { target_server_id: string; auto_dns: boolean; email?: string }) =>
    request<{ job_id: string }>(`/api/nodes/${seg(nodeId)}/migrate`, { method: 'POST', body: JSON.stringify(payload) }),
  testServerSSH: (id: string) => request<SSHTestResult>(`/api/servers/${seg(id)}/test-ssh`, { method: 'POST' }),
  serverBilling: () => request<ServerBilling[]>('/api/servers/billing'),
  setServerBilling: (id: string, payload: Omit<ServerBilling, 'server_id'>) =>
    request<ServerBilling>(`/api/servers/${seg(id)}/billing`, { method: 'PUT', body: JSON.stringify(payload) }),
  changeServerIP: (id: string, ip: string, updateDNS: boolean) =>
    request<{ old_ip: string; new_ip: string; dns_ok: boolean; dns: { node: string; domain: string; result: string; failed?: boolean; warning?: string }[] }>(
      `/api/servers/${seg(id)}/change-ip`, { method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' }, body: JSON.stringify({ ip, update_dns: updateDNS }) }),
  serverTrend: (id: string, days: number) => request<{ days: number; points: ServerTrendPoint[] }>(`/api/servers/${seg(id)}/metrics/trend?days=${days}`),
  installHy2: (serverId: string, payload: { domain: string; email?: string; port?: number; name?: string; in_store?: boolean; auto_dns?: boolean }) =>
    request<{ job_id: string }>(`/api/servers/${seg(serverId)}/install-hy2`, { method: 'POST', body: JSON.stringify(payload) }),
  // 检测任意 IP / 域名的纯净度；命中本面板节点时顺带让该节点现场检测流媒体 / AI 解锁。
  probeIP: (query: string) => request<IPProbeResult>('/api/ip-probe', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ query }) }),
  // Cloudflare Token 能管理的域名（一键安装只填前缀用）。没配 Token 时 configured=false。
  cloudflareZones: () => request<{ configured: boolean; zones: string[]; error?: string }>('/api/cloudflare/zones'),
  installJobs: () => request<{ jobs: InstallJob[] }>('/api/install-jobs'),
  resolveInstallJob: (id: string) => request<{ status: string }>(`/api/install-jobs/${seg(id)}/resolve`, { method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' }, body: JSON.stringify({ confirm_remote_stopped: true }) }),
  installJob: (id: string) => request<InstallJob>(`/api/install-jobs/${seg(id)}`),
  deliveryDomain: () => request<DeliveryDomainState>('/api/settings/delivery-domain'),
  setDeliveryDomain: (value: string) => request<DeliveryDomainState>('/api/settings/delivery-domain', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ value }) }),
  // 只改订阅显示名称（客户端导入后的分组名），不动交付域名。
  setSubscriptionName: (name: string) => request<DeliveryDomainState>('/api/settings/delivery-domain', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ value: '-', name }) }),
  purityKeys: () => request<PurityKeysState>('/api/settings/purity-keys'),
  // 字段缺省 = 不改；空串 = 清除面板里的值。
  setPurityKeys: (payload: Partial<Record<PurityKeyName, string>>) =>
    request<PurityKeysState>('/api/settings/purity-keys', { method: 'PUT', body: JSON.stringify(payload) }),
  unlockList: () => request<{ items: NodeUnlock[] }>('/api/unlock'),
  refreshUnlock: (id: string) =>
    request<{ status: string; results: UnlockResult[] }>(`/api/nodes/${seg(id)}/unlock/refresh`, { method: 'POST' }),
  refreshAllIPPurity: () => request<{ ok: number; skipped: number; failed: number; errors: string[] }>('/api/ip-purity/refresh-all', {
    method: 'POST',
  }),
  // password 可选：不传则由**节点随机生成**。
  // ⚠️ 调用方若要保证"重复创建不换密码"，必须显式传一个确定性密码
  // （见后端 provisionAccountPassword）—— 否则每次 create 都会换掉节点上的密码，
  // 而库里记的旧值随即失效，客户端报 authentication error。
  createNodeUser: (id: string, payload: { user_id?: string; password?: string; duration_days?: number; ip_limit?: number; traffic_gb?: number; note?: string }) =>
    request<NodeUserCreated>(`/api/nodes/${seg(id)}/users`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  renewNodeUser: (id: string, uid: string, payload: { extend_days?: number; add_traffic_gb?: number }) =>
    request<NodeUserRenewed>(`/api/nodes/${seg(id)}/users/${seg(uid)}/renew`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  deleteNodeUser: (id: string, uid: string) =>
    request<{ status: string; user_id: string }>(`/api/nodes/${seg(id)}/users/${seg(uid)}`, {
      method: 'DELETE',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  nodeProxyServices: (id: string) => request<NodeProxyList>(`/api/nodes/${seg(id)}/proxy-services`),
  createNodeProxyService: (id: string, payload: { protocol: ProxyProtocol; port?: number; note?: string }) =>
    request<NodeProxyService & { node_id: string }>(`/api/nodes/${seg(id)}/proxy-services`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  deleteNodeProxyService: (id: string, pid: string) =>
    request<{ status: string; id: string }>(`/api/nodes/${seg(id)}/proxy-services/${seg(pid)}`, {
      method: 'DELETE',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  // Merged subscription. The feed itself is public and token-addressed; these are
  // the management calls behind a normal session.
  subscriptionState: () => request<SubscriptionState>('/api/subscription'),
  rotateSubscription: () =>
    request<{ token: string; url: string; path: string }>('/api/subscription/rotate', {
      method: 'POST',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  updateSubscription: (payload: { enabled?: boolean; label?: string }) =>
    request<{ enabled: boolean; label: string; node_state_ok?: boolean; node_state_message?: string }>('/api/subscription', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  subscriptionPreview: (format: SubscriptionFormat) => requestText(`/api/subscription/preview?format=${format}`),
  // ---- Multi-subscription storefront ----
  subscriptionCenter: () => request<SubscriptionCenter>('/api/subscriptions'),
  createSubscription: (payload: {
    label?: string
    duration_days?: number
    expires_at?: number
    ip_limit?: number
    auto_delete?: boolean
    user_email?: string
    note?: string
    // 分类圈定：非空时这条订阅只包含该分类下的节点。
    // 空/不传 = 不圈定（全部分类），与加分类功能之前的行为一致。
    category_id?: string
    // 直接圈定节点（多选）。非空时优先于 category_id，只下发这些节点。
    node_ids?: string[]
  }) =>
    // ⚠️ provisioned_nodes / excluded 是 2026-10-04 加的（修「手工建订阅
    // 不会开户」那个 BUG 时一并返回的）。**必须显示到界面上** ——
    // 只报"创建成功"而客户其实连不上，是这个 BUG 能长期潜伏的原因。
    request<{
      id: string
      token: string
      label: string
      expires_at_text: string
      delivery_url: string
      subscription_url: string
      /** 成功开户的节点数。0 表示一个都没开成。 */
      provisioned_nodes?: number
      /** 这条订阅范围内的可开户节点总数（分母）。 */
      expected_nodes?: number
      /** 没开成的节点及原因。空数组 = 全部成功。 */
      excluded?: { node_id: string; node_name: string; reason: string }[]
      // ⚠️ 2026-10-30：开户结果的可信度。unverified = 后端读不到节点范围，
      // 此时 provisioned_nodes 与 excluded **都不能**当作结论 ——
      // 界面必须显示"未确认"，不能报"全部开好"，也不能报"一个都没成"。
      provision_status?: 'complete' | 'partial' | 'unverified'
      provision_note?: string
    }>(
      '/api/subscriptions',
      { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) },
    ),
  updateSubscriptionRecord: (
    id: string,
    payload: {
      label?: string
      enabled?: boolean
      duration_days?: number
      expires_at?: number
      ip_limit?: number
      auto_delete?: boolean
      user_email?: string
      note?: string
      reset_ips?: boolean
    },
  ) =>
    // 返回值除订阅本体外，还带**节点联动结果**（停用/启用时后端会去各节点
    // 把账号置为 disabled/active，客户端据此立刻断开）。
    // 节点可能不听话（老节点没 set_status、不可达、限流），
    // 所以必须把失败信息回给界面 —— 不能无条件显示「已停用」。
    request<ManagedSubscription & {
      node_state_message?: string
      node_state_ok?: boolean
      node_state_failed?: Record<string, string>
      // 保存成功但操作者必须知道的提示（例如「续期了但订阅仍是停用」「已登记后台补开户」），没有时为空串。
      notice?: string
    }>(`/api/subscriptions/${seg(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  deleteSubscriptionRecord: (id: string) =>
    request<{ ok: boolean }>(`/api/subscriptions/${seg(id)}`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  rotateSubscriptionRecord: (id: string) =>
    request<{ token: string; subscription_url: string; delivery_url: string }>(`/api/subscriptions/${seg(id)}/rotate`, {
      method: 'POST',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  rotateStoreKey: () =>
    request<{ key: string; auth_header: string }>('/api/subscriptions/store-key', {
      method: 'POST',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  // Public delivery view. No session header is attached on purpose — this is what
  // a customer's browser calls, and the token in the path is the credential.
  deliveryView: async (token: string): Promise<DeliveryView> => {
    const base = API_BASE.endsWith('/') ? API_BASE.slice(0, -1) : API_BASE
    const response = await fetchWithTimeout(
      `${base}/api/sub/view/${encodeURIComponent(token)}`,
      { cache: 'no-store' },
      PUBLIC_REQUEST_TIMEOUT_MS,
    )
    if (!response.ok) {
      let message = response.status === 404 ? '订阅不存在或已被删除' : `读取订阅失败 (${response.status})`
      try {
        const body = await response.json()
        if (body?.error && response.status !== 404) message = `${body.error} (${response.status})`
      } catch {
        // 非 JSON 响应体时保留状态码兜底文案
      }
      const error = new Error(message) as ApiError
      error.status = response.status
      throw error
    }
    return parseJsonResponse<DeliveryView>(response)
  },
  serverMetrics: (id: string) => request<MetricSample[]>(`/api/servers/${seg(id)}/metrics`),
  // One call returns the 24h sparkline series for every host at once. The
  // dashboard used to fan out one request per host on every 15s poll.
  recentMetrics: () => request<Record<string, MetricSample[]>>('/api/metrics/recent'),
  uploadCredential: (id: string, authType: 'ssh_key' | 'password', secret: string) =>
    request<CredentialMeta>(`/api/servers/${seg(id)}/credential`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ auth_type: authType, secret }),
    }),
  credentialMeta: (id: string) => request<CredentialMeta[]>(`/api/servers/${seg(id)}/credential`),
  setWriteOps: (id: string, allow: boolean, units: string[]) =>
    request<{ allow_write_ops: boolean; write_units: string[] }>(`/api/servers/${seg(id)}/write-ops`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Confirm-Dangerous': 'yes' },
      body: JSON.stringify({ allow, units }),
    }),
  taskLogs: (id: string) => request<TaskLog[]>(`/api/tasks/${seg(id)}/logs`),
  retryTask: (id: string, dangerous = false) =>
    request<{ id: string; status: string }>(`/api/tasks/${seg(id)}/retry`, {
      method: 'POST',
      headers: dangerous ? { 'X-Confirm-Dangerous': 'yes' } : undefined,
    }),
  // ⚠️ 返回类型原本被误写成 DeleteNodeResult，而它**两种形状都可能**：
  //   - 正常更新          → 节点本体（Hy2Node），没有 status 字段
  //   - 关商城时注销买家账号不干净 → 207 {status:'updated_with_revoke_failures', failed_count, error}
  // 调用方靠 `result.status === 'updated_with_revoke_failures'` 区分，
  // 所以类型必须是联合而不是单一形状（原来的标注让这条判断在类型上"看起来对"、
  // 实际是拿节点本体去比一个不存在的字段）。
  updateNode: (id: string, payload: { name?: string; server_id?: string; listen_port?: number; version?: string; domain?: string; in_store?: boolean }) =>
    request<Hy2Node | DeleteNodeResult>(`/api/nodes/${seg(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),

  // ---- 孤儿账号补删 ----
  // 删订阅/节点时若某买家账号没能在节点侧注销，会被登记到这里等补删。
  //
  // ⚠️ 返回的是**对象**（orphans/count/pending/note/how_it_happens），
  // 不是数组 —— 我第一版按数组写，解析出来是 undefined，
  // 界面会永远显示"没有孤儿账号"（最糟的那种错：看着正常其实全吞了）。
  orphans: (includeResolved = false) =>
    request<OrphanListResponse>(`/api/orphans${includeResolved ? '?include_resolved=1' : ''}`),
  // 用留存的凭据重试删除。⚠️ 不可逆 —— 需要确认头。
  retryOrphan: (id: string) =>
    // 后端回 detail（节点上本就没有这个账号）或 warning（删掉了但状态写回失败），从不回 message。
    request<{ status: string; user_id?: string; detail?: string; warning?: string }>(
      `/api/orphans/${seg(id)}/retry`,
      { method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' } },
    ),
  // 不补删、直接结案（机器已永久停用时用）。仅管理员。
  // 节点月度流量限额。limitGB 为 null 表示不限额。仅管理员。
  setNodeTrafficLimit: (id: string, limitGB: number | null, resetDay: number) =>
    request<{ status: string }>(`/api/nodes/${seg(id)}/traffic-limit`, {
      method: 'PUT', body: JSON.stringify({ limit_gb: limitGB, reset_day: resetDay }),
    }),
  // 2026-11-08：已删除节点的机器确认下线后，整台节点的待补删一次结案。
  resolveOrphansMachineOffline: (nodeId: string) =>
    request<{ status: string; node_name: string; resolved: number }>(`/api/orphans/nodes/${seg(nodeId)}/resolve-machine-offline`, { method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  resolveOrphan: (id: string) =>
    request<{ status: string; user_id: string }>(
      `/api/orphans/${seg(id)}/resolve`,
      { method: 'POST', headers: { 'X-Confirm-Dangerous': 'yes' } },
    ),

  // ---- 订阅账号对账 ----
  // 把确定性密码重新下发到各节点，修「库里记的密码与节点上实际生效的不一致」。
  // 幂等且无副作用（密码是确定性推导的，重复下发不会换密码），可随便跑。
  reconcileSubscription: (id: string) =>
    request<ReconcileResult>(`/api/subscriptions/${seg(id)}/reconcile`, { method: 'POST' }),
  reconcileAllSubscriptions: () =>
    request<ReconcileAllResult>('/api/subscriptions/reconcile-all', { method: 'POST' }),

  // ---- 节点能力 ----
  // 一次拿到所有节点的协议/扩展/门户版本总览。
  nodeCapabilities: (force = false) =>
    request<NodeCapabilitiesResult>(`/api/nodes/capabilities${force ? '?refresh=1' : ''}`),

  // ---- 节点分类 ----
  listCategories: () =>
    request<{ categories: NodeCategory[] }>('/api/categories'),
  createCategory: (payload: { slug: string; name: string; description?: string; sort_order?: number }) =>
    request<{ id: string; slug: string; name: string }>('/api/categories', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  updateCategory: (id: string, payload: { slug?: string; name?: string; description?: string; sort_order?: number; enabled?: boolean }) =>
    request<{ ok: boolean; categories: NodeCategory[] }>(`/api/categories/${seg(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  deleteCategory: (id: string) =>
    request<{ ok: boolean }>(`/api/categories/${seg(id)}`, {
      method: 'DELETE',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  // 覆盖式设置某节点的分类（不是增删两个接口）：
  // 界面上是勾选框，提交的就是"最终应该是什么样"。
  // 分成 add/remove 会让"勾选后网络失败重试"变成不确定的结果。
  setNodeCategories: (nodeID: string, categoryIDs: string[]) =>
    // 移出分类时会回收那些分类订阅在本节点上的账号；回收不完整返回 207 + ok:false。
    request<{ ok: boolean; category_ids: string[]; status?: string; error?: string; revoked_count?: number; failed_count?: number }>(`/api/nodes/${seg(nodeID)}/categories`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ category_ids: categoryIDs }),
    }),
  deleteCredential: (id: string) =>
    request<{ status: string }>(`/api/servers/${seg(id)}/credential`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  updateUser: (id: string, role: UserAccount['role']) =>
    request<{ status: string }>(`/api/users/${seg(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ role }),
    }),
  resetUserPassword: (id: string, password: string) =>
    request<{ status: string }>(`/api/users/${seg(id)}/password`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    }),
  deleteUser: (id: string) =>
    request<{ status: string }>(`/api/users/${seg(id)}`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  serverDetail: (id: string) => request<Server>(`/api/servers/${seg(id)}`),
  updateServer: (id: string, payload: { name?: string; region?: string; provider?: string; tags?: string[]; host?: string; ssh_port?: number; ssh_user?: string; reset_host_key?: boolean; traffic_api_url?: string; traffic_secret_file?: string }) =>
    request<{ status: string }>(`/api/servers/${seg(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  deleteServer: (id: string) =>
    request<{ status: string }>(`/api/servers/${seg(id)}`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  // decommissioned：机器已永久停用。删除时没能注销的账号仍会登记，但直接结案，不进待补删。
  deleteNode: (id: string, decommissioned = false) =>
    request<DeleteNodeResult>(`/api/nodes/${seg(id)}${decommissioned ? '?decommissioned=1' : ''}`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  cancelTask: (id: string) => request<{ status: string }>(`/api/tasks/${seg(id)}/cancel`, { method: 'POST' }),
  acknowledgeAlert: (id: string) => request<{ status: string }>(`/api/alerts/${seg(id)}/ack`, { method: 'POST' }),
  trafficHourly: () => request<TrafficBucket[]>('/api/traffic/hourly'),
  auditLogs: (limit = 50) => request<AuditEntry[]>(`/api/audit?limit=${limit}`),
  users: () => request<UserAccount[]>('/api/users'),
  createUser: (payload: { email: string; password: string; role: UserAccount['role'] }) =>
    request<{ id: string }>('/api/users', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  // 改密会作废所有旧令牌（含本会话）；后端随响应签发一枚新令牌，换上它当前页面才不会被踢回登录页。
  changePassword: async (currentPassword: string, newPassword: string) => {
    const res = await request<{ status: string; token?: string }>('/api/auth/change-password', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
    })
    const session = loadSession()
    if (res.token && session) saveSession({ ...session, token: res.token })
    // 同步推进终端认证代数并清理旧终端会话 Cookie，避免改密后复用已作废的终端凭证。
    terminalAuthEpoch++
    void queueTerminalCookie(clearTerminalCookie).catch(() => undefined)
    return res
  },
  runtime: () => request<RuntimeStatus>('/api/runtime'),
  // ---- 3X-UI 面板对接 ----
  // 凭据只在 create/preview 上行；读接口永不回显密码。
  xuiPanels: () => request<XuiPanel[]>('/api/xui/panels'),
  xuiPreviewPanel: (payload: { name: string; base_url: string; username: string; password: string }) =>
    request<XuiPreview>('/api/xui/panels/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  xuiCreatePanel: (payload: { name: string; base_url: string; username: string; password: string }) =>
    request<{ id: string }>('/api/xui/panels', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  xuiDeletePanel: (id: string) =>
    request<{ status: string }>(`/api/xui/panels/${seg(id)}`, { method: 'DELETE', headers: { 'X-Confirm-Dangerous': 'yes' } }),
  xuiRefreshPanel: (id: string) =>
    request<{ status: string; inbounds_count: number; clients_count: number; online_count: number | null; onlines_error?: string }>(`/api/xui/panels/${seg(id)}/refresh`, { method: 'POST' }),
  xuiInbounds: (id: string) => request<XuiInboundsResponse>(`/api/xui/panels/${seg(id)}/inbounds`),
  xuiAddClient: (id: string, payload: { inbound_id: number; email: string; limit_total_bytes: number; expire_days: number }) =>
    request<{ status: string }>(`/api/xui/panels/${seg(id)}/clients`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }),
  xuiDeleteClient: (id: string, inboundID: number, clientID: string) =>
    request<{ status: string }>(`/api/xui/panels/${seg(id)}/inbounds/${inboundID}/clients/${seg(clientID)}`, {
      method: 'DELETE',
      headers: { 'X-Confirm-Dangerous': 'yes' },
    }),
  xuiResetClientTraffic: (id: string, inboundID: number, email: string) =>
    request<{ status: string }>(`/api/xui/panels/${seg(id)}/inbounds/${inboundID}/clients/${seg(email)}/reset`, {
      method: 'POST',
    }),
  tasks: (ids?: string[]) => request<OperationTask[]>(ids?.length ? `/api/tasks?ids=${ids.map(seg).join(',')}` : '/api/tasks'),
  alerts: (includeResolved = false) => request<Alert[]>(`/api/alerts${includeResolved ? '?include=resolved' : ''}`),
  alertEvents: (id: string) => request<AlertEvent[]>(`/api/alerts/${seg(id)}/events`),
  // 告警外发配置。
  //
  // ⚠️ 服务端只回 host，不回完整 URL —— webhook 地址里带 access_token，
  // 而设置页多个管理员都能看。所以**不要**试图把 URL 回填进输入框：
  // 后端根本不返回它，回填只会得到一个空框，然后一次误保存就把配置清掉了。
  alertWebhook: () => request<{ configured: boolean; host: string }>('/api/alerts/webhook'),
  setAlertWebhook: (url: string) => request<{ configured: boolean; host: string }>(
    '/api/alerts/webhook',
    { method: 'PUT', body: JSON.stringify({ url }) },
  ),
  // Live systemd state for the host's whitelisted units. Unlike the write
  // operations this is answered synchronously and is safe to poll.
  serverServices: (id: string) => request<ServiceStateResponse>(`/api/servers/${seg(id)}/services`),
  createTask: (payload: { server_id: string; operation_type: ServiceOperation; node_id?: string; parameters?: Record<string, unknown> }) => {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (DANGEROUS_OPS.has(payload.operation_type)) {
      headers['X-Confirm-Dangerous'] = 'yes'
    }
    return request<{ id: string; status: string }>('/api/tasks', {
      method: 'POST',
      headers,
      body: JSON.stringify(payload),
    })
  },

  // ---- 节点监控 ----------------------------------------------------------
  //
  // 一次拿到全部节点的实时状态 + 网络延迟 + 最近一轮三网测速。
  // 为什么是一个聚合端点而不是让前端分别调三个：这个页面是"扫一眼看
  // 哪台有问题"，三处数据必须来自同一时刻 —— 分三次请求会出现
  // "状态是新的、测速是旧的"这种拼接不一致。
  nodeMonitoring: () => request<NodeMonitoringResponse>('/api/monitoring/nodes'),
  // 三网质量趋势：按运营商 × 时间桶聚合的历史延迟与丢包（china_quality.go）。
  nodeChinaQuality: (nodeId: string, range: ChinaQualityRange) =>
    request<ChinaQualityResponse>(`/api/monitoring/nodes/${encodeURIComponent(nodeId)}/quality?range=${range}`),
  speedtestTargets: () => request<SpeedtestTarget[]>('/api/speedtest/targets'),
  // 立即测速。服务端**后台执行**并立刻返回（一轮全量最坏几分钟，
  // 同步等待会被反代超时切断），所以这里返回的是 Accepted 语义。
  runSpeedtest: (nodeId?: string) => request<{ ok: boolean; started: number; message: string; skipped?: string[] }>(`/api/speedtest/run${nodeId ? `?node_id=${encodeURIComponent(nodeId)}` : ''}`, { method: 'POST' }),
}

// ---------------------------------------------------------------------------
// 节点监控的类型
// ---------------------------------------------------------------------------

export type MonitoringNode = {
  node_id: string
  node_name: string
  server_id: string
  server_name: string
  host: string
  region: string
  provider: string
  node_status: NodeStatus
  // 状态是怎么来的：api / ssh / ''（从未探测）。
  // 前端据此说明"这个节点没指标是因为它走的是 API 探测，不是采集器坏了"。
  // 没有这一栏，用户看到空白指标会误判（实测踩过）。
  status_source: '' | 'api' | 'ssh'
  // 节点级错误，例如 "unreachable" / "unauthorized" / "no_api"。
  // 与 server_status_error 不同：
  //   node_status_error    → 这一台节点有什么问题（换密钥 / 查机器）
  //   server_status_error  → 这台主机的采集有什么问题（SSH 凭据失效 / 内核拿不到）
  node_status_error: string
  listen_port: number
  // -1 表示节点没上报（不是"0 个用户"）。
  users_count: number

  server_status: ServerStatus
  // 主机级错误。最常见的是「SSH 凭据失效」—— 此时节点可能还在线（走 API
  // 探测的话仍显示 online），但**没有 CPU/内存/流量数据**。
  // 之前没暴露这一栏，前端就不知道为啥数据是空的，会以为采集器坏了。
  server_status_error: string
  cpu_percent: number
  memory_percent: number
  disk_percent: number
  load_average: string
  // 上面四个指标的采样时刻。后端只返回 15 分钟内的采样；
  // 缺失 = 这台主机已经停采，界面必须显示「—」而不是旧数字。
  metrics_sampled_at?: string

  // 面板 → 主机的纯 TCP 建连耗时。null = 还没测到（**与 0ms 不同**）。
  net_latency_ms: number | null
  net_latency_checked_at: string | null
  // SSH 握手耗时。与上面那个是不同的口径，界面上要分别标注。
  ssh_handshake_ms: number | null

  tx_bytes_24h: number
  rx_bytes_24h: number

  speedtest: SpeedtestSummary | null
}

export type SpeedtestSummary = {
  measured_at: string
  // false 表示这一轮探针整体没跑成功（比如节点没装 ping）。
  probe_ok: boolean
  probe_error: string
  targets: SpeedtestTargetView[]
  by_carrier: Record<string, CarrierAggregate>
}

export type SpeedtestTargetView = {
  key: string
  name: string
  host: string
  carrier: string
  carrier_label: string
  // 全部可空：null 表示这项**没测到**，与 0 不是一回事。
  icmp_avg_ms: number | null
  icmp_loss_pct: number | null
  tcp_connect_ms: number | null
  download_mbps: number | null
  upload_mbps: number | null
  probe_ok: boolean
  probe_error: string
}

export type CarrierAggregate = {
  carrier: string
  label: string
  avg_ms: number | null
  avg_loss_pct: number | null
  best_target: string
  best_ms: number | null
  // 分子/分母都给出：3 个靶点只测到 1 个时，"平均 50ms"的说服力完全不同。
  measured_targets: number
  total_targets: number
}

export type NodeMonitoringResponse = {
  nodes: MonitoringNode[]
  count: number
  speedtest_note: string
  // 非空表示这次没读到测速结果：卡片上的「未测得」不是真实状态。
  speedtest_error?: string
}

export type ChinaQualityRange = '24h' | '7d' | '30d'

export type ChinaQualitySeries = {
  carrier: 'telecom' | 'unicom' | 'mobile'
  label: string
  avg_ms: number | null
  p95_ms: number | null
  loss_pct: number | null
  points: { t: string; avg_ms: number | null; loss_pct: number | null }[]
}

export type ChinaQualityResponse = {
  range: ChinaQualityRange
  bucket_seconds: number
  series: ChinaQualitySeries[]
}

export type SpeedtestTarget = {
  id: string
  key: string
  name: string
  host: string
  carrier: string
  carrier_label: string
  enabled: boolean
  sort_order: number
}
