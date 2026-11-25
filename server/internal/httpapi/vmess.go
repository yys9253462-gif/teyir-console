package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Only public client parameters are retained; UUIDs belong to the subscriber,
// never to the portal administrator's config.
type nodeVMess struct {
	Domain, SNI, WSPath string
	Port                int
}

func (v *nodeVMess) ready() bool {
	return v != nil && v.Domain != "" && v.SNI != "" && strings.HasPrefix(v.WSPath, "/") && v.Port > 0 && v.Port <= 65535
}
func capabilityBool(m map[string]any, key string) bool { v, _ := m[key].(bool); return v }
func capabilityString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}
func capabilityPort(m map[string]any) int {
	v, _ := m["port"].(float64)
	if v != float64(int(v)) {
		return 0
	}
	return int(v)
}
func vmessFromCapability(m map[string]any) *nodeVMess {
	if !capabilityBool(m, "installed") || !capabilityBool(m, "active") {
		return nil
	}
	if configured, present := m["configured"]; present && configured != true {
		return nil
	}
	v := &nodeVMess{Domain: capabilityString(m, "domain"), SNI: capabilityString(m, "sni"), WSPath: capabilityString(m, "ws_path"), Port: capabilityPort(m)}
	if v.SNI == "" {
		v.SNI = v.Domain
	}
	if v.WSPath == "" {
		v.WSPath = "/"
	}
	if !v.ready() {
		return nil
	}
	return v
}

// New portals' capabilities still use the shared Xray process for Reality.
// The Basic endpoint checks the managed inbound. No credentials means unknown,
// not a fabricated running Reality service based on another protocol's process.
func verifyRealityCapability(ctx context.Context, r map[string]any, target *portalWebTarget) {
	if r == nil {
		return
	}
	if configured, present := r["configured"]; present && configured != true {
		r["active"] = false
		r["state_source"] = "configured"
		return
	}
	if inbound, present := r["inbound"]; present {
		r["active"] = capabilityBool(r, "active") && inbound == true
		r["state_source"] = "inbound"
		return
	}
	r["active"] = false
	r["state_source"] = "unknown"
	r["state_error"] = "未绑定 portal 网页凭据，无法确认 Reality 入站状态"
	if target == nil {
		return
	}
	envelope, err := portalWebCall(ctx, target, http.MethodGet, "reality-status", nil)
	if err != nil {
		r["state_error"] = err.Error()
		return
	}
	var status portalRealityInfo
	if json.Unmarshal(envelope.Data, &status) != nil {
		r["state_error"] = "Reality 状态响应无法解析"
		return
	}
	r["active"] = status.Active && status.Installed
	r["state_source"] = "reality-status"
	delete(r, "state_error")
}

func vmessUUIDForUser(userID string) string  { return protocolUUIDForUser("hy2-portal-vmess:", userID) }
func (n subscriptionNode) VMessName() string { return n.Name + "-VMess" }
func vmessURI(n subscriptionNode) string {
	v := n.VMess
	payload := map[string]string{"v": "2", "ps": n.VMessName(), "add": v.Domain, "port": strconv.Itoa(v.Port), "id": vmessUUIDForUser(n.AccountUserID), "aid": "0", "scy": "auto", "net": "ws", "type": "none", "host": v.Domain, "path": v.WSPath, "tls": "tls", "sni": v.SNI, "alpn": "", "fp": "chrome"}
	raw, _ := json.Marshal(payload)
	return "vmess://" + base64.StdEncoding.EncodeToString(raw)
}
func clashVMessProxy(n subscriptionNode) map[string]any {
	v := n.VMess
	return map[string]any{"name": n.VMessName(), "type": "vmess", "server": v.Domain, "port": v.Port, "uuid": vmessUUIDForUser(n.AccountUserID), "alterId": 0, "cipher": "auto", "udp": true, "tls": true, "servername": v.SNI, "client-fingerprint": "chrome", "network": "ws", "ws-opts": map[string]any{"path": v.WSPath, "headers": map[string]any{"Host": v.Domain}}}
}
func singBoxVMessOutbound(n subscriptionNode) map[string]any {
	v := n.VMess
	return map[string]any{"type": "vmess", "tag": n.VMessName(), "server": v.Domain, "server_port": v.Port, "uuid": vmessUUIDForUser(n.AccountUserID), "security": "auto", "alter_id": 0, "tls": map[string]any{"enabled": true, "server_name": v.SNI, "utls": map[string]any{"enabled": true, "fingerprint": "chrome"}}, "transport": map[string]any{"type": "ws", "path": v.WSPath, "headers": map[string]any{"Host": v.Domain}}}
}
