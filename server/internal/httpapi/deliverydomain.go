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
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// 交付域名：发给买家的订阅 / 交付页链接用哪个域名（2026-10-17）。
//
// 优先级：控制台设置（system_settings）> 环境变量 SUBSCRIPTION_PUBLIC_BASE_URL > 请求 Host。
// 值缓存在内存里，生成链接时不查库；保存时同步更新缓存。
const deliveryBaseKey = "subscription_public_base_url"

var deliveryBaseCache atomic.Value // string

// 订阅显示名称：客户端导入后订阅分组 / 配置文件叫什么（v2rayN 默认是 import_sub）。
// v2rayN 读链接上的 remarks 参数；Clash Verge / Stash / Hiddify 读 profile-title 头与文件名。
const (
	subscriptionNameKey     = "subscription_display_name"
	defaultSubscriptionName = "Teyir"
)

var subscriptionNameCache atomic.Value // string

func subscriptionDisplayName() string {
	if v, _ := subscriptionNameCache.Load().(string); v != "" {
		return v
	}
	if env := strings.TrimSpace(os.Getenv("SUBSCRIPTION_DEFAULT_NAME")); env != "" && validSubscriptionName(env) {
		return env
	}
	return defaultSubscriptionName
}

// subLink 生成发给买家的订阅链接：带上 format（可选）与 remarks（显示名称）。
func subLink(base, token, format string) string {
	q := url.Values{}
	if format != "" {
		q.Set("format", format)
	}
	q.Set("remarks", subscriptionDisplayName())
	// 空格编码成 %20 而不是 +：v2rayN 的参数解析不一定把 + 还原成空格。
	return base + "/sub/" + token + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

func validSubscriptionName(v string) bool {
	n := len([]rune(v))
	if n < 1 || n > 32 {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`"\/<>`, r) {
			return false
		}
	}
	return true
}

func deliveryBaseOverride() string {
	if v, _ := deliveryBaseCache.Load().(string); v != "" {
		return v
	}
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("SUBSCRIPTION_PUBLIC_BASE_URL")), "/")
	if strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
		return v
	}
	return ""
}

// backupDeliveryBases 备用交付域名（2026-10-27）：主域名在国内被墙时，交付页额外给出
// 几条换了域名的订阅链接。只换域名、token 不变 —— 指向同一个订阅，停用 / 删除用户时
// 所有备用链接一起失效，不会漏收。环境变量 SUBSCRIPTION_BACKUP_BASE_URLS，逗号分隔。
// 与主域名重复的、格式不对的都会被丢掉。
func backupDeliveryBases(primary string) []string {
	out := make([]string, 0)
	// ⚠️ 2026-10-30：主域名也要过一遍 normalizeDeliveryBase 再进 seen。
	//
	// 原来只做 strings.ToLower，于是 primary 带尾斜杠（`https://sub.example.com/`）
	// 或没写协议（`sub.example.com`）时，同一个域名在后台被填进
	// SUBSCRIPTION_BACKUP_BASE_URLS 会**去重失败** —— 交付页上主线路与
	// 「备用线路1」是同一个地址，客户看不出区别，只以为备用线路也坏了。
	normalizedPrimary, err := normalizeDeliveryBase(primary)
	if err != nil || normalizedPrimary == "" {
		normalizedPrimary = strings.ToLower(strings.TrimRight(strings.TrimSpace(primary), "/"))
	}
	seen := map[string]bool{normalizedPrimary: true}
	for _, raw := range strings.Split(os.Getenv("SUBSCRIPTION_BACKUP_BASE_URLS"), ",") {
		base, err := normalizeDeliveryBase(raw)
		if err != nil || base == "" || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, base)
	}
	return out
}

func (s *Server) loadDeliveryBase(ctx context.Context) {
	if s.db == nil {
		return
	}
	var v string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, deliveryBaseKey).Scan(&v); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Warn("delivery domain: load public base failed; retaining existing cache", "error", err)
		return
	}
	var name string
	if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, subscriptionNameKey).Scan(&name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Warn("delivery domain: load subscription name failed; retaining existing cache", "error", err)
		return
	}
	deliveryBaseCache.Store(strings.TrimSpace(v))
	subscriptionNameCache.Store(strings.TrimSpace(name))
}

// normalizeDeliveryBase 接受 "sub.example.com" 或 "https://sub.example.com"，只允许 https + 纯域名（可带端口）。
func normalizeDeliveryBase(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", errors.New("域名格式不对")
	}
	if u.Scheme != "https" {
		return "", errors.New("只支持 https")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("只填域名，不要带路径或参数")
	}
	host := u.Hostname()
	// 2026-11-12：主机名里不能有 %。url.Parse 会把主机里的 %25 解码成字面 %，重新拼成 URL 后就不再是合法转义，
	// 后面构造请求会失败（见 checkDeliveryBase）；这样的「域名」也发不出能打开的链接。
	if !strings.Contains(host, ".") || strings.ContainsAny(host, " _%") {
		return "", errors.New("域名格式不对")
	}
	// 裸内网/回环/保留地址不得作为发给买家的交付域名。
	if ip := net.ParseIP(host); ip != nil && !portaltls.PublicIP(ip) {
		return "", errors.New("交付域名不能指向内网或保留地址")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

var errDeliveryNonPublicAddress = errors.New("域名解析到了内网或保留地址，不能作为买家交付域名")

// deliveryCheckClient 复用连接池与安全拨号器校验交付域名，避免每次探测新建传输层导致连接泄漏，
// 同时阻止通过自定义域名探测内网或云元数据地址。
func newDeliveryCheckClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := lookupIPWithPublicFallback(ctx, host)
				if err != nil {
					return nil, err
				}
				if len(ips) == 0 {
					return nil, errors.New("no such host")
				}
				for _, ip := range ips {
					if !portaltls.PublicIP(ip.IP) {
						return nil, errDeliveryNonPublicAddress
					}
				}
				return portaltls.DialCheckedIPs(ctx, dialer, network, port, ips)
			},
		},
	}
}

var deliveryCheckClient = newDeliveryCheckClient()

// allowPlainHTTPLoopbackForTest 仅由 *_test.go 的 init 开启，用于兼容本地明文回环桩服务单测；
// 生产二进制不会编译测试文件，该开关在生产环境恒为 false。
var allowPlainHTTPLoopbackForTest bool

// deliveryTestHTTPClient 仅在单元测试使用本地明文回环桩服务时复用，生产环境永远不会启用。
var deliveryTestHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     15 * time.Second,
	},
}

// checkDeliveryBase 确认这个域名真的指到了本面板：请求 /api/health 要拿到面板的健康响应。
// 防止填错后发给买家一批打不开的链接。
func checkDeliveryBase(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 2026-11-12：不能丢掉构造请求的错误。原来 `req, _ :=`：地址拼不成合法 URL 时 req 为 nil，
	// 下面 client.Do(nil) 直接空指针 panic，管理员看到的是连接被重置而不是「域名格式不对」。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/health", nil)
	if err != nil {
		return errors.New("域名格式不对")
	}
	client := deliveryCheckClient
	if allowPlainHTTPLoopbackForTest && req.URL.Scheme == "http" {
		client = deliveryTestHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errDeliveryNonPublicAddress) {
			return errDeliveryNonPublicAddress
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, "no such host"):
			return errors.New("域名还没解析，请先把 A 记录指向面板服务器")
		case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls"):
			return errors.New("HTTPS 证书不可用：请在反向代理（Caddy）里加上这个域名")
		}
		return fmt.Errorf("访问不到这个域名：%s", shortErr(msg))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("这个域名没有指到本面板（/api/health 返回 HTTP %d）；请在反向代理里把它转发到面板", resp.StatusCode)
	}
	// ⚠️ 2026-10-30：必须按 JSON 字段判断，不能只搜子串 "ok"。
	//
	// 原来用 `strings.Contains(body, "ok")`：任何含 "ok" 的页面都能通过 ——
	// "book"、"cookie"、"look"、甚至一张停靠页/错误页都可能命中。
	// 而这一步是**发给买家的链接**的最后一道校验：放过去就意味着
	// 一批打不开的订阅地址。现在要求响应体解析成功且 status 恰好为 "ok"。
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Status != "ok" {
		return fmt.Errorf("这个域名没有指到本面板（/api/health 的响应不是面板健康格式，HTTP %d）；请在反向代理里把它转发到面板", resp.StatusCode)
	}
	return nil
}

func shortErr(msg string) string {
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}

// GET /api/settings/delivery-domain
func (s *Server) getDeliveryDomain(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		claims, _ := claimsFrom(r.Context())
		if !s.enforceReadLimit(w, claims.Subject) {
			return
		}
	}
	saved, _ := deliveryBaseCache.Load().(string)
	env := strings.TrimSpace(os.Getenv("SUBSCRIPTION_PUBLIC_BASE_URL"))
	effective := deliveryBaseOverride()
	source := "setting"
	switch {
	case saved != "":
	case effective != "":
		source = "env"
	default:
		source = "request"
		effective = subscriptionBaseURL(r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": saved, "env": env, "effective": effective, "source": source, "name": subscriptionDisplayName()})
}

// PUT /api/settings/delivery-domain  {"value": "sub.example.com", "name": "Teyir"}
// value 空串 = 清除（回退到环境变量或面板域名）；value 为 "-" 且带 name = 只改显示名称。
func (s *Server) setDeliveryDomain(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Value string  `json:"value"`
		Name  *string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 先完整校验名称与交付域名，全部通过后再落库与刷缓存，避免返回 400 时已部分修改名称。
	updateName := in.Name != nil
	var trimmedName string
	if updateName {
		trimmedName = strings.TrimSpace(*in.Name)
		if trimmedName != "" && !validSubscriptionName(trimmedName) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "显示名称 1-32 个字，不能含引号、斜杠、尖括号"})
			return
		}
	}
	updateDomain := !(updateName && strings.TrimSpace(in.Value) == "-")
	var base string
	if updateDomain {
		var err error
		base, err = normalizeDeliveryBase(in.Value)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if base != "" {
			if err := checkDeliveryBase(r.Context(), base); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
	}
	if updateName {
		// 先落库再更新缓存：写失败时绝不刷新内存缓存，防止重启后回退。
		if err := s.putSetting(r.Context(), subscriptionNameKey, trimmedName, "客户端导入后的订阅分组名称（留空则使用默认名称）"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存订阅名称失败：" + err.Error()})
			return
		}
		subscriptionNameCache.Store(trimmedName)
		s.audit(r.Context(), r, "set_subscription_name", "system_setting", subscriptionNameKey, map[string]any{"value": trimmedName})
		if !updateDomain {
			s.getDeliveryDomain(w, r)
			return
		}
	}
	// 同上：写失败必须让操作者知道，否则"改了域名"只在内存里生效到下次重启。
	if err := s.putSetting(r.Context(), deliveryBaseKey, base, "发给买家的订阅/交付链接域名（空 = 用环境变量或面板域名）"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存交付域名失败：" + err.Error()})
		return
	}
	deliveryBaseCache.Store(base)
	s.audit(r.Context(), r, "set_delivery_domain", "system_setting", deliveryBaseKey, map[string]any{"value": base})
	s.getDeliveryDomain(w, r)
}
