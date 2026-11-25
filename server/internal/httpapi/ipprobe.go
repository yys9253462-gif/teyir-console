package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"nodeops/server/internal/ippurity"
)

// ipprobe.go — 检测任意 IP / 域名的纯净度（2026-10-17）。
//
// 纯净度（连接类型、风险分、代理 / VPN / 滥用标记、原生 IP）对任何公网 IP 都能查。
// 流媒体与 AI 解锁不同：它必须**从这个 IP 出口真实访问** Netflix / ChatGPT 才有意义，
// 面板替别人的 IP 测不了。所以只有当输入命中本面板管理的节点时，才让该节点现场检测；
// 其它 IP 明确告诉用户「无法检测」，绝不给出猜测的结果。
//
// 结果不落库（与节点列表的纯净度记录互不影响）；命中节点时的解锁结果会照常更新到该节点。

type ipProbeNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ipProbeResult struct {
	Query       string            `json:"query"`
	IP          string            `json:"ip"`
	ResolvedIPs []string          `json:"resolved_ips,omitempty"`
	Location    string            `json:"location,omitempty"`
	Purity      *ippurity.Verdict `json:"purity,omitempty"`
	PurityError string            `json:"purity_error,omitempty"`
	RegCountry  string            `json:"reg_country,omitempty"`
	GeoCountry  string            `json:"geo_country,omitempty"`
	Native      *bool             `json:"native"`
	Node        *ipProbeNode      `json:"node"`
	// NodeLookupError 非空 = 没能确定是否为本面板节点（数据库问题），与「不是你的节点」不是一回事。
	NodeLookupError string         `json:"node_lookup_error,omitempty"`
	Unlock          []unlockResult `json:"unlock,omitempty"`
	UnlockError     string         `json:"unlock_error,omitempty"`
	UnlockNote      string         `json:"unlock_note,omitempty"`
}

// POST /api/ip-probe  {"query": "1.2.3.4" | "hy2.example.com"}
func (s *Server) probeIP(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	query := normalizeProbeQuery(in.Query)
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请输入 IP 或域名"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	res := ipProbeResult{Query: query}
	if ip := net.ParseIP(query); ip != nil {
		res.IP = ip.String()
	} else {
		if !installDomainPattern.MatchString(query) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不是有效的 IP 或域名"})
			return
		}
		addrs, err := lookupIPWithPublicFallback(ctx, query)
		if err != nil || len(addrs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "域名解析失败：" + query})
			return
		}
		for _, a := range addrs {
			res.ResolvedIPs = append(res.ResolvedIPs, a.IP.String())
		}
		// 优先 IPv4：纯净度数据源对 IPv4 覆盖最好。
		res.IP = res.ResolvedIPs[0]
		for _, a := range addrs {
			if a.IP.To4() != nil {
				res.IP = a.IP.String()
				break
			}
		}
	}
	if !ippurity.IsPublicIP(res.IP) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "只能检测公网 IP（" + res.IP + " 是内网 / 保留地址）"})
		return
	}

	node, nodeErr := s.nodeForProbe(ctx, res.IP, query)
	if nodeErr != nil {
		s.logger.Warn("ip probe: 判断是否为本面板节点失败", "error", nodeErr)
		res.NodeLookupError = "无法确认这个 IP 是否为本面板的节点（数据库暂时不可用），解锁检测已跳过"
	} else {
		res.Node = node
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // 纯净度（强制走 API 拿评分）
		defer wg.Done()
		if s.purity == nil {
			res.PurityError = "IP 纯净度检测未启用（请在系统设置 → 集成密钥里配置）"
			return
		}
		callCtx, c := context.WithTimeout(ctx, purityCheckTimeout)
		defer c()
		v, err := s.purity.Lookup(callCtx, res.IP, true)
		if err != nil {
			res.PurityError = err.Error()
			return
		}
		res.Purity = &v
	}()
	go func() { // 原生 IP：注册地（RDAP）与实际使用地（geo）一致即原生
		defer wg.Done()
		callCtx, c := context.WithTimeout(ctx, 20*time.Second)
		defer c()
		reg, _, _ := ippurity.RegistrationCountry(callCtx, res.IP)
		res.RegCountry = strings.ToUpper(strings.TrimSpace(reg))
		if s.geo != nil {
			if info, err := s.geo.Lookup(callCtx, res.IP); err == nil {
				res.GeoCountry = strings.ToUpper(strings.TrimSpace(info.Country))
				res.Location = info.Name()
			}
		}
		if res.RegCountry != "" && res.GeoCountry != "" {
			native := res.RegCountry == res.GeoCountry
			res.Native = &native
		}
	}()
	go func() { // 解锁：只有本面板的节点能从自己的出口现场检测
		defer wg.Done()
		if res.Node == nil {
			// 查库失败时 res.Node 同样是 nil，但结论完全不同（见 NodeLookupError）：不能说成「不是你的节点」。
			if res.NodeLookupError == "" {
				res.UnlockNote = "流媒体与 AI 解锁必须从这个 IP 出口实际访问才能测出，面板只能检测自己管理的节点；这个 IP 不是你的节点。"
			}
			return
		}
		results, err := s.checkAndStoreUnlock(ctx, res.Node.ID)
		if err != nil {
			res.UnlockError = err.Error()
			return
		}
		res.Unlock = results
	}()
	wg.Wait()

	s.audit(r.Context(), r, "probe_ip", "ip", "", map[string]any{"query": query, "ip": res.IP, "node": res.Node != nil})
	writeJSON(w, http.StatusOK, res)
}

// normalizeProbeQuery 接受 IP、域名，或顺手粘进来的整段地址（https://host:port/path）。
func normalizeProbeQuery(raw string) string {
	q := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(q, "://"); i >= 0 {
		q = q[i+3:]
	}
	if i := strings.IndexAny(q, "/?#"); i >= 0 {
		q = q[:i]
	}
	if i := strings.LastIndex(q, "@"); i >= 0 {
		q = q[i+1:]
	}
	if strings.HasPrefix(q, "[") { // [IPv6]:port
		if end := strings.Index(q, "]"); end > 0 {
			return q[1:end]
		}
	}
	if host, _, err := net.SplitHostPort(q); err == nil {
		q = host
	}
	q = strings.TrimSuffix(q, ".")
	if len(q) > 253 {
		return ""
	}
	return q
}

// nodeForProbe 找输入对应的本面板节点：按节点域名、已检测到的出口 IP、所在主机地址匹配。
// 返回值：节点（没匹配到为 nil）与错误。2026-11-06：查库失败不能再当成「这个 IP 不是你的节点」——
// 那会告诉用户一个错的结论，解锁检测也被跳过。
func (s *Server) nodeForProbe(ctx context.Context, ip, query string) (*ipProbeNode, error) {
	if s.db == nil {
		return nil, nil
	}
	var n ipProbeNode
	err := s.db.QueryRow(ctx, `
SELECT n.id::text, n.name
FROM hysteria_nodes n
LEFT JOIN servers sv ON sv.id = n.server_id
LEFT JOIN node_ip_purity p ON p.node_id = n.id
WHERE n.deleted_at IS NULL
  AND (lower(COALESCE(n.domain,'')) = $2 OR p.ip = $1 OR sv.host = $1 OR sv.host = $2)
ORDER BY (lower(COALESCE(n.domain,'')) = $2) DESC
LIMIT 1`, ip, query).Scan(&n.ID, &n.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // 确实不是本面板的节点
		}
		return nil, err
	}
	return &n, nil
}
