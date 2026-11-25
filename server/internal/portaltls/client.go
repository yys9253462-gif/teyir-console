// Package portaltls 默认验证系统 CA；显式登记的指纹只信任该 origin 的指定证书。
package portaltls

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
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
// 支持大小写不敏感匹配，避免配置了大写域名或带协议前缀的源站键时静默漏配。
func lookupPin(pins map[string]string, u *url.URL) string {
	candidates := make([]string, 0, 3)
	if u.Scheme != "" && u.Host != "" {
		candidates = append(candidates, strings.ToLower(u.Scheme)+"://"+u.Host)
	}
	if u.Host != "" {
		candidates = append(candidates, u.Host)
	}
	if h := u.Hostname(); h != "" && h != u.Host {
		candidates = append(candidates, h)
	}
	for _, key := range candidates {
		if v := strings.TrimSpace(pins[key]); v != "" {
			return v
		}
	}
	for _, want := range candidates {
		for k, v := range pins {
			if strings.EqualFold(strings.TrimRight(strings.TrimSpace(k), "/"), want) {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}
