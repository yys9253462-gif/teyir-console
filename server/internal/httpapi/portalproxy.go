package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// portalproxy.go drives the node's gost proxy-service registry through the
// portal REST API (Bearer-authenticated, same api_key as everything else under
// /api/v1/).
//
// The returned username/password are the actual working credentials the portal
// wrote into its gost config, so they are shown once in the response and never
// persisted or written to the audit log.

// portalProxyProtocols is the allowlist portal.py enforces. A protocol outside
// this set is rejected by the node with a 400; refusing it here turns a round
// trip into an immediate, clearly-worded error.
var portalProxyProtocols = map[string]bool{
	"socks5": true,
	"http":   true,
	"https":  true,
}

// proxyIDPattern matches the portal's secrets.token_hex(6) identifiers (12 hex
// chars). It is a security boundary: the id is placed in the request body and
// used to select the row to delete.
var proxyIDPattern = regexp.MustCompile(`^[a-f0-9]{6,64}$`)

// portalProxyService is one entry of the portal's proxy_services list. The
// portal deliberately does not mask the password for API-key callers.
type portalProxyService struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Note     string `json:"note"`
	URL      string `json:"url"`
	Format   string `json:"format"`
}

// portalProxyListResponse is the shared list envelope for both GET and POST
// /api/v1/proxy-services/list. gost_installed/gost_active/host were added on
// the node side so the panel can show where to connect and whether gost is
// actually running (older portal builds omit them — treat as unknown).
type portalProxyListResponse struct {
	OK            bool                 `json:"ok"`
	Count         int                  `json:"count"`
	GostInstalled *bool                `json:"gost_installed"`
	GostActive    *bool                `json:"gost_active"`
	Host          string               `json:"host"`
	Services      []portalProxyService `json:"services"`
}

// GET /api/nodes/{id}/proxy-services — read the node's proxy service list.
func (s *Server) portalProxyServices(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}
	// permissive if the node predates this endpoint: portal.py answers 404
	// "API endpoint not found" on older builds, which surfaces as a normal error.
	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodGet, "/proxy-services/list", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed portalProxyListResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的代理服务列表无法解析"})
		return
	}
	if parsed.Services == nil {
		parsed.Services = []portalProxyService{}
	}
	// 2026-11-06：只读账号看不到代理服务的密码与完整连接串。原来原样回给所有登录用户 ——
	// 只读账号拿到的 url / format 就是一条能直接用的 socks5 / http 代理（含账号密码）。
	// 运维能新建代理服务（创建时本来就会拿到密码），照常可见。
	redactProxyServicesForRole(parsed.Services, claims.Role)
	resp := map[string]any{
		"node_id":   target.NodeID,
		"node_name": target.NodeName,
		"count":     len(parsed.Services),
		"services":  parsed.Services,
	}
	// 透传节点的 gost 运行状态与对外主机名；老版 portal 不带这些字段时保持
	// 缺省（false / 空串），前端按"未知"渲染而不是谎报可用。
	if parsed.GostInstalled != nil {
		resp["gost_installed"] = *parsed.GostInstalled
	}
	if parsed.GostActive != nil {
		resp["gost_active"] = *parsed.GostActive
	}
	if parsed.Host != "" {
		resp["host"] = parsed.Host
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/nodes/{id}/proxy-services — create a gost proxy service.
func (s *Server) portalCreateProxyService(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Protocol string `json:"protocol"`
		Port     int    `json:"port"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	protocol := strings.ToLower(strings.TrimSpace(input.Protocol))
	if !portalProxyProtocols[protocol] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "协议只能是 socks5、http 或 https"})
		return
	}
	// Port 0 means "let the portal auto-allocate". Anything else must be a real
	// port; rejecting out-of-range values here gives a clearer message than the
	// portal's own bounds behaviour.
	if input.Port < 0 || input.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需为 0（自动分配）或 1-65535"})
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}

	payload := map[string]any{"type": protocol, "port": input.Port}
	if note := strings.TrimSpace(input.Note); note != "" {
		payload["note"] = note
	}
	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/proxy-services/add", payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed portalProxyService
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的创建结果无法解析"})
		return
	}
	if !parsed.OK || parsed.ID == "" || parsed.Port < 1 || parsed.Port > 65535 || parsed.Username == "" || parsed.Password == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点未确认完整的代理服务创建结果"})
		return
	}
	s.audit(r.Context(), r, "create_node_proxy_service", "hysteria_node", target.NodeID, map[string]any{
		"type": parsed.Type,
		"port": parsed.Port,
		"id":   parsed.ID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"node_id":  target.NodeID,
		"id":       parsed.ID,
		"type":     parsed.Type,
		"host":     parsed.Host,
		"port":     parsed.Port,
		"username": parsed.Username,
		"password": parsed.Password,
		"note":     parsed.Note,
		"url":      parsed.URL,
		"format":   parsed.Format,
	})
}

// DELETE /api/nodes/{id}/proxy-services/{pid} — remove a gost proxy service.
func (s *Server) portalDeleteProxyService(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除代理服务需要 X-Confirm-Dangerous: yes"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	proxyID := strings.TrimSpace(r.PathValue("pid"))
	if !proxyIDPattern.MatchString(proxyID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "代理服务 ID 不合法"})
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}
	if _, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/proxy-services/delete", map[string]any{"id": proxyID}); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), r, "delete_node_proxy_service", "hysteria_node", target.NodeID, map[string]any{"id": proxyID})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": proxyID})
}

// redactProxyServicesForRole 对只读账号清空代理服务的密码与连接串（列表与节点概览共用，2026-11-06）。
func redactProxyServicesForRole(services []portalProxyService, role string) {
	if role != "viewer" {
		return
	}
	for i := range services {
		services[i].Password = ""
		services[i].URL = ""
		services[i].Format = ""
	}
}
