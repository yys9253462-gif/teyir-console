package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 节点出口设置（2026-10-15）。
//
// 节点 portal 早就有「自定义出站」：socks5 / http / direct 出站的增删、
// 「全局出口 / 按域名分流」两种模式、域名分流规则、出口 IP 探测。
// 面板一直没接，只能登进每台节点的门户网页去改。这里把它接进面板：
//
//   GET  /api/nodes/{id}/outbounds         读出站列表、模式、规则、应用状态
//   GET  /api/nodes/{id}/outbounds/probe   探测某个出站的真实出口 IP
//   写操作走 portal-action 的 manage_outbounds 动词（白名单 + 服务端校验 + 审计）。
//
// 安全边界：
//   - 出站密码不回传浏览器，只告诉前端「有没有设密码」。
//   - 探测只接受**已配置出站的名字**，地址由服务端从节点配置里查出来 ——
//     不让浏览器指定任意地址，免得把节点变成一个「替你访问任意代理」的探测器。

type outboundEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Addr        string `json:"addr,omitempty"`
	URL         string `json:"url,omitempty"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"has_password"`
	Insecure    bool   `json:"insecure,omitempty"`
	Mode        string `json:"mode,omitempty"`
}

type outboundRule struct {
	Domain   string `json:"domain"`
	Outbound string `json:"outbound"`
}

type outboundState struct {
	Mode       string          `json:"mode"`
	Applying   bool            `json:"applying"`
	Applied    bool            `json:"applied"`
	ApplyError string          `json:"apply_error"`
	Outbounds  []outboundEntry `json:"outbounds"`
	Rules      []outboundRule  `json:"rules"`
	Builtin    []string        `json:"builtin"`
}

type portalOutboundList struct {
	Mode       string           `json:"mode"`
	Applying   bool             `json:"applying"`
	Applied    bool             `json:"applied"`
	ApplyError string           `json:"apply_error"`
	Outbounds  []map[string]any `json:"outbounds"`
	Rules      []outboundRule   `json:"rules"`
	Builtin    []string         `json:"builtin"`
}

func fetchOutbounds(ctx context.Context, target *portalWebTarget) (*portalOutboundList, error) {
	env, err := portalWebCall(ctx, target, http.MethodGet, "outbounds/list", nil)
	if err != nil {
		// ⚠️ 2026-10-25 五轮排查第三轮：与 capabilities.go 同一判据。
		// 原来这里用裸子串 "404"，端口号 `:40401` 也会命中，
		// 把「节点连不上」误诊成「门户过旧，去升级」。
		if isPortalEndpointMissing(err.Error()) {
			return nil, errors.New("节点门户版本过低，不支持出口设置；请先「批量更新门户」")
		}
		return nil, err
	}
	var list portalOutboundList
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, errors.New("出站列表返回格式无法解析")
	}
	return &list, nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func (s *Server) nodeOutbounds(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	target, err := s.loadPortalWebTarget(ctx, r.PathValue("id"))
	if err != nil {
		s.writePortalWebTargetError(w, err)
		return
	}
	list, err := fetchOutbounds(ctx, target)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": redactPortalError(err.Error(), target)})
		return
	}
	out := outboundState{
		Mode: list.Mode, Applying: list.Applying, Applied: list.Applied, ApplyError: list.ApplyError,
		Rules: list.Rules, Builtin: list.Builtin, Outbounds: make([]outboundEntry, 0, len(list.Outbounds)),
	}
	if out.Rules == nil {
		out.Rules = []outboundRule{}
	}
	for _, o := range list.Outbounds {
		insecure, _ := o["insecure"].(bool)
		out.Outbounds = append(out.Outbounds, outboundEntry{
			Name: str(o, "name"), Type: str(o, "type"), Addr: str(o, "addr"), URL: redactProxyURL(str(o, "url")),
			Username: str(o, "username"), HasPassword: str(o, "password") != "", Insecure: insecure, Mode: str(o, "mode"),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// redactProxyURL 去掉 http 代理 URL 里内嵌的 user:pass。
func redactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// 2026-11-06：解析失败时不能原样返回 —— 密码里一个非法的 %（如 p%ss）就让 url.Parse 失败，
		// 原来整串连同账号密码回给只读用户。把 scheme:// 与最后一个 @ 之间的内容抹掉。
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			if i := strings.Index(raw, "://"); i >= 0 && i+3 <= at {
				return raw[:i+3] + "***" + raw[at:]
			}
			return "***" + raw[at:]
		}
		return raw
	}
	if u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// nodeOutboundProbe 探测出口 IP。name 为空或 "direct" 时探测节点直连出口。
func (s *Server) nodeOutboundProbe(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID := r.PathValue("id")
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	target, err := s.loadPortalWebTarget(ctx, nodeID)
	if err != nil {
		s.writePortalWebTargetError(w, err)
		return
	}
	var rawProxyURL string
	query := url.Values{"kind": {"direct"}}
	if name != "" && name != "direct" {
		if name == "warp" || name == "warp_socks" {
			query.Set("kind", "warp")
		} else {
			list, err := fetchOutbounds(ctx, target)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": redactPortalError(err.Error(), target)})
				return
			}
			found := false
			for _, o := range list.Outbounds {
				if str(o, "name") != name {
					continue
				}
				found = true
				switch str(o, "type") {
				case "socks5":
					query.Set("kind", "socks5")
					query.Set("addr", str(o, "addr"))
				case "http":
					query.Set("kind", "http")
					rawProxyURL = str(o, "url")
					query.Set("addr", rawProxyURL)
				default:
					query.Set("kind", "direct")
				}
			}
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "出站不存在，刷新后重试"})
				return
			}
		}
	}
	env, err := portalWebCall(ctx, target, http.MethodGet, "outbounds/probe", nil, query)
	if err != nil {
		msg := redactPortalError(err.Error(), target)
		if rawProxyURL != "" {
			msg = strings.ReplaceAll(msg, rawProxyURL, redactProxyURL(rawProxyURL))
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
		return
	}
	var res struct {
		IP   string `json:"ip"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点门户返回的出口探测结果无法解析"})
		return
	}
	if res.IP == "" || res.Kind == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点门户未返回完整的出口探测结果"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "kind": res.Kind, "ip": res.IP})
}

var (
	outboundNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,31}$`)
	socksAddrPattern    = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]{1,253}:\d{1,5}$`)
	proxyURLPattern     = regexp.MustCompile(`^https?://[^\s/]+(?:/[^\s]*)?$`)
)

// validateOutboundForm 是 manage-outbounds 的服务端校验（portal 自己也会校验，这里是纵深防御，
// 并且能在发到节点之前就给出清楚的错误）。
func validateOutboundForm(form map[string]string) error {
	switch form["action"] {
	case "save":
		if !outboundNamePattern.MatchString(form["name"]) {
			return errors.New("出站名只能是字母、数字、下划线，不能以数字开头，最长 32 位")
		}
		switch form["type"] {
		case "socks5":
			if !socksAddrPattern.MatchString(form["addr"]) {
				return errors.New("SOCKS5 地址格式应为 host:port，例如 1.2.3.4:1080")
			}
			_, portStr, err := net.SplitHostPort(form["addr"])
			if err != nil {
				return errors.New("SOCKS5 地址格式应为 host:port，例如 1.2.3.4:1080")
			}
			if port, err := strconv.Atoi(portStr); err != nil || port < 1 || port > 65535 {
				return errors.New("SOCKS5 端口需为 1-65535")
			}
		case "http":
			if !proxyURLPattern.MatchString(form["url"]) {
				return errors.New("HTTP 代理地址格式应为 http://host:port 或 https://host:port")
			}
		case "direct":
			switch form["mode"] {
			case "", "auto", "64", "46", "6", "4":
			default:
				return errors.New("direct 模式只能是 auto / 64 / 46 / 6 / 4")
			}
		default:
			return errors.New("出站类型只能是 direct / socks5 / http")
		}
		for _, k := range []string{"username", "password", "addr", "url"} {
			if controlChars.MatchString(form[k]) {
				return errors.New("参数不能包含控制字符或换行")
			}
		}
	case "delete":
		if !outboundNamePattern.MatchString(form["name"]) {
			return errors.New("出站名不合法")
		}
	case "mode":
		if form["mode"] != "rules" && form["mode"] != "global" {
			return errors.New("模式只能是 rules（按域名分流）或 global（全局出口）")
		}
	case "rule-add":
		if !warpDomainPattern.MatchString(strings.TrimPrefix(form["domain"], ".")) {
			return errors.New("域名格式不合法")
		}
		if o := form["outbound"]; o != "" && !outboundNamePattern.MatchString(o) {
			return errors.New("出站名不合法")
		}
	case "rule-del":
		if !warpDomainPattern.MatchString(strings.TrimPrefix(form["domain"], ".")) {
			return errors.New("域名格式不合法")
		}
	default:
		return errors.New("出口设置只支持 save / delete / mode / rule-add / rule-del")
	}
	return nil
}
