// Package portaltls 默认验证系统 CA；显式登记的指纹只信任该 origin 的指定证书。
package portaltls

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func Client(base *http.Client, endpoint string) (*http.Client, error) {
	if base == nil {
		return nil, errors.New("portal pin requires HTTP client")
	}
	raw := strings.TrimSpace(os.Getenv("PORTAL_TLS_PINS"))
	if raw == "" {
		return base, nil
	}
	var pins map[string]string
	if json.Unmarshal([]byte(raw), &pins) != nil {
		return nil, errors.New("PORTAL_TLS_PINS must be a JSON host/origin to SHA256 map")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	pin := lookupPin(pins, u)
	if pin == "" {
		return base, nil
	}
	normalizedPin := strings.TrimSpace(pin)
	// 允许常见的 sha256: 前缀写法，统一归一为裸 64 位十六进制串后再严格校验长度。
	if len(normalizedPin) >= 7 && strings.EqualFold(normalizedPin[:7], "sha256:") {
		normalizedPin = strings.TrimSpace(normalizedPin[7:])
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(normalizedPin, ":", ""))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid portal certificate SHA256 pin")
	}
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	transport, ok := rt.(*http.Transport)
	if !ok {
		return nil, errors.New("portal pin requires HTTP transport")
	}
	copyClient := *base
	copyTransport := transport.Clone()
	copyTransport.DisableKeepAlives = true
	// 显式 pin 替代 CA/名称匹配；始终验证指纹和有效期，不存在无校验的分支。
	copyTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("portal TLS certificate missing")
		}
		cert := state.PeerCertificates[0]
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return errors.New("portal pinned certificate expired or not valid yet")
		}
		digest := sha256.Sum256(cert.Raw)
		if subtle.ConstantTimeCompare(digest[:], decoded) != 1 {
			return errors.New("portal TLS certificate pin mismatch")
		}
		return nil
	}}
	copyClient.Transport = copyTransport
	return &copyClient, nil
}

// lookupPin 按源站（scheme://host）、主机含端口（host）、裸主机名（hostname）顺序查找指纹，
// 支持大小写不敏感匹配以及默认端口（https:443 / http:80）等价归一，避免省略或显式写默认端口时漏配指纹。
func lookupPin(pins map[string]string, u *url.URL) string {
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	host := strings.ToLower(strings.TrimSpace(u.Host))
	hostname := strings.ToLower(strings.TrimSpace(u.Hostname()))
	port := strings.TrimSpace(u.Port())

	defaultPort := ""
	switch scheme {
	case "https":
		defaultPort = "443"
	case "http":
		defaultPort = "80"
	}

	var hostNoDefault, hostWithDefault string
	if hostname != "" {
		if port == "" || port == defaultPort {
			// IPv6 主机名在源站或带端口表达中需补齐方括号。
			hostNoDefault = formatHostOnly(hostname)
			if defaultPort != "" {
				hostWithDefault = net.JoinHostPort(hostname, defaultPort)
			}
		} else {
			hostWithDefault = host
		}
	}

	candidates := make([]string, 0, 6)
	addCandidate := func(c string) {
		c = strings.TrimRight(strings.ToLower(strings.TrimSpace(c)), "/")
		if c == "" {
			return
		}
		for _, exist := range candidates {
			if exist == c {
				return
			}
		}
		candidates = append(candidates, c)
	}
	if scheme != "" && host != "" {
		addCandidate(scheme + "://" + host)
	}
	if scheme != "" && hostNoDefault != "" {
		addCandidate(scheme + "://" + hostNoDefault)
	}
	if scheme != "" && hostWithDefault != "" {
		addCandidate(scheme + "://" + hostWithDefault)
	}
	addCandidate(host)
	addCandidate(hostWithDefault)
	addCandidate(hostNoDefault)
	addCandidate(hostname)

	// 先做归一化键表构建并按字典序稳定处理等价键，消除 Go 映射随机迭代顺序带来的不确定性。
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	normalized := make(map[string]string, len(pins))
	for _, k := range keys {
		v := strings.TrimSpace(pins[k])
		if v == "" {
			continue
		}
		nk := strings.TrimRight(strings.ToLower(strings.TrimSpace(k)), "/")
		if nk == "" {
			continue
		}
		if _, exists := normalized[nk]; !exists {
			normalized[nk] = v
		}
	}
	for _, want := range candidates {
		if v := normalized[want]; v != "" {
			return v
		}
	}
	return ""
}

func formatHostOnly(hostname string) string {
	if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
		return "[" + hostname + "]"
	}
	return hostname
}
