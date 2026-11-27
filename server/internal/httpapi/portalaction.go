package httpapi

// portalaction.go — 把门户网页端的写操作安全地转发出去。
//
// 这是本次「纯 API 管理节点」能力里威力最大的一块，因此约束也最严。三条铁律：
//
//  1. **动词白名单**。只有下表里的 action 能被转发。门户的 14 个 POST 端点里，
//     install-* 与 do-upgrade 会下载并执行远端脚本、reboot-server 会重启整机 ——
//     这些不能由前端自由拼装，必须在 Go 侧固化成固定组合。
//
//  2. **参数白名单**。每个 action 只允许携带下表声明的参数名，其余一律丢弃。
//     portal 侧用 parse_qs 解析，多传一个字段就可能命中它没预期的分支；与其
//     依赖 portal 的输入校验，不如在边界上就不放行。
//
//  3. **危险动词二次确认**。reboot 与 upgrade 需要 X-Confirm-Dangerous: yes，
//     与 unbindNodeAPI 的约定一致；面板前端已经会为这类操作弹确认框。
//
// 另外所有转发都写审计，但**审计内容里剔除凭据类参数**（password 等）——
// 审计表是可被多人查看的，把新建代理的密码写进去等于把凭据泄漏给了所有管理员。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 2026-11-06：这些值最终进节点上以 root 运行的 portal（会拼进客户端配置与命令行）。
// 门户在 2026-10-28 才自己补了校验，老节点（尚未升级门户）会原样接收，所以面板这侧必须自己卡住。
var (
	// awgEndpointPattern：域名或 IPv4，可选端口。
	awgEndpointPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})(:[0-9]{1,5})?$`)
	// portalPortPattern：端口只接受 1-65535。
	portalPortPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	// proxyPrefixedIDPattern：新版门户把 gost 代理服务 id 写成 proxy_<12 位 hex>。
	proxyPrefixedIDPattern = regexp.MustCompile(`^proxy_[0-9a-f]{12}$`)
	// controlChars 用于 note / username 这类自由文本。
	controlChars = regexp.MustCompile("[\\x00-\\x1f\\x7f]")
)

// awgClientNamePattern 与 portal 侧 VALID_AWG_NAME_RE 对齐：
// 只允许字母、数字、点、下划线、连字符，长度 1-32。客户端名会被
// hy2-awgctl 拼进命令行与文件名，这里先卡一道。
var awgClientNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// warpDomainPattern 校验 WARP 分流规则里的域名。
//
// 这里刻意用白名单而不是黑名单。早先的实现是「不含这些特殊字符」的黑名单，
// 结果换行符（\n \r）与所有控制字符都能溜过去 —— 实测 warp_add_rule 传 "a\nb"
// 会返回 200。而这些值最终会被 portal 拼进配置文件，属于典型的注入面。
// 白名单从根上杜绝：只认真正的域名字符，其余一律拒绝。
//
// 允许前导点（.example.com）以匹配「整个子域」的写法。
var warpDomainPattern = regexp.MustCompile(`^\.?[A-Za-z0-9]([A-Za-z0-9._-]{0,251}[A-Za-z0-9])?$`)

// portalActionTimeout 是普通写操作（开关协议、重启服务等）的预算。
const portalActionTimeout = 120 * time.Second

// 重型 root 操作的预算必须盖住门户侧的死线（2026-11-06）。门户给 do-upgrade / install-gost 600 秒、
// install-xray 300 秒、install-warp 240 秒、manage-amneziawg 的 update / set_line 480 秒、peer_add 120 秒。
// 面板原来一律 120 秒：超时回 502 判失败，而节点上的操作照常跑完，写锁随之释放 —— 下一个写操作打到
// 正在重启的门户，审计里也一条记录都没有（审计只在成功后写）。
const (
	portalHeavyInstallTimeout = 620 * time.Second
	portalAwgWriteTimeout     = 500 * time.Second
)

// portalActionBudget 按门户路径给出这次调用的预算。
func portalActionBudget(path string) time.Duration {
	switch {
	case path == "do-upgrade" || strings.HasPrefix(path, "install-"):
		return portalHeavyInstallTimeout
	case path == "manage-amneziawg":
		return portalAwgWriteTimeout
	}
	return portalActionTimeout
}

// portalActionSpec 描述一个可转发的动词。
type portalActionSpec struct {
	// Path 是 /{token}/ 之后的端点名。
	Path string
	// Action 是写进表单的 action 值。portal 各端点对「缺省 action」的处理不一致
	// （manage-reality / manage-warp 默认 toggle，manage-proxy 默认空串），
	// 所以显式带上，不依赖它的默认值。
	Action string
	// Allowed 是该动词允许携带的参数名（不含 action 本身）。
	Allowed []string
	// Label 用于审计与 UI 展示。
	Label string
	// Dangerous 的动词需要 X-Confirm-Dangerous: yes。
	Dangerous bool
	// Fixed 是不经前端、由服务端补死的参数。用于 install-* 这类
	// 「点一下就该按默认形态执行」的操作，防前端篡改。
	Fixed map[string]string
	// Requires 是该动作依赖的组件（warp / reality / awg / gost）。
	// 非空时，若节点未安装该组件，请求会被拦在前面，不下发到节点。
	//
	// 起因是一轮实测：节点没装 WARP，面板照样把 manage-warp 发过去，
	// 节点侧 portal 断连（unexpected EOF），随后连 SSH 都短暂不可达。
	// 这类操作既无意义又扰动生产节点，面板该替操作者挡住。
	//
	// install_* 这类动词刻意不设 Requires —— 它们存在的意义就是
	// 把组件装上，装的时候组件当然还不在。
	Requires string
}

// portalActions 是全部可转而未开放的动词。刻意不做成「任意路径 + 任意 action」
// 的通用代理：那样等于把 portal 的管理员会话整个暴露给浏览器。
var portalActions = map[string]portalActionSpec{
	// ---- 版本与自升级 ----
	"upgrade_core": {
		Path: "do-upgrade", Action: "",
		Fixed: map[string]string{"target": "core"},
		Label: "升级 Hysteria 内核", Dangerous: true,
		// Upgrade target is fixed by the server-side verb.
	},
	"enable_reality_online": {
		Path: "do-upgrade", Action: "",
		Fixed: map[string]string{"target": "reality-online"},
		Label: "开启 Reality 在线 IP 统计", Dangerous: true,
	},
	"upgrade_portal": {
		Path: "do-upgrade", Action: "",
		Fixed: map[string]string{"target": "portal"},
		Label: "升级 portal 自身", Dangerous: true,
	},
	"upgrade_awg": {
		Path: "do-upgrade", Action: "",
		Fixed: map[string]string{"target": "awg"},
		Label: "升级 AmneziaWG", Dangerous: true, Requires: "awg",
	},

	// ---- BBR ----
	"set_bbr": {
		Path: "set-bbr", Action: "",
		Allowed: []string{"version"},
		Label:   "设置 BBR 拥塞控制",
	},

	// ---- Reality / Xray ----
	"reality_toggle": {
		Path: "manage-reality", Action: "toggle",
		Label: "开关 Reality", Requires: "reality",
	},
	"reality_reset": {
		Path: "manage-reality", Action: "reset",
		Label: "重置 Reality 密钥", Dangerous: true, Requires: "reality",
	},
	"install_xray": {
		Path: "install-xray", Action: "",
		Label: "安装 Xray（Reality 依赖）",
	},

	"vmess_enable":  {Path: "manage-vmess", Action: "enable", Label: "启用 VMess"},
	"vmess_disable": {Path: "manage-vmess", Action: "disable", Label: "停用 VMess", Dangerous: true},

	// VLESS Encryption（2026-10-24）：管理动作固定 enable / disable，
	// 与 VMess 一致 —— 不接受 toggle，停用需危险确认。
	"vlessenc_enable":  {Path: "manage-vlessenc", Action: "enable", Label: "启用 VLESS Encryption"},
	"vlessenc_disable": {Path: "manage-vlessenc", Action: "disable", Label: "停用 VLESS Encryption", Dangerous: true},

	// AnyTLS（2026-10-30，节点侧由独立的 sing-box 进程承载，不碰 xray.json）：
	// 与 VMess / VLESS Encryption 同一语义 —— 只接受 enable / disable，停用需危险确认。
	// 老门户没有 manage-anytls ⇒ 节点返回 404，由调用方如实报「门户版本不支持」。
	"anytls_enable":  {Path: "manage-anytls", Action: "enable", Label: "启用 AnyTLS"},
	"anytls_disable": {Path: "manage-anytls", Action: "disable", Label: "停用 AnyTLS", Dangerous: true},

	// ---- WARP ----
	"install_warp": {
		Path: "install-warp", Action: "",
		Label: "安装 Cloudflare WARP",
	},
	"warp_toggle": {
		Path: "manage-warp", Action: "toggle",
		Label: "开关 WARP 分流", Requires: "warp",
	},
	"warp_add_rule": {
		Path: "manage-warp", Action: "add_rule",
		Allowed: []string{"domain"},
		Label:   "添加 WARP 路由域名", Requires: "warp",
	},
	"warp_del_rule": {
		Path: "manage-warp", Action: "del_rule",
		Allowed: []string{"domain"},
		Label:   "删除 WARP 路由域名", Requires: "warp",
	},
	"warp_reset_rules": {
		Path: "manage-warp", Action: "reset_rules",
		Label: "恢复 WARP 默认路由表", Requires: "warp",
	},

	// ---- gost 代理服务 ----
	"install_gost": {
		Path: "install-gost", Action: "",
		Label: "安装 gost",
	},
	"manage_proxy": {
		Path: "manage-proxy", Action: "",
		Allowed: []string{"action", "type", "port", "username", "password", "note", "id"},
		Label:   "管理 gost 代理服务", Requires: "gost",
	},

	// ---- 出口设置（自定义出站 / 全局出口 / 域名分流）----
	"manage_outbounds": {
		Path: "manage-outbounds", Action: "",
		Allowed: []string{"action", "name", "type", "addr", "url", "username", "password", "insecure", "mode", "domain", "outbound"},
		Label:   "出口设置",
	},

	// ---- AmneziaWG ----
	"install_awg": {
		Path: "install-amneziawg", Action: "",
		Allowed: []string{"line", "endpoint", "port"},
		Label:   "安装 AmneziaWG",
	},
	"manage_awg": {
		Path: "manage-amneziawg", Action: "",
		Allowed: []string{"action", "name", "endpoint", "line"},
		Label:   "管理 AmneziaWG", Requires: "awg",
	},

	// ---- 整机 ----
	"reboot": {
		Path: "reboot-server", Action: "",
		Label: "重启服务器", Dangerous: true,
	},
}

// capabilityProbes 说明「某组件装了没」该问哪个端点、读哪个字段。
//
// 字段名不统一是 portal 的现状：warp / reality / awg 都用 installed，
// gost 偏偏用 gost_installed。必须逐项写明，不能靠猜 —— 猜错会让门禁
// 永远判为「未安装」，把所有写操作都挡掉。
var capabilityProbes = map[string]struct {
	Path  string
	Field string
}{
	"warp":    {Path: "warp-status", Field: "installed"},
	"reality": {Path: "reality-status", Field: "installed"},
	"awg":     {Path: "awg-state", Field: "installed"},
	"gost":    {Path: "proxy-services", Field: "gost_installed"},
}

// capabilityMeta 是给操作者看的名词与「该装什么」的指引。
var capabilityMeta = map[string]struct {
	Label   string
	Install string
}{
	"warp":    {Label: "Cloudflare WARP", Install: "install_warp"},
	"reality": {Label: "Reality（Xray）", Install: "install_xray"},
	"awg":     {Label: "AmneziaWG", Install: "install_awg"},
	"gost":    {Label: "gost", Install: "install_gost"},
}

// capabilityMissingError 表示动作依赖的组件在节点上还没装。
type capabilityMissingError struct {
	Component string
	Label     string
	Install   string
}

func (e *capabilityMissingError) Error() string {
	return "该节点未安装 " + e.Label + "，此操作无法执行；请先安装后再试"
}

// ensureCapability 在真正下发写操作之前，确认节点已具备该组件。
//
// 探测失败时刻意「放行」而不是拦截：写操作自己会把真实错误带回来，
// 而若因为探测抖动就拦下写入，会变成「明明能装却被说不满足条件」，
// 比不打门禁更难用。门禁的价值在于挡住明知无意义的操作，不在于
// 变成一个新的故障点。
func (s *Server) ensureCapability(ctx context.Context, target *portalWebTarget, requires string) error {
	if requires == "" {
		return nil
	}
	probe, ok := capabilityProbes[requires]
	if !ok {
		return nil
	}
	envelope, err := portalWebCall(ctx, target, http.MethodGet, probe.Path, nil)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(envelope.Data, &payload); err != nil {
		return nil
	}
	installed, _ := payload[probe.Field].(bool)
	if installed {
		return nil
	}
	meta := capabilityMeta[requires]
	return &capabilityMissingError{
		Component: requires,
		Label:     meta.Label,
		Install:   meta.Install,
	}
}

// portalActionRequest 是 POST /api/nodes/{id}/portal-action 的载荷。
type portalActionRequest struct {
	// Verb 是上面那张家表的键。
	Verb string `json:"verb"`
	// Params 是该动词允许携带的参数。未在 Allowed 里的键会被静默丢弃
	// （而不是报错）—— 前端可能为了兼容多传字段，报错只会让 UI 更难写。
	Params map[string]string `json:"params"`
}

// buildForm 把一次请求收敛成 portal 认识的表单。
//
// 顺序很重要：先铺前端参数，再用 Fixed 覆盖，最后写 action。若反过来
// （Fixed 先、前端后），前端就能覆盖服务端想钉死的字段 —— 那正是固定值
// 存在的意义所在。这个顺序是被单测 TestBuildFormAppliesFixedValues 钉住的。
func buildForm(spec portalActionSpec, params map[string]string) map[string]string {
	form := map[string]string{}
	for _, key := range spec.Allowed {
		value, ok := params[key]
		if !ok {
			continue
		}
		// 长度上限：portal 侧多数字段自己有切片截断，但 id/name 这类
		// 会直接进 URL 或命令行的必须先卡住。按字符数卡 —— 按字节会把
		// 中文切成半个字，那个值随后要进表单/URL，畸形的 UTF-8 会在对面炸掉。
		value = truncateRunes(strings.TrimSpace(value), 512)
		form[key] = value
	}
	// Fixed 覆盖前端值：这些字段不接受调用方指手画脚。
	for k, v := range spec.Fixed {
		form[k] = v
	}
	// action 最后由 spec 决定，同样不可被 params 覆盖。
	if spec.Action != "" {
		form["action"] = spec.Action
	}
	return form
}

// 哪些参数不能进审计表。写 proxy 服务的 password、manage-user 的 password
// 都是真凭据，落进审计等于泄漏。
var portalAuditOmitted = map[string]bool{
	"password": true,
}

func sanitizeParamsForAudit(form map[string]string) map[string]string {
	if len(form) == 0 {
		return nil
	}
	out := make(map[string]string, len(form))
	for k, v := range form {
		lower := strings.ToLower(k)
		if portalAuditOmitted[lower] && v != "" {
			out[k] = "(已省略)"
			continue
		}
		// HTTP 代理出站地址可能内嵌 user:pass，写入审计前必须先抹掉密码段。
		if lower == "url" && v != "" {
			out[k] = redactProxyURL(v)
			continue
		}
		out[k] = v
	}
	return out
}

// POST /api/nodes/{id}/portal-action
// portalActionAdminOnly 判断一个节点动词是否只允许管理员执行。
func portalActionAdminOnly(verb string, spec portalActionSpec, params map[string]string) bool {
	verb = strings.TrimSpace(verb)
	if spec.Dangerous || strings.HasPrefix(verb, "install_") || verb == "manage_outbounds" {
		return true
	}
	// 2026-11-06：manage_awg / manage_proxy 也是以 root 改节点对外服务，与 manage_outbounds 同级。
	//   - manage_awg 的 update 会重下 hy2-awgctl 并以 root 替换二进制（等同于要危险确认的 upgrade_awg），
	//     set_line 会让所有 AWG 客户端必须重新导入配置，stop 直接断服务；只读的 state 与增删 peer 仍对 operator 开放。
	//   - manage_proxy 的 create 能在节点上开任意端口的 SOCKS5/HTTP 出口（按流量改道）。
	if verb == "manage_awg" {
		switch strings.TrimSpace(params["action"]) {
		case "", "state", "peer_add", "peer_del":
			return false
		}
		return true
	}
	if verb == "manage_proxy" {
		return true
	}
	return false
}

func (s *Server) portalNodeAction(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	nodeID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}

	var input portalActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 2026-11-06：动词只在这里 trim 一次，后面统一用 input.Verb。原来查表与管理员判断都 trim 过，
	// 但订阅缓存作废用的是原值：带前后空格的 " vmess_disable" 照常执行，缓存却不作废，刚停用的协议还会再下发最多 10 分钟。
	input.Verb = strings.TrimSpace(input.Verb)
	spec, ok := portalActions[input.Verb]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":     fmt.Sprintf("不支持的节点操作 %q", input.Verb),
			"supported": portalActionVerbs(),
		})
		return
	}

	// 2026-11-03：以 root 跑远端脚本的动词（危险动词、install_*、出口管理）仅限管理员。
	// 路由只套了 authenticated ⇒ operator 能重启节点、升级内核、装 WARP / AWG；
	// 而同样以 root 跑脚本的 install-hy2、迁移、换 IP 都早已要求管理员。
	// 开关类（启用协议、BBR、WARP 开关与规则）仍对 operator 开放。
	if portalActionAdminOnly(input.Verb, spec, input.Params) && claims.Role != "admin" {
		s.auditDenied(r, "forbidden_admin_only")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("「%s」仅限管理员操作", spec.Label)})
		return
	}

	// 危险动词的二次确认放在解析凭据之前：确认缺失是客户端错误，
	// 不该先花一次网络往返。
	if spec.Dangerous && r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"error":     fmt.Sprintf("「%s」是不可逆操作，需要 X-Confirm-Dangerous: yes", spec.Label),
			"dangerous": true,
		})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}

	// 纯本地校验放在最前：它不需要任何网络往返，而且语义上
	// 「你的参数传错了」应当优先于「节点没装东西」。若把门禁摆在前面，
	// 未装 WARP 的节点上连「域名格式非法」都会返回 409，操作者会去
	// 装 WARP 却发现还是报错 —— 那是在给错误归因制造障碍。
	form := buildForm(spec, input.Params)
	if err := validateActionForm(spec, form); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// 同节点的写操作串行执行。节点侧 portal 是单线程的，并发写会把它
	// 打崩（实测拿到 502 / unexpected EOF）。见 Server.nodeWriteLocks 注释。
	// 等锁最多 15 秒且随客户端断开放弃：同节点正在批量更新时直接告诉操作者，不让请求挂着。
	lockCtx, lockCancel := context.WithTimeout(r.Context(), 15*time.Second)
	unlock, lockErr := s.lockNodeWriteCtx(lockCtx, nodeID)
	lockCancel()
	if lockErr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点正在执行其他写操作（例如门户更新），请稍后重试"})
		return
	}
	defer unlock()

	budget := portalActionBudget(spec.Path)
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	target, err := s.loadPortalWebTarget(ctx, nodeID)
	if err != nil {
		s.writePortalWebTargetError(w, err)
		return
	}

	// 能力门禁：节点没装某个组件时，别把对应的写操作发过去。
	// 实测在未装 WARP 的节点上反复下发 manage-warp，会让节点 portal
	// 断连、并短暂扰动网络（连 SSH 都一度不可达）。那是无意义的操作。
	if err := s.ensureCapability(ctx, target, spec.Requires); err != nil {
		var missing *capabilityMissingError
		if errors.As(err, &missing) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":              missing.Error(),
				"missing_capability": missing.Component,
				"install_verb":       missing.Install,
			})
			return
		}
	}

	envelope, err := portalWebCall(ctx, target, http.MethodPost, spec.Path, form)
	// 改变协议开关的动作：订阅构建缓存立刻作废（2026-10-30）。缓存过期后最多还会
	// 「先回旧结果、后台刷新」10 分钟 —— 不清的话，刚关掉的 Reality 等协议仍可能
	// 被拉进买家订阅。失败也清：节点可能已部分生效，宁可下次多查一次节点。
	if subscriptionAffectingVerbs[input.Verb] {
		InvalidateSubscriptionCache()
		invalidateNodeCapsCache(nodeID)
	}
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		// 超时 ≠ 失败：节点上的 root 脚本很可能仍在跑（门户自己的死线比这里长时更是如此）。
		// 如实说「结果未知，请核验」，并留下审计 —— 使用脱离请求取消的独立短超时上下文，防止客户端断开或超时导致审计丢单。
		auditCtx, cancelAudit := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		s.audit(auditCtx, r, "portal_"+input.Verb+"_unknown", "hysteria_node", nodeID, map[string]any{
			"endpoint": spec.Path, "reason": "面板等待超时，节点可能仍在执行", "budget_seconds": int(budget.Seconds()),
		})
		cancelAudit()
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{
			"error": fmt.Sprintf("等待「%s」返回超时（%d 秒），节点侧可能仍在执行，结果未知；请稍后在节点状态里核验，不要立即重试", spec.Label, int(budget.Seconds())),
		})
		return
	}
	if err != nil {
		// portal 的业务错误（400 Invalid action 之类）在这里透传出去，
		// 先经过凭据脱敏避免路径令牌或口令回显给前端与日志。
		safeErr := redactPortalError(err.Error(), target)
		s.logger.Warn("portal action failed", "node", nodeID, "verb", input.Verb, "error", safeErr)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": safeErr})
		return
	}

	s.audit(r.Context(), r, "portal_"+input.Verb, "hysteria_node", nodeID, map[string]any{
		"endpoint": spec.Path,
		"params":   sanitizeParamsForAudit(form),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"verb":   input.Verb,
		"label":  spec.Label,
		"result": envelope.Data,
	})
}

// subscriptionAffectingVerbs 会改变节点对外提供哪些协议的动作 —— 订阅里每个节点下发
// 哪些协议由 /capabilities 的 installed+active 决定，这些动作执行后必须重新读。
var subscriptionAffectingVerbs = map[string]bool{
	"reality_toggle": true, "reality_reset": true, "install_xray": true,
	"vmess_enable": true, "vmess_disable": true,
	"vlessenc_enable": true, "vlessenc_disable": true,
	"anytls_enable": true, "anytls_disable": true,
}

// validateActionForm 对少数语义固定的字段做服务端校验。
//
// 这几项之所以在 Go 侧再校验一遍，是因为它们会被 portal 直接拼进命令行或
// 文件路径：gost 的 type、WARP 的域名、AWG 的客户端名。portal 自己也有校验，
// 但纵深防御在这里几乎不要成本。
// validateAwgEndpointAndLine 卡住 AmneziaWG 的 endpoint / line / port。
// endpoint 会写进客户端配置文件的多行字段，换行等于往配置里注入一行。
func validateAwgEndpointAndLine(form map[string]string) error {
	if endpoint := form["endpoint"]; endpoint != "" {
		if controlChars.MatchString(endpoint) || !awgEndpointPattern.MatchString(endpoint) {
			return errors.New("endpoint 只能是域名或 IPv4，可带端口（例如 vpn.example.com:51820），不能含换行")
		}
		if i := strings.LastIndex(endpoint, ":"); i >= 0 {
			port := endpoint[i+1:]
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return errors.New("endpoint 里的端口需为 1-65535")
			}
		}
	}
	if line := form["line"]; line != "" && line != "2" && line != "3" {
		return errors.New("线路只支持 2 或 3")
	}
	if port := form["port"]; port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("端口需为 1-65535 的数字")
		}
	}
	return nil
}

func validateActionForm(spec portalActionSpec, form map[string]string) error {
	switch spec.Path {
	case "manage-vmess":
		if form["action"] != "enable" && form["action"] != "disable" {
			return errors.New("VMess 动作只支持 enable / disable")
		}
	case "manage-vlessenc":
		if form["action"] != "enable" && form["action"] != "disable" {
			return errors.New("VLESS Encryption 动作只支持 enable / disable")
		}
	case "manage-proxy":
		if action := form["action"]; action != "" && action != "create" && action != "delete" {
			return errors.New("代理服务只支持 create / delete")
		}
		if port := form["port"]; port != "" {
			if !portalPortPattern.MatchString(port) {
				return errors.New("端口需为 1-65535 的数字")
			}
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return errors.New("端口需为 1-65535 的数字")
			}
		}
		if id := form["id"]; id != "" && !proxyIDPattern.MatchString(id) && !proxyPrefixedIDPattern.MatchString(id) {
			return errors.New("代理服务 id 格式不合法")
		}
		for _, key := range []string{"username", "password", "note"} {
			if v := form[key]; controlChars.MatchString(v) {
				return errors.New("用户名、密码与备注里不能有控制字符或换行")
			}
		}
		if ptype := form["type"]; ptype != "" && ptype != "socks5" && ptype != "http" && ptype != "https" {
			return errors.New("代理类型只支持 socks5 / http / https")
		}
	case "manage-warp":
		if domain := form["domain"]; domain != "" && !warpDomainPattern.MatchString(domain) {
			return errors.New("域名格式不合法：只允许字母、数字、点、连字符与下划线，且以字母或数字开头结尾")
		}
	case "manage-amneziawg", "install-amneziawg":
		if name := form["name"]; name != "" && !awgClientNamePattern.MatchString(name) {
			return errors.New("AWG 客户端名只允许字母、数字、点、下划线、连字符，长度 1-32")
		}
		if err := validateAwgEndpointAndLine(form); err != nil {
			return err
		}
		switch form["action"] {
		case "state", "peer_add", "peer_del", "set_line", "update", "start", "stop", "restart", "":
		default:
			return errors.New("AWG 动作不合法")
		}
	case "manage-outbounds":
		if err := validateOutboundForm(form); err != nil {
			return err
		}
	case "set-bbr":
		switch form["version"] {
		case "", "v1", "v2", "v3":
		default:
			return errors.New("BBR 版本只支持 v1 / v2 / v3")
		}
	case "do-upgrade":
		switch form["target"] {
		case "core", "portal", "awg", "reality-online":
		default:
			return errors.New("目标只支持 core / portal / awg / reality-online")
		}
	}
	return nil
}

func portalActionVerbs() []string {
	out := make([]string, 0, len(portalActions))
	for verb, spec := range portalActions {
		out = append(out, verb+" ("+spec.Label+")")
	}
	return out
}
