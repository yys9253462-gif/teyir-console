package ippurity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 原生 IP 判定（2026-10-15）。
//
// 「原生 IP」= 这段 IP 的**注册国家**与它的**实际所在国家**一致。
// 不一致的叫「广播 IP」（例如注册在新加坡、被拿到韩国用）—— 流媒体与 AI
// 服务常按注册地判地区，广播 IP 容易被识别成别的国家或当成代理。
//
// 注册国家取自 RDAP（WHOIS 的结构化继任者），查的是**这段 IP 自己**的
// 登记信息，而不是 ASN 的注册国 —— 阿里云、AWS 这种一个 ASN 横跨几十个
// 国家的，ASN 国家几乎没有参考价值。
//
// 各 RIR 的 RDAP 写法不同（实测）：
//   - APNIC / RIPE / AFRINIC / LACNIC：网段对象直接带 "country"。
//   - ARIN：网段对象没有 country，只能取 registrant 实体 vCard 地址的
//     最后一行（国家英文名，如 "United States" / "Sweden" / "Japan"）。

// rdapBootstrap 由它按 IP 重定向到对应 RIR。
const rdapBootstrap = "https://rdap.org/ip/"

// rdapHosts 是允许跟随的重定向目标 —— 只认五个 RIR 的 RDAP 服务。
var rdapHosts = map[string]bool{
	"rdap.arin.net":           true,
	"rdap.apnic.net":          true,
	"rdap.db.ripe.net":        true,
	"rdap.afrinic.net":        true,
	"rdap.lacnic.net":         true,
	"rdap.registro.br":        true,
	"rdap.org":                true,
	"www.rdap.net":            true,
	"rdap-bootstrap.arin.net": true,
}

var rdapClient = func() *http.Client {
	c := newSafeClient()
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("RDAP 重定向次数过多")
		}
		if req.URL.Scheme != "https" || !rdapHosts[strings.ToLower(req.URL.Hostname())] {
			return fmt.Errorf("RDAP 重定向到了非 RIR 主机：%s", req.URL.Host)
		}
		return nil
	}
	return c
}()

type rdapEntity struct {
	Roles      []string        `json:"roles"`
	VCardArray json.RawMessage `json:"vcardArray"`
	Entities   []rdapEntity    `json:"entities"`
}

type rdapNetwork struct {
	Country  string       `json:"country"`
	Name     string       `json:"name"`
	Port43   string       `json:"port43"`
	Entities []rdapEntity `json:"entities"`
}

// RegistrationCountry 查询 IP 段的注册国家（ISO 两位大写）。
// 返回的 registry 是 RIR 名（如 "whois.apnic.net"），排障用。
func RegistrationCountry(ctx context.Context, ip string) (country, registry string, err error) {
	ip = strings.TrimSpace(ip)
	if !IsPublicIP(ip) {
		return "", "", errors.New("不是可查询的公网 IP")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rdapBootstrap+url.PathEscape(ip), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	resp, err := rdapClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("RDAP 查询失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("RDAP 返回 %d", resp.StatusCode)
	}
	var network rdapNetwork
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&network); err != nil {
		return "", "", fmt.Errorf("解析 RDAP 响应失败: %w", err)
	}
	country, ok := countryFromRDAP(network)
	if !ok {
		return "", network.Port43, errors.New("RDAP 未给出注册国家")
	}
	return country, network.Port43, nil
}

func countryFromRDAP(n rdapNetwork) (string, bool) {
	if c := strings.ToUpper(strings.TrimSpace(n.Country)); isASCIIAlpha2(c) {
		return c, true
	}
	// ARIN：取 registrant 实体的地址国家。只认 registrant —— abuse / noc
	// 联系人常是总部（如 AWS 一律是西雅图），用它会把所有海外段都判成 US。
	for _, e := range n.Entities {
		if !hasRole(e.Roles, "registrant") {
			continue
		}
		if c, ok := vcardCountry(e.VCardArray); ok {
			return c, true
		}
	}
	return "", false
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

// vcardCountry 从 jCard（RFC 7095）的 adr 属性取国家：
// ["adr", {"label": "...\nUnited States"}, "text", ["", "", street, city, region, zip, country]]
// 先看结构化的 country 分量，再退回 label 的最后一行。
func vcardCountry(raw json.RawMessage) (string, bool) {
	var card []json.RawMessage
	if json.Unmarshal(raw, &card) != nil || len(card) < 2 {
		return "", false
	}
	var props [][]json.RawMessage
	if json.Unmarshal(card[1], &props) != nil {
		return "", false
	}
	for _, prop := range props {
		if len(prop) < 4 {
			continue
		}
		var name string
		if json.Unmarshal(prop[0], &name) != nil || name != "adr" {
			continue
		}
		var parts []string
		if json.Unmarshal(prop[3], &parts) == nil && len(parts) >= 7 {
			if c, ok := countryCode(parts[6]); ok {
				return c, true
			}
		}
		var params struct {
			Label string `json:"label"`
			CC    string `json:"cc"`
		}
		if json.Unmarshal(prop[1], &params) == nil {
			if c, ok := countryCode(params.CC); ok {
				return c, true
			}
			lines := strings.Split(strings.TrimSpace(params.Label), "\n")
			if c, ok := countryCode(lines[len(lines)-1]); ok {
				return c, true
			}
		}
	}
	return "", false
}

// countryCode 把 ISO 码或英文国家名归一成 ISO 两位大写。
// 只收常见 VPS 所在国；认不出的返回 false（→ 未判定），绝不猜。
func countryCode(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if isASCIIAlpha2(s) {
		return strings.ToUpper(s), true
	}
	c, ok := countryNameToCode[strings.ToLower(s)]
	return c, ok
}

// isASCIIAlpha2 判断字符串是否恰好为两位英文字母，避免将 "--" 或数字串误判为国家码。
func isASCIIAlpha2(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

var countryNameToCode = map[string]string{
	"united states": "US", "united states of america": "US", "usa": "US",
	"canada": "CA", "mexico": "MX", "brazil": "BR", "argentina": "AR", "chile": "CL",
	"united kingdom": "GB", "great britain": "GB", "ireland": "IE", "france": "FR",
	"germany": "DE", "netherlands": "NL", "the netherlands": "NL", "belgium": "BE",
	"luxembourg": "LU", "switzerland": "CH", "austria": "AT", "italy": "IT", "spain": "ES",
	"portugal": "PT", "sweden": "SE", "norway": "NO", "denmark": "DK", "finland": "FI",
	"poland": "PL", "czech republic": "CZ", "czechia": "CZ", "romania": "RO", "bulgaria": "BG",
	"hungary": "HU", "greece": "GR", "turkey": "TR", "türkiye": "TR", "russia": "RU",
	"russian federation": "RU", "ukraine": "UA", "israel": "IL", "united arab emirates": "AE",
	"saudi arabia": "SA", "bahrain": "BH", "qatar": "QA", "south africa": "ZA", "egypt": "EG",
	"india": "IN", "japan": "JP", "korea": "KR", "south korea": "KR", "republic of korea": "KR",
	"korea, republic of": "KR", "china": "CN", "hong kong": "HK", "taiwan": "TW",
	"singapore": "SG", "malaysia": "MY", "thailand": "TH", "vietnam": "VN", "viet nam": "VN",
	"philippines": "PH", "indonesia": "ID", "australia": "AU", "new zealand": "NZ",
}
