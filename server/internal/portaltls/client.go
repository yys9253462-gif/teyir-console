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
	pin := pins[u.Host]
	if pin == "" {
		pin = pins[u.Hostname()]
	}
	if pin == "" {
		return base, nil
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(pin, ":", ""))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid portal certificate SHA256 pin")
	}
	transport, ok := base.Transport.(*http.Transport)
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
