package httpapi

// vlessenc.go — VLESS Encryption 通道（2026-10-24）。
//
// 上游 portal 55c677fa52be 新增：
//   - capabilities.protocols.vlessenc：installed / active / configured / domain / sni / port
//   - GET  vlessenc-status（Basic）：config.uri 是**管理员**的 vless:// 链接
//   - POST manage-vlessenc：固定 action = enable / disable
//
// 关键事实（核对上游源码，不猜测）：
//
//  1. capabilities 里**没有客户端 encryption 参数**。它只出现在
//     vlessenc-status.config.uri 的 query 里，而那条 URI 的 userinfo 是
//     管理员 UUID（reality_uuid_for_user(MASTER_USER_ID)）。
//
//  2. encryption 是**公开的客户端密钥**，可以安全解析并下发给买家；
//     decryption 是服务端私钥，上游从不通过任何通道输出，我们也绝不读取。
//
//  3. 上游 vlessenc_clients() 与 Reality 共用 UUID 命名空间
//     （reality_uuid_for_user），且不带 flow —— 所以这里必须用
//     realityUUIDForUser(买家账号)，不能另造一个 vmess 风格的命名空间。
//
//  4. 没有公开 encryption / 没有 Basic 凭据 / 端点缺失时，**不可交付**：
//     不生成任何 VLESS-Enc 客户端配置。宁可少一条通道，也不发一条
//     注定握手失败的配置（那会让买家以为节点坏了）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// vlessEncryptionPattern 校验客户端 encryption 的**公开**格式。
//
// 官方 Xray 生成形态（main/commands/all/vlessenc.go）：
//
//	mlkem768x25519plus.native.0rtt.<base64url X25519 client key>
//
// 官方文档允许在第三段后追加 padding 块，所以这里接受任意数量的
// base64url 段（含 100-111-1111 这类 padding 参数）。第一段必须精确，
// 第二段是 native/xorpub/random，第三段是 0rtt/1rtt。
//
// 不接受 "none"：那表示未启用 VLESS Encryption，不是本通道。
var vlessEncryptionPattern = regexp.MustCompile(
	`^mlkem768x25519plus\.(native|xorpub|random)\.(0rtt|1rtt)(\.[A-Za-z0-9_-]+)+$`)

// nodeVLESSEncryption 是一台节点上 VLESS Encryption 的**客户端公开参数**。
// 服务端 decryption 永远不会进入这个结构。
type nodeVLESSEncryption struct {
	Domain     string
	SNI        string
	Encryption string
	Port       int
}

func (v *nodeVLESSEncryption) ready() bool {
	return v != nil &&
		strings.TrimSpace(v.Domain) != "" &&
		validVLESSEncryption(v.Encryption) &&
		v.Port >= 1 && v.Port <= 65535
}

func validVLESSEncryption(raw string) bool {
	return vlessEncryptionPattern.MatchString(strings.TrimSpace(raw))
}

// vlessEncCapability 是 capabilities.protocols.vlessenc 的已校验视图。
type vlessEncCapability struct {
	Installed  bool
	Active     bool
	Configured bool
	Domain     string
	SNI        string
	Port       int
}

// vlessEncFromCapability 只接受「已安装、已启用、配置完整」的能力段。
//
// 老节点没有 vlessenc 段 ⇒ 返回 nil，调用方据此完全保持原有行为；
// 这不是错误，只是该节点没有这个通道。
func vlessEncFromCapability(m map[string]any) *vlessEncCapability {
	if !capabilityBool(m, "installed") || !capabilityBool(m, "active") {
		return nil
	}
	if configured, present := m["configured"]; present && configured != true {
		return nil
	}
	c := &vlessEncCapability{
		Installed:  true,
		Active:     true,
		Configured: true,
		Domain:     capabilityString(m, "domain"),
		SNI:        capabilityString(m, "sni"),
		Port:       capabilityPort(m),
	}
	if c.Domain == "" || c.Port < 1 || c.Port > 65535 {
		return nil
	}
	if c.SNI == "" {
		c.SNI = c.Domain
	}
	return c
}

// parseVLESSEncryptionURI 只从管理员链接里取公开的 encryption 参数。
//
// ⚠️ userinfo（管理员 UUID）被**完全忽略**，函数也不返回它 ——
// 买家必须使用自己的 realityUUIDForUser(账号)，否则会用管理员身份连接。
// expectedHost / expectedPort 用来确认这条 URI 与 capabilities 指向同一入站；
// 不一致时拒绝，避免把 A 节点的密钥配到 B 节点上（握手必然失败）。
func parseVLESSEncryptionURI(raw, expectedHost string, expectedPort int) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "vless" {
		return "", errors.New("节点返回的 VLESS Encryption 链接格式无效")
	}
	// 刻意不读取 parsed.User —— 那是管理员 UUID，不属于买家。
	encryption := strings.TrimSpace(parsed.Query().Get("encryption"))
	if !validVLESSEncryption(encryption) {
		return "", errors.New("节点未提供有效的公开客户端 encryption 参数")
	}
	if expectedHost != "" && !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return "", errors.New("VLESS Encryption 链接的地址与能力声明不一致")
	}
	if expectedPort > 0 && parsed.Port() != strconv.Itoa(expectedPort) {
		return "", errors.New("VLESS Encryption 链接的端口与能力声明不一致")
	}
	return encryption, nil
}

// portalVLESSEncInfo 是 vlessenc-status 的**对外**响应结构。
//
// ⚠️ 2026-10-25 五轮排查第二轮：这里刻意**不声明** Config.URI 与 Config.UUID。
// 上游 config.uri 是**管理员**的 vless:// 链接，userinfo 是管理员 UUID；
// 这个结构体会被 /api/nodes/{id}/portal-status 原样序列化给浏览器，
// 声明了就等于把管理员身份发给每个打开面板的会话 —— 而前端只用
// domain / sni / port 三个字段，从不读 uri。
// 需要读 URI 的只有服务端自己（portalVLESSEncStatus），见下。
type portalVLESSEncInfo struct {
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	Config    struct {
		Domain string `json:"domain"`
		SNI    string `json:"sni"`
		Port   int    `json:"port"`
	} `json:"config"`
}

// portalVLESSEncStatus 是服务端内部解析用的完整响应（含管理员 URI）。
// 它**绝不能**被直接写进 HTTP 响应体。
type portalVLESSEncStatus struct {
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	Config    struct {
		URI    string `json:"uri"`
		Domain string `json:"domain"`
		SNI    string `json:"sni"`
		Port   int    `json:"port"`
	} `json:"config"`
}

// readVLESSEncryptionStatus 通过 Basic 会话读取公开参数。
func readVLESSEncryptionStatus(ctx context.Context, target *portalWebTarget, cap *vlessEncCapability) (*nodeVLESSEncryption, error) {
	if target == nil {
		return nil, errPortalNoBasic
	}
	envelope, err := portalWebCall(ctx, target, http.MethodGet, "vlessenc-status", nil)
	if err != nil {
		return nil, err
	}
	var status portalVLESSEncStatus
	if err := json.Unmarshal(envelope.Data, &status); err != nil {
		return nil, errors.New("VLESS Encryption 状态响应无法解析")
	}
	if !status.Installed || !status.Active {
		return nil, errors.New("节点报告 VLESS Encryption 未启用（与能力声明不一致）")
	}
	encryption, err := parseVLESSEncryptionURI(status.Config.URI, cap.Domain, cap.Port)
	if err != nil {
		return nil, err
	}
	return &nodeVLESSEncryption{
		Domain:     cap.Domain,
		SNI:        cap.SNI,
		Encryption: encryption,
		Port:       cap.Port,
	}, nil
}

// applyVLESSEncryptionCapability 在能力快照上补「能否交付」的结论。
//
// capabilities 本身不含客户端密钥，所以节点页只能看到 active 却不知道
// 买家拿不拿得到可用配置。这里明确写出 delivery_error，前端据此提示
// 「不可交付」，而不是显示一个看似可用、实际拿不到参数的通道。
func applyVLESSEncryptionCapability(ctx context.Context, m map[string]any, target *portalWebTarget) {
	applyVLESSEncryptionCapabilityWithTarget(ctx, m, target, nil)
}

// applyVLESSEncryptionCapabilityWithTarget 与上面相同，只是允许调用方复用
// 已加载的网页凭据（2026-10-25 五轮排查第二轮），并原样带出加载失败的原因
// （第三轮）—— 「没绑凭据」与「凭据解密失败」要分开说。
func applyVLESSEncryptionCapabilityWithTarget(ctx context.Context, m map[string]any, target *portalWebTarget, targetErr error) {
	if m == nil {
		return
	}
	cap := vlessEncFromCapability(m)
	if cap == nil {
		// 未启用 / 未安装：没有通道，也不需要「不可交付」警告。
		return
	}
	m["client_encryption_present"] = false
	if target == nil {
		if targetErr != nil && !errors.Is(targetErr, errPortalNoBasic) {
			m["delivery_error"] = truncateRunes("读取节点门户凭据失败，该通道不可交付："+targetErr.Error(), 180)
			return
		}
		m["delivery_error"] = "未绑定 portal 网页凭据，无法读取公开的客户端 encryption 参数；该通道不可交付"
		return
	}
	if _, err := readVLESSEncryptionStatus(ctx, target, cap); err != nil {
		if isPortalEndpointMissing(err.Error()) {
			m["delivery_error"] = "节点门户版本过旧（无 vlessenc-status 端点），该通道不可交付"
			return
		}
		m["delivery_error"] = truncateRunes("公开客户端 encryption 参数不可用："+err.Error(), 180)
		return
	}
	m["client_encryption_present"] = true
	delete(m, "delivery_error")
}

// loadNodeVLESSEncryption 供订阅构建使用：返回可下发的客户端参数，
// 或一句**可读的不可交付原因**（第二返回值）。两者互斥。
func (s *Server) loadNodeVLESSEncryption(ctx context.Context, nodeID string, cap *vlessEncCapability) (*nodeVLESSEncryption, string) {
	if cap == nil {
		return nil, ""
	}
	target, err := s.loadPortalWebTarget(ctx, nodeID)
	if err != nil {
		return nil, portalTargetDeliveryReason(err)
	}
	return loadNodeVLESSEncryptionWithTarget(ctx, target, cap)
}

// loadNodeVLESSEncryptionWithTarget 与 loadNodeVLESSEncryption 相同，
// 但复用调用方已经加载好的网页凭据 —— 调用方通常还要用它确认 Reality 入站，
// 再查一次库是白白的往返（2026-10-25 五轮排查第二轮）。
func loadNodeVLESSEncryptionWithTarget(ctx context.Context, target *portalWebTarget, cap *vlessEncCapability) (*nodeVLESSEncryption, string) {
	if cap == nil {
		return nil, ""
	}
	if target == nil {
		return nil, "节点未绑定 portal 网页凭据，无法读取公开的客户端 encryption 参数；VLESS Encryption 不可交付"
	}
	enc, err := readVLESSEncryptionStatus(ctx, target, cap)
	if err != nil {
		if isPortalEndpointMissing(err.Error()) {
			return nil, "节点门户版本过旧（无 vlessenc-status 端点），VLESS Encryption 不可交付"
		}
		return nil, truncateRunes(err.Error()+"；VLESS Encryption 不可交付", 180)
	}
	return enc, ""
}

// portalTargetDeliveryReason 把「拿不到网页凭据」翻译成面向操作者的原因。
// 凭据缺失（errPortalNoBasic）是配置问题，其余是读取失败 —— 处理动作不同。
func portalTargetDeliveryReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errPortalNoBasic) {
		return "节点未绑定 portal 网页凭据，无法读取公开的客户端 encryption 参数；VLESS Encryption 不可交付"
	}
	return truncateRunes("读取节点门户凭据失败，VLESS Encryption 不可交付："+err.Error(), 180)
}

// vlessEncName 与上游 artifacts() 的后缀保持一致。
func (n subscriptionNode) VLESSEncName() string { return n.Name + "-VLESS-Enc" }

// vlessEncURI 拼 VLESS Encryption 直链，字段顺序对齐上游
// vlessenc_uri_for_user()：
//
//	vless://<买家UUID>@<domain>:<port>?encryption=...&security=tls&sni=...
//	  &type=tcp&headerType=none&fp=chrome#<name>
//
// encryption 必须逐字保留（只做百分号编码，不改内容）—— 任何截断都会
// 导致握手失败，而客户端报错只会说 authentication failed。
func vlessEncURI(n subscriptionNode, uuid string) string {
	v := n.VLESSEnc
	params := []string{
		"encryption=" + pctEncode(v.Encryption),
		"security=tls",
		"sni=" + pctEncode(v.SNI),
		"type=tcp",
		"headerType=none",
		"fp=chrome",
	}
	return fmt.Sprintf("vless://%s@%s?%s#%s",
		uuid, net.JoinHostPort(v.Domain, strconv.Itoa(v.Port)), strings.Join(params, "&"), pctEncode(n.VLESSEncName()))
}

// clashVLESSEncryptionProxy 生成 mihomo/Clash Meta 的 vless + encryption 节点。
//
// 字段依据 mihomo 官方文档（docs/config/proxies/vless.en.md）：
// `encryption` 是 **vless 代理的顶层字段**，不是 tls 子项。
// 不带 flow：上游入站不带 flow，VLESS Encryption 与 Reality 的
// xtls-rprx-vision 不是一回事。
func clashVLESSEncryptionProxy(n subscriptionNode, uuid string) map[string]any {
	v := n.VLESSEnc
	return map[string]any{
		"name":               n.VLESSEncName(),
		"type":               "vless",
		"server":             v.Domain,
		"port":               v.Port,
		"uuid":               uuid,
		"udp":                true,
		"tls":                true,
		"servername":         v.SNI,
		"network":            "tcp",
		"encryption":         v.Encryption,
		"client-fingerprint": "chrome",
	}
}
