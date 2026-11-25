package httpapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// GET /api/servers/suggest-name?host=1.2.3.4 —— 添加主机时按地址自动命名。
// 规则与节点一致：`国家-城市`（geo.Info.Name），与已有主机重名时追加序号。
// 主机地址是域名时先解析出 IP 再识别。
func (s *Server) suggestServerName(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" || len(host) > 253 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "host required"})
		return
	}
	ip := host
	if net.ParseIP(host) == nil {
		if !installDomainPattern.MatchString(strings.ToLower(strings.TrimSuffix(host, "."))) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主机地址不是有效的 IP 或域名"})
			return
		}
		lookupCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		addrs, err := net.DefaultResolver.LookupHost(lookupCtx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			writeJSON(w, http.StatusOK, map[string]string{"name": "", "error": "域名解析失败"})
			return
		}
		ip = addrs[0]
	}
	info := s.resolveGeo(r.Context(), ip)
	base := info.Name() // 国家-城市，与节点命名一致（如 美国-哥伦布）
	name := ""
	if base != "" {
		existing := map[string]bool{}
		rows, err := s.db.Query(r.Context(), `SELECT name FROM servers WHERE deleted_at IS NULL`)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				s.databaseError(w, r, err)
				return
			}
			existing[strings.TrimSpace(n)] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			s.databaseError(w, r, err)
			return
		}
		rows.Close()
		name = nextNumberedNodeName(base, existing)
	}

	writeJSON(w, http.StatusOK, map[string]string{"name": name, "region": info.Region, "provider": info.Provider, "ip": ip})
}
