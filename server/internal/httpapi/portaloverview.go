package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// portaloverview.go exposes a single read-only endpoint that summarises what the
// panel can learn about one node through its portal API key.
//
// IMPORTANT — the reachable surface is much narrower than it first looks.
// portal.py's `do_GET` only serves three subpaths under /api/v1/:
//
//	node/meta              → the node's own client_meta.json
//	users/list             → dynamic users (passwords deliberately omitted)
//	proxy-services/list    → gost proxy services
//
// Everything else answers 404 `API endpoint not found`. In particular
// check-version / reality-status / bbr-status / warp-status / traffic-speed are
// NOT under /api/v1/ — they live behind the portal's *web session* prefix
// (/{token}/…), authenticated by the admin session or Basic auth, and are
// therefore unreachable with the Bearer key alone. An earlier revision of this
// file probed those paths anyway; every probe 404ed and the UI rendered a row of
// "unavailable" cards. They have been removed rather than papered over.
//
// So this endpoint reports exactly three things — the node's own description, its
// users, and its gost services — and derives the aggregate counters in Go. That
// is the honest extent of what the stored key unlocks.

// portalOverviewTimeout bounds the whole fan-out. Three overlapping 10s round
// trips do not need three times the per-call budget.
const portalOverviewTimeout = 15 * time.Second

// portalMeta is the subset of client_meta.json the panel cares about.
//
// Field names are load-bearing: the installer writes this file with exactly these
// keys, and portal.py's artifacts() reads them the same way.
type portalMeta struct {
	PublicIP     string `json:"public_ip"`
	ServerName   string `json:"server_name"`
	ListenPort   int    `json:"listen_port"`
	SubsPort     int    `json:"subscription_port"`
	HopPortRange string `json:"hop_port_range"`
	CertType     string `json:"cert_type"`
	IsInsecure   bool   `json:"is_insecure"`
	AuthPassword string `json:"auth_password"`
	ObfsPassword string `json:"obfs_password"`
	PinSHA256    string `json:"pin_sha256"`
}

// portalUserEntry is one row of users/list. Passwords are never returned by that
// endpoint, which is precisely why the panel cannot build a subscription out of
// it and must fall back to the node's own auth_password.
type portalUserEntry struct {
	UserID     string `json:"user_id"`
	ExpiresAt  int64  `json:"expires_at"`
	Active     bool   `json:"active"`
	LimitBytes int64  `json:"traffic_limit_bytes"`
	UsedBytes  int64  `json:"traffic_used_bytes"`
	IPLimit    int    `json:"ip_limit"`
	CreatedAt  int64  `json:"created_at"`
	Status     string `json:"status"`
}

// portalMetaPublic is the browser-facing view of the node's meta.
//
// It deliberately omits auth_password and obfs_password. auth_password is the
// node's *master* credential — the same secret the subscription builder uses to
// authenticate clients — so handing it to the browser would defeat the rule that
// the portal key and everything it unlocks stay server-side. Only a boolean is
// exposed, which is enough for the UI to say "obfs enabled".
type portalMetaPublic struct {
	PublicIP     string `json:"public_ip"`
	ServerName   string `json:"server_name"`
	ListenPort   int    `json:"listen_port"`
	SubsPort     int    `json:"subscription_port"`
	HopPortRange string `json:"hop_port_range"`
	CertType     string `json:"cert_type"`
	IsInsecure   bool   `json:"is_insecure"`
	HasObfs      bool   `json:"has_obfs"`
	HasPin       bool   `json:"has_pin"`
}

func (m portalMeta) redacted() portalMetaPublic {
	return portalMetaPublic{
		PublicIP:     m.PublicIP,
		ServerName:   m.ServerName,
		ListenPort:   m.ListenPort,
		SubsPort:     m.SubsPort,
		HopPortRange: m.HopPortRange,
		CertType:     m.CertType,
		IsInsecure:   m.IsInsecure,
		HasObfs:      m.ObfsPassword != "",
		HasPin:       m.PinSHA256 != "",
	}
}

// GET /api/nodes/{id}/overview — node summary built from the reachable endpoints.
func (s *Server) portalOverview(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), portalOverviewTimeout)
	defer cancel()

	// Three independent reads, fetched concurrently.
	var (
		wg          sync.WaitGroup
		meta        portalMeta
		metaErr     error
		users       []portalUserEntry
		usersErr    error
		services    []portalProxyService
		servicesErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		envelope, err := portalCall(ctx, target.BaseURL, target.APIKey, http.MethodGet, "/node/meta", nil)
		if err != nil {
			metaErr = err
			return
		}
		var parsed struct {
			Meta portalMeta `json:"meta"`
		}
		if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
			metaErr = err
			return
		}
		meta = parsed.Meta
	}()
	go func() {
		defer wg.Done()
		envelope, err := portalCall(ctx, target.BaseURL, target.APIKey, http.MethodGet, "/users/list", nil)
		if err != nil {
			usersErr = err
			return
		}
		var parsed struct {
			Users []portalUserEntry `json:"users"`
		}
		if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
			usersErr = err
			return
		}
		users = parsed.Users
	}()
	go func() {
		defer wg.Done()
		envelope, err := portalCall(ctx, target.BaseURL, target.APIKey, http.MethodGet, "/proxy-services/list", nil)
		if err != nil {
			servicesErr = err
			return
		}
		var parsed struct {
			Services []portalProxyService `json:"services"`
		}
		if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
			servicesErr = err
			return
		}
		services = parsed.Services
	}()
	wg.Wait()
	// 2026-11-06：概览里的代理服务同样按角色脱敏。原来原样返回：/proxy-services 列表已对只读账号清空密码，
	// 这里却把带账号密码的 socks5 / http 连接串整串回给只读账号，等于绕过了那次修复。
	redactProxyServicesForRole(services, claims.Role)

	// Aggregate the user counters in Go rather than in the browser, so the UI has
	// one authoritative set of numbers to show.
	var activeUsers, unlimited int
	var usedBytes, limitBytes int64
	for _, user := range users {
		if user.Active {
			activeUsers++
		}
		usedBytes += user.UsedBytes
		if user.LimitBytes > 0 {
			limitBytes += user.LimitBytes
		} else {
			unlimited++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":   target.NodeID,
		"node_name": target.NodeName,
		"meta":      meta.redacted(),
		"summary": map[string]any{
			"users_count":          len(users),
			"users_active":         activeUsers,
			"users_unlimited":      unlimited,
			"traffic_used_bytes":   usedBytes,
			"traffic_limit_bytes":  limitBytes,
			"proxy_services_count": len(services),
			"has_obfs":             meta.ObfsPassword != "",
			"is_insecure":          meta.IsInsecure,
			"hop_port_range":       meta.HopPortRange,
			"subscription_port":    meta.SubsPort,
			"cert_type":            meta.CertType,
		},
		"users":          orEmptyUsers(users),
		"proxy_services": orEmptyServices(services),
		// Each read reports whether it succeeded, so "the node said no" is never
		// rendered the same way as "there is nothing there".
		"probes": map[string]any{
			"meta":           probeState(metaErr),
			"users":          probeState(usersErr),
			"proxy_services": probeState(servicesErr),
		},
		// Stated explicitly so the UI can explain the gap instead of leaving the
		// operator wondering why version / Reality / BBR / WARP are absent.
		"unreachable": "版本、Reality、BBR、WARP 状态在 portal 上走网页会话鉴权（/{token}/…），Bearer 密钥够不到，本面板不读取",
	})
}

func probeState(err error) map[string]any {
	if err == nil {
		return map[string]any{"available": true}
	}
	return map[string]any{"available": false, "error": err.Error()}
}

func orEmptyUsers(value []portalUserEntry) []portalUserEntry {
	if value == nil {
		return []portalUserEntry{}
	}
	return value
}

func orEmptyServices(value []portalProxyService) []portalProxyService {
	if value == nil {
		return []portalProxyService{}
	}
	return value
}
