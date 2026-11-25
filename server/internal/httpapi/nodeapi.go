package httpapi

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

	"github.com/jackc/pgx/v5"
)

// The node-side portal of hysteria2-installer exposes GET /api/v1/node/meta guarded
// only by a Bearer key, and that key also unlocks the node's obfs/auth passwords.
// So the key is stored with the same envelope encryption as SSH private keys, and the
// URL is treated as attacker-controlled: every request goes through a dialer that
// refuses to connect to non-global addresses, which keeps the API server from being
// used as a proxy into SE's internal network or the cloud metadata endpoint.
func newPortalClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("节点 API 不接受重定向")
		},
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip, ok := migrationDialIP(ctx); ok {
					return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
				}
				ips, err := lookupIPWithPublicFallback(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("解析 %s 失败: %w", host, err)
				}
				for _, ip := range ips {
					// 2026-11-03：统一走 portaltls.PublicIP（额外拒绝 CGNAT / NAT64 等，阿里云元数据在 100.100.100.200）。
					if !portaltls.PublicIP(ip.IP) {
						return nil, fmt.Errorf("%s 解析到非公网地址，已拒绝访问", host)
					}
				}
				// 全部地址校验通过后按 IPv4 优先依次尝试拨号，避免双栈域名首个地址不可达时直接失败。
				return portaltls.DialCheckedIPs(ctx, dialer, network, port, ips)
			},
		},
	}
}

var portalClient = newPortalClient()

// lookupIPWithPublicFallback 先用系统解析；失败再直接问公共 DNS。
// 刚建好的域名（一键安装自动配 Cloudflare）常被容器 / 宿主机的解析器负缓存成「不存在」，
// 导致节点明明装好了，面板却连不上它的 API。
func lookupIPWithPublicFallback(ctx context.Context, host string) ([]net.IPAddr, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err == nil && len(ips) > 0 {
		return ips, nil
	}
	if net.ParseIP(host) != nil {
		return ips, err
	}
	for _, ns := range []string{"1.1.1.1:53", "8.8.8.8:53", "223.5.5.5:53"} {
		ns := ns
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", ns)
		}}
		if got, lerr := r.LookupIPAddr(ctx, host); lerr == nil && len(got) > 0 {
			return got, nil
		}
	}
	return ips, err
}

type portalMetaResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Time  int64  `json:"time"`
	Meta  struct {
		PublicIP     string `json:"public_ip"`
		ServerName   string `json:"server_name"`
		ListenPort   int    `json:"listen_port"`
		HopPortRange string `json:"hop_port_range"`
		CertType     string `json:"cert_type"`
		SubsPort     int    `json:"subscription_port"`
		AuthPassword string `json:"auth_password"`
		ObfsPassword string `json:"obfs_password"`
	} `json:"meta"`
	UsersCount int `json:"users_count"`
}

// portalPathTokenPattern matches the portal's {token} path segment. The portal
// mints it with secrets.token_hex(32), i.e. 64 lowercase hex chars, but older
// installs used token_hex(16) (32 chars), so the pattern accepts both rather
// than locking to one length. Being strict here matters: the token is
// interpolated into request paths.
var portalPathTokenPattern = regexp.MustCompile(`^[a-f0-9]{16,128}$`)

// splitPortalURL extracts the {token} segment from a portal URL.
//
// The portal's own portal-access.json carries `"url": "https://host:port/<token>/"`,
// and that is what an operator naturally pastes. Accepting it directly is what
// lets the whole "manage a node by API" flow stay a copy-paste of one file
// instead of asking the operator to hand-split a URL.
//
// Returns ok=false when there is no trailing path segment, which is the normal
// case for a URL the operator typed by hand ("https://host:8443").
func splitPortalURL(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	// 2026-11-06：与 normalizePortalURLWithToken 一样先补 scheme。原来对「hy2.example.com:8443/<token>/」
	// 这类不带 https:// 的输入，url.Parse 把主机名当成 scheme、路径为空（IP:端口 的写法直接解析失败），
	// token 一律是空串：门户网页凭据照样保存，节点却永远读不出买家的客户端参数，对所有买家静默失效。
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", "", false
	}
	if !portalPathTokenPattern.MatchString(segments[0]) {
		return "", "", false
	}
	return parsed.Host, segments[0], true
}

// normalizePortalURLWithToken is normalizePortalURL but tolerates (and strips)
// a trailing /{token}/ segment, returning the origin as the base URL.
func normalizePortalURLWithToken(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("请填写节点 API 地址")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(strings.TrimSuffix(value, "/api/v1/node/meta"))
	if err != nil {
		return "", errors.New("地址不合法")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("只支持 http/https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("地址里不要带账号或查询参数")
	}
	trailing := strings.Trim(parsed.Path, "/")
	if trailing != "" {
		segments := strings.Split(trailing, "/")
		if len(segments) != 1 || !portalPathTokenPattern.MatchString(segments[0]) {
			return "", errors.New("地址里不要带账号、路径或查询参数")
		}
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String(), nil
}

// originFromBase returns scheme://host:port for an already-normalised base URL.
func originFromBase(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return strings.TrimSuffix(baseURL, "/")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String()
}

// portalEnvelope is the common shape every portal response shares. The REST API
// answers `{"ok": true, ...}` on success and `{"ok": false, "error": "..."}` on
// rejection, so the caller can unwrap the payload while keeping the envelope
// fields available for the error path.
type portalEnvelope struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  json.RawMessage `json:"-"` // filled by portalCall, not by encoding/json
}

// portalCall performs one authenticated request against the node-side portal.
//
// Every node-side feature (users, proxy services, the read-only overview) shares
// this single entry point so that the security-relevant posture — the
// private-address-refusing dialer, the 10s deadline, the redirect refusal, and
// the response-size cap — cannot be accidentally weakened in a copy-paste
// variant. The body is capped at 256 KiB because the portal returns a full
// Clash YAML and sing-box JSON per created user, and silently truncating them
// would hand the operator a broken subscription file.
//
// `path` must be a path under /api/v1/ (e.g. "/users/list"). It is joined with
// the already-normalised base URL, so no caller can smuggle a scheme or host.
func portalCall(ctx context.Context, baseURL, apiKey, method, path string, payload any) (*portalEnvelope, error) {
	return portalCallWithin(ctx, 10*time.Second, baseURL, apiKey, method, path, payload)
}

// portalCallWithin 与 portalCall 相同，只是截止时间可调 —— 给本身就要跑
// 十几秒的节点侧检测（如 /unlock-check）用。其余安全姿势完全共用。
// portalResponseLimit 节点 API 响应体上限，见 portalCallWithin 里的说明。
const portalResponseLimit = 8 << 20

func portalCallWithin(ctx context.Context, timeout time.Duration, baseURL, apiKey, method, path string, payload any) (*portalEnvelope, error) {
	if len(apiKey) < 16 {
		return nil, errors.New("api_key 太短，确认是 portal-access.json 里的那个 48 位密钥")
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("构造请求失败")
		}
		body = strings.NewReader(string(encoded))
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, baseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client, err := portaltls.Client(migrationAwareClient(portalClient, ctx), req.URL.String())
	if err != nil {
		return nil, err
	}
	// portalClient 自带 10s 的 Client.Timeout，会盖过上面的 ctx 截止时间。
	// 需要更长时间的调用（如 /unlock-check）用一份放宽了 Timeout 的浅拷贝 ——
	// Transport 共用，拨号、TLS 固定等安全姿势不变。
	if client.Timeout != 0 && client.Timeout < timeout {
		relaxed := *client
		relaxed.Timeout = timeout
		client = &relaxed
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接节点 API 失败: %w", stripURLError(err))
	}
	defer resp.Body.Close()
	// 上限原为 256 KiB：/users/list 会把节点级的 measurement_gaps（最多 100 条）复制进
	// 每个用户，53 个用户的日本-东京实测 628 KB，被截断后报「不是合法 JSON」，
	// 对账、同步、开户全部失败（2026-10-28）。放宽到 8 MiB，超限时明确报「响应过大」。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, portalResponseLimit+1))
	if err != nil {
		return nil, errors.New("读取响应失败")
	}
	if len(raw) > portalResponseLimit {
		return nil, fmt.Errorf("节点 API 响应超过 %d MiB，已拒绝解析", portalResponseLimit>>20)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("api_key 不被接受 (401)：确认该节点是用「商城集群 Agent 模式」安装的")
	}

	// The portal answers a plain 404 with a JSON body, so the envelope is parsed
	// before the status code is judged: a 404 carrying {"ok":false,"error":...}
	// carries a far better message than the bare number.
	var parsed portalEnvelope
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("节点 API 返回 %d", resp.StatusCode)
		}
		return nil, errors.New("响应不是合法 JSON，确认这是 hysteria2-installer 的 portal")
	}
	if !parsed.OK {
		if parsed.Error != "" {
			return nil, errors.New("节点 API 报错: " + parsed.Error)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("节点 API 返回 %d", resp.StatusCode)
		}
		return nil, errors.New("节点 API 未返回 ok")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("节点 API 返回 %d，未确认操作成功", resp.StatusCode)
	}
	parsed.Data = raw
	return &parsed, nil
}

func fetchPortalMeta(ctx context.Context, baseURL, apiKey string) (*portalMetaResponse, error) {
	envelope, err := portalCall(ctx, baseURL, apiKey, http.MethodGet, "/node/meta", nil)
	if err != nil {
		return nil, err
	}
	var parsed portalMetaResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		return nil, errors.New("响应不是合法 JSON，确认这是 hysteria2-installer 的 portal")
	}
	if parsed.Meta.ListenPort < 1 || parsed.Meta.ListenPort > 65535 {
		return nil, errors.New("meta 里没有合法的 listen_port")
	}
	return &parsed, nil
}

// portalTarget is everything a node-side handler needs to reach the portal:
// the base URL and the already-decrypted Bearer key.
type portalTarget struct {
	NodeID   string
	NodeName string
	BaseURL  string
	APIKey   string
}

// loadPortalTarget resolves a node id to its portal endpoint, decrypting the
// stored key. It normalises the "node not found" and "node has no portal API"
// cases into distinct sentinel errors so every caller renders the same message
// instead of inventing its own wording.
var (
	errPortalNodeNotFound = errors.New("节点不存在")
	errPortalNotBound     = errors.New("该节点没有绑定节点侧 API，请先在节点详情里绑定")
	// errPortalNodeBusy：节点所在主机有安装 / 迁移任务在跑。回 409 和原文 ——
	// 原来是普通 error，落进 500「解析节点凭据失败」，操作者会去查凭据，实际只需等任务结束。
	errPortalNodeBusy = errors.New("节点所在主机正在安装或迁移，请等任务完成后再操作")
)

func (s *Server) loadPortalTarget(ctx context.Context, nodeID string) (*portalTarget, error) {
	// 畸形 ID 先挡掉：nodeMaintenance 的查询带 $1::uuid，查询报错时按「维护中」处理（fail-closed）。
	if !uuidPattern.MatchString(strings.TrimSpace(nodeID)) {
		return nil, errPortalNodeNotFound
	}
	if s.nodeMaintenance(ctx, nodeID) {
		return nil, errPortalNodeBusy
	}
	var name string
	var baseURL string
	var ciphertext []byte
	err := s.db.QueryRow(ctx, `SELECT name, COALESCE(api_url,''), api_key FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&name, &baseURL, &ciphertext)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, errPortalNodeNotFound
		}
		return nil, err
	}
	if baseURL == "" || len(ciphertext) == 0 {
		return nil, errPortalNotBound
	}
	if s.box == nil {
		return nil, errors.New("服务端未配置凭据加密密钥")
	}
	plain, err := s.box.Open(ciphertext)
	if err != nil {
		return nil, errors.New("节点 API 密钥解密失败")
	}
	return &portalTarget{NodeID: nodeID, NodeName: name, BaseURL: baseURL, APIKey: string(plain)}, nil
}

// writePortalTargetError renders the sentinel errors above with the right status
// code, falling back to 500 for anything unexpected.
func (s *Server) writePortalTargetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPortalNodeNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errPortalNotBound):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, errPortalNodeBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		// 内部错误的原文只进日志，不回给调用方。
		//
		// 原始实现是 writeJSON(..., err.Error())，实测下能把 Postgres 的
		// 「invalid input syntax for type uuid ... SQLSTATE 22P02」原样送到
		// 响应体里 —— 表名、列类型、错误码全暴露了。这类细节对操作者没有
		// 诊断价值（他们改不了数据库），对探测者却是免费的内部结构情报。
		s.logger.Warn("node portal: resolve target failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "解析节点凭据失败，请查看服务端日志",
		})
	}
}

// matchServer links an imported node to an already enrolled host by public IP or hostname.
func (s *Server) matchServer(ctx context.Context, publicIP, serverName string) (string, string, error) {
	publicIP = strings.TrimSpace(publicIP)
	serverName = strings.TrimSuffix(strings.TrimSpace(serverName), ".")
	rows, err := s.db.Query(ctx, `SELECT id, name, host FROM servers WHERE deleted_at IS NULL`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	// 2026-11-04：IP 匹配优先于主机名匹配。原来按行序返回第一条「IP 或主机名」命中：
	// 一台主机按域名登记、另一台按 IP 登记时，节点归到哪台取决于物理行序 ——
	// 可能挂到另一台机器名下（之后主机级告警、换 IP、删除主机的判断都跟着错）。
	var byNameID, byNameName string
	for rows.Next() {
		var id, name, host string
		if err := rows.Scan(&id, &name, &host); err != nil {
			return "", "", err
		}
		if publicIP != "" && strings.EqualFold(strings.TrimSpace(host), publicIP) {
			return id, name, nil
		}
		if byNameID == "" && serverName != "" && strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(host), "."), serverName) {
			byNameID, byNameName = id, name
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return byNameID, byNameName, nil
}

// ensureHostForImportedNode returns the id of a host row representing a node that
// was added purely through its portal API.
//
// The panel's data model ties every node to a host, because that is where SSH
// credentials live for metrics collection. An API-imported node has no SSH
// credential yet, but it still needs a host row so it appears in the fleet view
// and the foreign key holds. Rather than making the operator leave the import
// dialog and invent an SSH user they may not have, the node's own public IP is
// recorded as a credential-less host. Uploading a real credential later is a
// no-op upgrade: collection simply starts working.
//
// Reuses an existing host with the same address when there is one, so importing
// several nodes from one machine does not pile up duplicate hosts.
func (s *Server) ensureHostForImportedNode(ctx context.Context, publicIP, serverName string) (string, error) {
	existing, _, err := s.matchServer(ctx, publicIP, serverName)
	if err != nil {
		return "", fmt.Errorf("查询已有主机失败：%w", err)
	}
	if existing != "" {
		return existing, nil
	}
	address := strings.TrimSpace(publicIP)
	if address == "" {
		// No public IP reported (unusual, but the portal is the authority on
		// itself). Fall back to the hostname so the row is still identifiable.
		address = strings.TrimSpace(serverName)
	}
	if address == "" {
		return "", fmt.Errorf("节点未上报公网地址，无法自动登记主机")
	}
	name := strings.TrimSpace(serverName)
	if name == "" {
		name = address
	}
	// ssh_user is NOT NULL and non-empty by schema. Historically this was a
	// sentinel (`api-only`) chosen to be obviously-not-a-login — but the collector
	// only reads servers that HAVE a credential, and until one is uploaded this
	// host has none, so the sentinel was never actually used to connect. The user
	// is now corrected at credential-upload time (see uploadCredential).
	//
	// It is still worth not writing a fake-looking name: if someone later uploads
	// a credential without specifying a username, `root` is what these nodes
	// actually use, and a wrong user is the failure mode that gets this host's IP
	// banned by the node's fail2ban. Default to the real one.
	var id string
	// 自动识别地区与商家，用它来命名 —— 见 geoapply.go 的三条业务规则。
	//
	// 这里的输入是**节点自己上报的公网 IP**（portal 的 node/meta 里的
	// public_ip），比用户手填的地址更可信：那是节点从自己的网卡上读到的。
	// 但仍属外部输入，所以 Resolver 会复核它是不是公网地址。
	//
	// 这是新建主机行，current 全空 —— 识别不出来时 applyGeo 原样返回，
	// 于是 name 保持上面那套原有逻辑（节点自报 server_name，再退到地址）。
	//
	// 注意 NameAuto 传 true 而非 false：false 在 applyGeo 里会被解释成
	// "这个已有名字是人打的，别碰"；而这里的 name 只是**兜底占位**，
	// 我们希望识别成功时它能被换成「地区-商家」。
	geoResult := applyGeo(geoCurrent{
		Name: name,
		// 名字是刚算出来的占位值，不是人工输入，因此标记为可替换。
		NameAuto:     true,
		RegionAuto:   true,
		ProviderAuto: true,
	}, s.resolveGeo(ctx, address))
	name = geoResult.Name

	// provider 的兜底语义要保留：这是一台「通过 API 导入、还没有 SSH
	// 凭据」的机器，标成「API 节点」能让它在列表里一眼认出来源。
	// 但识别出真实商家时，真实商家优先。
	provider := strings.TrimSpace(geoResult.Provider)
	if provider == "" {
		provider = "API 节点"
		// 兜底值不是识别结果，标记为 manual，避免下次识别时被当成
		// "已有值" 而永远不再尝试填真实商家。
		geoResult.ProviderAuto = false
	}

	// ON CONFLICT targets the partial unique index on (host, ssh_port) limited to
	// live rows (migration 005). The bare `ON CONFLICT DO NOTHING` form does not
	// match a *partial* index, so the target must be spelled out or the insert
	// errors instead of deduplicating.
	err = s.db.QueryRow(ctx, `
INSERT INTO servers (name, host, ssh_port, ssh_user, region, provider, tags,
                     name_source, region_source, provider_source)
VALUES ($1, $2, 22, 'root', $3, $4, '{}'::text[], $5, $6, $7)
ON CONFLICT (host, ssh_port) WHERE deleted_at IS NULL DO NOTHING
RETURNING id`, name, address, geoResult.Region, provider,
		autoSource(geoResult.NameAuto), autoSource(geoResult.RegionAuto), autoSource(geoResult.ProviderAuto)).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("自动登记主机失败: %w", err)
	}
	// Deduplicated against an existing host — re-read it.
	if matched, _, err := s.matchServer(ctx, publicIP, serverName); err != nil {
		return "", fmt.Errorf("重新读取已存在主机失败：%w", err)
	} else if matched != "" {
		return matched, nil
	}
	// Deduplicated against an existing host — re-read it.
	// 补 ORDER BY：同一 host 可能因历史原因存在多行（改过 SSH 端口后又改回来
	// 等），没有 ORDER BY 时「读回哪一行」不确定。取最早的一条，与
	// servers 列表按 created_at 的稳定顺序一致。
	row := s.db.QueryRow(ctx, `SELECT id FROM servers WHERE host=$1 AND ssh_port=22 AND deleted_at IS NULL ORDER BY created_at ASC, id ASC LIMIT 1`, address)
	if scanErr := row.Scan(&id); scanErr != nil {
		return "", fmt.Errorf("自动登记主机后无法读回: %w", scanErr)
	}
	return id, nil
}

// portalAccessInput 是绑定/预览共用的入参形态。
//
// 三种填法都接受，因为操作者的手头信息不一样：
//
//  1. 粘整个 portal-access.json 到 `access_json`（最省事，也是推荐的）
//  2. 分别填 url / api_key（等价于旧行为，向后兼容）
//  3. 在上面基础上补 basic_user / basic_password，解锁网页会话能力
//
// username/password 是可选的：不填就仍是一个「只有 Bearer 能力」的节点，
// 面板会据 portal_basic_user 是否为空来决定哪些按钮可点。
type portalAccessInput struct {
	URL           string `json:"url"`
	APIKey        string `json:"api_key"`
	BasicUser     string `json:"basic_user"`
	BasicPassword string `json:"basic_password"`
	AccessJSON    string `json:"access_json"`
}

// resolve 把三种填法归一成 (baseURL, pathToken, apiKey, basicUser, basicPassword)。
// basicUser/basicPassword 可能为空串，表示调用方没提供。
func (in *portalAccessInput) resolve() (baseURL, pathToken, apiKey, basicUser, basicPassword string, err error) {
	raw := strings.TrimSpace(in.AccessJSON)
	if raw != "" {
		var parsed struct {
			URL      string `json:"url"`
			Username string `json:"username"`
			Password string `json:"password"`
			APIKey   string `json:"api_key"`
		}
		if jsonErr := json.Unmarshal([]byte(raw), &parsed); jsonErr != nil {
			return "", "", "", "", "", errors.New("这段内容不是合法的 portal-access.json")
		}
		if strings.TrimSpace(parsed.URL) == "" {
			return "", "", "", "", "", errors.New("portal-access.json 里没有 url 字段")
		}
		base, normErr := normalizePortalURLWithToken(parsed.URL)
		if normErr != nil {
			return "", "", "", "", "", normErr
		}
		// token 是可选的：url 里没有路径段时只有 /api/v1/ 那半边可用。
		_, token, _ := splitPortalURL(parsed.URL)
		// 显式填写的字段优先，允许「粘 JSON 但覆盖某一项」。
		apiKey = strings.TrimSpace(in.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(parsed.APIKey)
		}
		basicUser = strings.TrimSpace(in.BasicUser)
		if basicUser == "" {
			basicUser = strings.TrimSpace(parsed.Username)
		}
		basicPassword = in.BasicPassword
		if basicPassword == "" {
			basicPassword = parsed.Password
		}
		return base, token, apiKey, basicUser, basicPassword, nil
	}

	base, err := normalizePortalURLWithToken(in.URL)
	if err != nil {
		return "", "", "", "", "", err
	}
	_, token, _ := splitPortalURL(in.URL)
	return base, token, strings.TrimSpace(in.APIKey), strings.TrimSpace(in.BasicUser), in.BasicPassword, nil
}

// probeBasicAuth 验证这对 Basic 凭据真的能开网页会话。
//
// 用一个无副作用的 GET 做探测，而不是只看格式：凭据可能是从另一台节点抄来的、
// 或者密码已轮换。绑定时就报错，远好过等到操作者点「重启」才发现。
//
// 失败不阻断绑定 —— 可能只是临时网络问题。但会把结果回给前端，并在库里
// 把 portal_basic_user 存下来（凭据本身是对的，探不通是另一回事）。
func probeBasicAuth(ctx context.Context, origin, pathToken, user, password string) error {
	if user == "" || password == "" {
		return errPortalNoBasic
	}
	target := &portalWebTarget{Origin: origin, PathToken: pathToken, Username: user, Password: password}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := portalWebCall(probeCtx, target, http.MethodGet, "check-version", nil)
	return err
}

// POST /api/nodes/preview — read-only probe used by the import dialog. Nothing is stored.
func (s *Server) previewNodeAPI(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	var input portalAccessInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	input.URL = strings.TrimSpace(input.URL)
	baseURL, pathToken, apiKey, basicUser, basicPassword, err := input.resolve()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	meta, err := fetchPortalMeta(r.Context(), baseURL, apiKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// 顺带探一次 Basic：能通就告诉前端「这个节点可以全功能管理」，
	// 不能通也照常返回，只是把 basic_error 带上供操作者判断。
	basicOK := false
	basicError := ""
	if basicUser != "" && basicPassword != "" {
		origin := originFromBase(baseURL)
		if pathToken == "" {
			basicError = "地址里没有门户路径段（{token}），无法组成网页会话地址；请直接粘贴 portal-access.json 的 url"
		} else if err := probeBasicAuth(r.Context(), origin, pathToken, basicUser, basicPassword); err != nil {
			basicError = err.Error()
		} else {
			basicOK = true
		}
	}

	serverID, serverName, err := s.matchServer(r.Context(), meta.Meta.PublicIP, meta.Meta.ServerName)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// suggested_name goes through geo resolution so the operator sees the same
	// "城市-商家-端口" naming that manual entries get, instead of the raw
	// server_name (typically a domain like "hy2.example.com") which says nothing
	// about where the box actually lives.
	//
	// 数据库无法枚举现有名称时返回错误，不能推荐可能重复的节点名。
	suggestedName, err := s.disambiguateNodeName(r.Context(), suggestedNodeName(meta.Meta.ServerName, meta.Meta.PublicIP,
		meta.Meta.ListenPort, s.resolveGeo(r.Context(), meta.Meta.PublicIP)))
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_port":    meta.Meta.ListenPort,
		"domain":         meta.Meta.ServerName,
		"public_ip":      meta.Meta.PublicIP,
		"hop_port_range": meta.Meta.HopPortRange,
		"cert_type":      meta.Meta.CertType,
		"users_count":    meta.UsersCount,
		"suggested_name": suggestedName,
		"server_id":      serverID,
		"server_name":    serverName,
		"api_url":        baseURL,
		"has_path_token": pathToken != "",
		"basic_ok":       basicOK,
		"basic_error":    basicError,
		// auto_host tells the import dialog it may submit without choosing a host:
		// the server will record one from the node's own public IP. This keeps the
		// "add a node" flow a two-field operation.
		"auto_host": serverID == "" && meta.Meta.PublicIP != "",
	})
}

// POST /api/nodes/{id}/api — bind (or rebind) a node to its portal API and refresh at once.
func (s *Server) bindNodeAPI(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input portalAccessInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	nodeID := r.PathValue("id")
	// 2026-11-03：安装 / 迁移进行中不允许改绑定。迁移会自己改写 api_url（含失败回滚），
	// 与这里交错会留下「地址是旧机器、密钥是新机器」的组合，此后每次调用都是 401。
	if s.nodeMaintenance(r.Context(), nodeID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点所在主机正在安装或迁移，完成后再改节点 API 绑定"})
		return
	}
	baseURL, pathToken, apiKey, basicUser, basicPassword, err := input.resolve()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.box == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "凭据加密未启用"})
		return
	}
	meta, err := fetchPortalMeta(r.Context(), baseURL, apiKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	ciphertext, err := s.box.Seal([]byte(apiKey))
	if err != nil {
		s.databaseError(w, r, err)
		return
	}

	// Basic 凭据与 api_key 同等加密。为空则整列置 NULL —— 这是「解绑网页会话
	// 能力」的表达方式，而不是存一个空串。
	var basicCipher []byte
	var basicUserValue *string
	if basicUser != "" && basicPassword != "" {
		basicCipher, err = s.box.Seal([]byte(basicPassword))
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		basicUserValue = &basicUser
	}
	var tokenValue *string
	if pathToken != "" {
		tokenValue = &pathToken
	}

	// 能力快照：绑定时就探一次，让 UI 不必等到用户点开面板才知道有没有。
	caps := map[string]any{"basic": basicUserValue != nil}
	if basicUserValue != nil {
		origin := originFromBase(baseURL)
		if pathToken == "" {
			caps["basic"] = false
			caps["basic_error"] = "缺少门户路径段"
		} else if err := probeBasicAuth(r.Context(), origin, pathToken, basicUser, basicPassword); err != nil {
			caps["basic"] = false
			caps["basic_error"] = err.Error()
		}
	}
	capsJSON, _ := json.Marshal(caps)

	// 2026-11-03：与整机流量计量同一把锁。换绑到另一台机器时，计量基线（开机以来的网卡计数器、
	// boot_id）属于旧机器：沿用它会把新机器开机以来的全部流量一次算进本周期，可能当场超额停机；
	// 而在途的一轮计量若在换绑之后才落库，又会把旧机器的计数器写回来当基线。
	unlockMeter, ok := lockTrafficMeterForRequest(w, r, nodeID)
	if !ok {
		return
	}
	defer unlockMeter()
	tag, err := s.db.Exec(r.Context(), `UPDATE hysteria_nodes
SET traffic_counter_rx = CASE WHEN api_url IS DISTINCT FROM $1 THEN NULL ELSE traffic_counter_rx END,
    traffic_counter_tx = CASE WHEN api_url IS DISTINCT FROM $1 THEN NULL ELSE traffic_counter_tx END,
    traffic_boot_id    = CASE WHEN api_url IS DISTINCT FROM $1 THEN NULL ELSE traffic_boot_id END,
    api_url=$1, api_key=$2, users_count=$3, api_checked_at=now(),
    portal_basic_user=$4, portal_basic_password=$5, portal_path_token=$6,
    portal_caps=$7::jsonb, portal_checked_at=now(),
    listen_port=COALESCE(NULLIF($8,0), listen_port),
    domain=CASE WHEN $9='' THEN domain ELSE $9 END,
    status='unknown', last_checked_at=NULL, updated_at=now()
WHERE id=$10::uuid AND deleted_at IS NULL`,
		baseURL, ciphertext, meta.UsersCount,
		basicUserValue, basicCipher, tokenValue, string(capsJSON),
		meta.Meta.ListenPort, meta.Meta.ServerName, nodeID)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机上已存在同端口节点"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	InvalidateSubscriptionCache()
	invalidateNodeCapsCache(nodeID)
	s.audit(r.Context(), r, "bind_node_api", "hysteria_node", nodeID, map[string]any{
		"api_url":     baseURL,
		"users_count": meta.UsersCount,
		// 只记「有没有」，绝不记凭据本身。
		"has_basic": basicUserValue != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "bound",
		"users_count": meta.UsersCount,
		"listen_port": meta.Meta.ListenPort,
		"has_basic":   basicUserValue != nil,
		"caps":        caps,
	})
}

// GET /api/nodes/{id}/api — re-read the portal and sync port / domain / user count.
func (s *Server) refreshNodeAPI(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	// 2026-11-06：走 loadPortalTarget，与其它门户接口同一套判据。原来手写一遍：没配加密密钥时
	// s.box 为 nil 直接 panic；密钥解密失败被报成「database request failed」；安装 / 迁移中也照常
	// 去连节点并改写端口与域名（迁移本身也在改这台节点的地址）。
	target, err := s.loadPortalTarget(r.Context(), nodeID)
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}
	meta, err := fetchPortalMeta(r.Context(), target.BaseURL, target.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE hysteria_nodes
SET users_count=$1, api_checked_at=now(),
    listen_port=COALESCE(NULLIF($2,0), listen_port),
    domain=CASE WHEN $3='' THEN domain ELSE $3 END,
    updated_at=now()
WHERE id=$4::uuid AND deleted_at IS NULL`, meta.UsersCount, meta.Meta.ListenPort, meta.Meta.ServerName, nodeID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	InvalidateSubscriptionCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "refreshed",
		"users_count": meta.UsersCount,
		"listen_port": meta.Meta.ListenPort,
		"domain":      meta.Meta.ServerName,
		"cert_type":   meta.Meta.CertType,
		"hop_range":   meta.Meta.HopPortRange,
		"node_time":   meta.Time,
	})
}

// DELETE /api/nodes/{id}/api — forget the endpoint and its key.
func (s *Server) unbindNodeAPI(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "解绑节点 API 需要 X-Confirm-Dangerous: yes"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	// 2026-11-03：安装 / 迁移进行中不允许改绑定。迁移会自己改写 api_url（含失败回滚），
	// 与这里交错会留下「地址是旧机器、密钥是新机器」的组合，此后每次调用都是 401。
	if s.nodeMaintenance(r.Context(), nodeID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点所在主机正在安装或迁移，完成后再改节点 API 绑定"})
		return
	}
	unlockMeter, ok := lockTrafficMeterForRequest(w, r, nodeID)
	if !ok {
		return
	}
	defer unlockMeter()
	// 2026-11-03：因流量超额被停用的节点不能解绑。解绑后计量只扫 api_key 非空的节点，
	// 面板再也不会为它发恢复请求 —— 节点上的代理服务会一直停着，下个周期也起不来。
	// 同时清掉计量基线：以后重新绑定（可能是另一台机器）要从新基线开始。
	var suspended bool
	if err := s.db.QueryRow(r.Context(), `SELECT traffic_suspended FROM hysteria_nodes WHERE id=$1::uuid AND api_url IS NOT NULL AND deleted_at IS NULL`, nodeID).Scan(&suspended); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "该节点没有绑定节点侧 API"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if suspended {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该节点因流量超额停用中：先在节点页取消限额（或等周期重置后自动恢复），再解绑"})
		return
	}
	// 2026-11-06：仍有订阅在这台节点上开着账号时不能解绑。解绑会抹掉 api_url / api_key，之后退款、到期、
	// 停用都要回收这些账号，却拿不到凭据（errPortalNotBound）：回收永远失败，买家账号在节点上一直能用，
	// 删节点时的凭据快照也是空的，连补删都做不了。先在订阅中心把这些订阅的账号收回，或直接删除节点（会先回收）。
	nodeKey, ok := canonicalUUID(nodeID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该节点没有绑定节点侧 API"})
		return
	}
	var mapped int
	if err := s.db.QueryRow(r.Context(), `SELECT count(*) FROM subscription_tokens
		WHERE deleted_at IS NULL AND node_accounts ? $1`, nodeKey).Scan(&mapped); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if mapped > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         fmt.Sprintf("还有 %d 条订阅在这台节点上开着账号；解绑后这些账号将无法回收。请先回收这些订阅的账号，或改用「删除节点」（会先回收账号）", mapped),
			"subscriptions": mapped,
		})
		return
	}
	// 持着计量锁，挂起状态只会由计量改变，所以「先查后改」在这里是安全的；条件里再带一次兜底。
	tag, err := s.db.Exec(r.Context(), `UPDATE hysteria_nodes SET
	api_url=NULL, api_key=NULL, users_count=NULL, api_checked_at=NULL,
	portal_basic_user=NULL, portal_basic_password=NULL, portal_path_token=NULL,
	portal_caps=NULL, portal_checked_at=NULL,
	traffic_counter_rx=NULL, traffic_counter_tx=NULL, traffic_boot_id=NULL, updated_at=now()
WHERE id=$1::uuid AND api_url IS NOT NULL AND deleted_at IS NULL AND NOT traffic_suspended`, nodeID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "节点状态刚刚变化，请刷新后重试"})
		return
	}
	InvalidateSubscriptionCache()
	invalidateNodeCapsCache(nodeID)
	s.audit(r.Context(), r, "unbind_node_api", "hysteria_node", nodeID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "unbound"})
}
