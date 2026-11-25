package httpapi

// portalweb.go — 走 portal 网页会话（Basic 鉴权）的那条链路。
//
// 背景：portal.py 有两套互相独立的鉴权，可达范围差别很大。
//
//	verify_api_key()    Authorization: Bearer <api_key>
//	  → /api/v1/ 下 9 个端点。见 nodeapi.go。
//
//	is_authenticated()  Authorization: Basic base64(user:password)
//	  → /{token}/ 下 21 个端点，即门户网页端的全部功能。见本文件。
//
// 而 portal.py 里 auth_hash = sha256(base64(username:password))，
// is_authenticated() 就是拿 Basic 头的 sha256 去比它 —— 所以 Basic 的那对凭据
// **等价于 portal 管理员密码**，权限远高于 api_key。
//
// 两套凭据来自同一个文件 portal-access.json：
//
//	{"url": "https://host:port/<token>/", "username": "...", "password": "...", "api_key": "..."}
//
// 面板长期只存了 api_key，于是网页会话那一整片能力（重启、升级、BBR、Reality、
// WARP、组件安装、真实速率）在面板上根本无法触达。本文件补上另一半。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"nodeops/server/internal/portaltls"
	"regexp"
	"strings"
	"time"
)

// portalWebCallTimeout 比 Bearer 那条略长：网页会话端点里有若干是要现执行
// 命令再回结果的（reality-status 要读 xray 配置、awg-state 要调 hy2-awgctl、
// check-version 要查远端版本），10s 偶发不够。15s 与 portaloverview 的
// 聚合超时保持一致。
const portalWebCallTimeout = 15 * time.Second

// portalWebBodyLimit 单独给到 1 MiB，比 Bearer 那条的 256 KiB 大。
// 原因是 awg-qr.svg 与 user-config 会回较大的文本块（含完整 Clash YAML、
// sing-box JSON、二维码 SVG）。截断这些内容不会报错，只会让操作者拿到
// 一个坏掉的配置文件 —— 这正是 nodeapi.go 里那段注释担心的事，所以宁可放宽。
const portalWebBodyLimit = 1 << 20

// errPortalNoBasic 在节点没有绑定 Basic 凭据时返回。措辞刻意指向「补填」而不是
// 「不支持」，因为能力是有的，只是凭据没给。
var errPortalNoBasic = errors.New("该节点未绑定 portal 访问凭据（username/password），无法使用网页会话功能；请在节点绑定时一并填入 portal-access.json 里的 username 与 password")

// uuidPattern 用于在访问数据库之前挡掉畸形的节点 ID。
//
// 起因是一轮实测：GET /api/nodes/not-a-uuid/portal-status 返回 500，
// 响应体里原样带着 Postgres 的错误
// （invalid input syntax for type uuid ... SQLSTATE 22P02）。
// 那既是不该出现的 5xx（调用方传错了东西，是 4xx 的范畴），
// 也是把数据库内部细节透给了前端。查询里的 $1::uuid 会对畸形输入直接抛错，
// 而 isNoRows 只认「查不到行」，于是错误一路传到了响应上。
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// portalWebTarget 是 Basic 链路需要的一切。
type portalWebTarget struct {
	NodeID   string
	NodeName string
	// Origin 形如 https://host:port —— 不含 /{token}/ 前缀。
	Origin string
	// PathToken 门户的 {token} 段。
	PathToken string
	Username  string
	Password  string
}

// webBase 拼出 /{token} 前缀。token 已由 parsePortalURL 严格校验过，
// 只会是十六进制串，因此直接拼接是安全的；这里再断言一次以防万一。
func (t *portalWebTarget) webBase() (string, error) {
	if !portalPathTokenPattern.MatchString(t.PathToken) {
		return "", errors.New("门户路径标识不合法")
	}
	return t.Origin + "/" + t.PathToken, nil
}

func (t *portalWebTarget) authorization() string {
	raw := t.Username + ":" + t.Password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

// portalWebCall 对 /{token}/ 前缀下的端点发一次带 Basic 鉴权的请求。
//
// 与 portalCall 共享同一套安全姿态（同一个 portalClient：拒私网拨号、拒重定向、
// 全局超时），这样两条链路的对外行为是一致的，不会出现「换条路子防护就没了」。
//
// method/path 里的 path 必须是 /{token}/ 之后的相对路径（如 "check-version"）。
// form 非空时按 application/x-www-form-urlencoded 发送 —— 这是 portal 网页端
// 所有 POST 处理器解析的格式（它们统一用 parse_qs），不是 JSON。
func portalWebCall(ctx context.Context, target *portalWebTarget, method, path string, form map[string]string, queries ...url.Values) (*portalEnvelope, error) {
	base, err := target.webBase()
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("内部错误：路径不该带查询或片段")
	}
	// 防呆：path 不允许多级回溯，避免拼出 /{token}/../../api/v1/... 这类越界。
	if strings.Contains(path, "..") {
		return nil, errors.New("内部错误：路径包含非法片段")
	}
	if len(queries) > 1 {
		return nil, errors.New("内部错误：只允许一组查询参数")
	}

	var body io.Reader
	contentType := ""
	if form != nil && method != http.MethodGet {
		values := make([]string, 0, len(form))
		for k, v := range form {
			values = append(values, urlEncode(k)+"="+urlEncode(v))
		}
		body = strings.NewReader(strings.Join(values, "&"))
		contentType = "application/x-www-form-urlencoded"
	}

	reqCtx, cancel := context.WithTimeout(ctx, portalWebRequestBudget(method, path))
	defer cancel()
	requestURL := base + "/" + path
	if len(queries) == 1 && len(queries[0]) > 0 {
		requestURL += "?" + queries[0].Encode()
	}
	req, err := http.NewRequestWithContext(reqCtx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", target.authorization())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client := *portalClient
	client.Timeout = portalWebRequestBudget(method, path)
	secured, err := portaltls.Client(migrationAwareClient(&client, ctx), req.URL.String())
	if err != nil {
		return nil, err
	}
	resp, err := secured.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接节点门户失败: %w", stripURLError(err))
	}
	defer resp.Body.Close()
	// 2026-11-03：多读 1 字节判断是否超限。原来按上限截断后再解析，超大的 user-config
	// 被截成半截 JSON，报成「响应不是合法 JSON」并附上前 200 字节 —— 与 portalCall 的处理不一致。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, portalWebBodyLimit+1))
	if err != nil {
		return nil, errors.New("读取响应失败")
	}
	if len(raw) > portalWebBodyLimit {
		return nil, fmt.Errorf("节点门户响应超过 %d KiB，已拒绝解析", portalWebBodyLimit>>10)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("portal 凭据不被接受 (401)：username/password 可能已轮换，请从节点 /etc/hysteria/portal-access.json 重新复制")
	}

	// awg-conf / awg-qr.svg 回的是裸文本或 SVG，不是信封。调用方若明确要
	// 这类原始响应，走 portalWebCallRaw；这里仍然按信封解析，解析失败就
	// 把原始体交给调用方判断（错误里带上前 200 字符，便于定位）。
	var parsed portalEnvelope
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("节点门户返回 %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("响应不是合法 JSON: %s", truncateForError(raw, 200))
	}
	if !parsed.OK {
		// portal 的 400 分支带的是业务错误（Invalid action 之类），
		// 这正是我们想透传给操作者的信息。
		if parsed.Error != "" {
			return nil, errors.New("节点门户报错: " + parsed.Error)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("节点门户返回 %d", resp.StatusCode)
		}
		return nil, errors.New("节点门户未返回 ok")
	}
	// 2026-11-03：非 2xx 即使信封里写着 ok:true 也不算成功（与 portalCall 一致）。
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("节点门户返回 %d", resp.StatusCode)
	}
	parsed.Data = raw
	return &parsed, nil
}

// urlEncode 做 application/x-www-form-urlencoded 的百分号编码。
// 单独写而不引 net/url.Values，是为了让「空格必须编成 +」这一约定在本文件里
// 一眼可见 —— portal 侧用 parse_qs 解析，它接受两种，但显式更稳。
func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

func truncateForError(raw []byte, limit int) string {
	text := strings.TrimSpace(string(raw))
	if len(text) <= limit {
		return text
	}
	// 按字符截断：节点回显的 stderr 常含中文，按字节切会留下半个字，
	// 运维在面板上看到的错误就成了一串问号。
	return truncateRunes(text, limit) + "…"
}

// loadPortalWebTarget 读取某节点的 Basic 凭据。与 loadPortalTarget 并列，
// 但多一个「凭据缺失」的前置判断 —— Basic 是可选绑定项，不是所有节点都有。
func (s *Server) loadPortalWebTarget(ctx context.Context, nodeID string) (*portalWebTarget, error) {
	// 畸形 ID 在碰数据库之前就挡掉。见 uuidPattern 上的注释：
	// 放过去会让 Postgres 的错误冒成 500 并透出 SQL 细节。
	// 必须早于 nodeMaintenance：它的查询带 $1::uuid，报错按「维护中」处理。
	if !uuidPattern.MatchString(strings.TrimSpace(nodeID)) {
		return nil, errPortalNodeNotFound
	}
	if s.nodeMaintenance(ctx, nodeID) {
		return nil, errPortalNodeBusy
	}
	var (
		name        string
		baseURL     string
		basicUser   *string
		basicCipher []byte
		storedToken *string
		apiCipher   []byte
	)
	err := s.db.QueryRow(ctx, `SELECT name, COALESCE(api_url,''), portal_basic_user, portal_basic_password,
		portal_path_token, api_key
	FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).
		Scan(&name, &baseURL, &basicUser, &basicCipher, &storedToken, &apiCipher)
	if err != nil {
		if isNoRows(err) {
			return nil, errPortalNodeNotFound
		}
		return nil, err
	}
	if baseURL == "" || len(apiCipher) == 0 {
		return nil, errPortalNotBound
	}
	if basicUser == nil || strings.TrimSpace(*basicUser) == "" || len(basicCipher) == 0 {
		return nil, errPortalNoBasic
	}
	if s.box == nil {
		return nil, errors.New("服务端未配置凭据加密密钥")
	}
	plain, err := s.box.Open(basicCipher)
	if err != nil {
		return nil, errors.New("portal 访问凭据解密失败")
	}

	// token 优先用库里存的；没有则从 api_url 反推（兼容 013 之前绑定的节点）。
	token := ""
	if storedToken != nil {
		token = strings.TrimSpace(*storedToken)
	}
	if token == "" {
		host, derived, ok := splitPortalURL(baseURL)
		if !ok {
			return nil, errors.New("无法从节点 API 地址推断门户路径，请重新绑定该节点")
		}
		token = derived
		_ = host
	}
	return &portalWebTarget{
		NodeID:    nodeID,
		NodeName:  name,
		Origin:    originFromBase(baseURL),
		PathToken: token,
		Username:  *basicUser,
		Password:  string(plain),
	}, nil
}

// isNoRows 是 pgx.ErrNoRows 的一个薄包装，避免在多个文件里重复导入 pgx。
func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows in result set")
}

// 写请求不能被只读探测的 10/15 秒上限提前截断（2026-11-06）：原来只有 do-upgrade / install-*
// 拿到 120 秒，重启服务、开关协议、管理 AWG 等 POST 实际都被压到 15 秒 —— 比外层 ctx 的预算还短，
// 慢一点的门户直接判超时，而节点上已经执行了。现在 POST 一律至少 portalActionTimeout，重型操作更长。
func portalWebRequestBudget(method, path string) time.Duration {
	if method == http.MethodPost {
		return portalActionBudget(path)
	}
	return portalWebCallTimeout
}

// stripURLError 去掉 *url.Error 里的完整 URL。
//
// 门户网页端点的路径里带着秘密 token（/{token}/check-version），而 *url.Error
// 的 Error() 会把整条 URL 打出来 —— 实测它进了日志，也会随 API 错误回到页面。
// 只保留底层原因（超时、拒绝连接等），排障需要的信息不丢。
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s 请求失败: %w", ue.Op, ue.Err)
	}
	return err
}
