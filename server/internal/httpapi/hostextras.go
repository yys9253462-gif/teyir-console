package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"nodeops/server/internal/cloudflare"
	"nodeops/server/internal/ippurity"
	"nodeops/server/internal/sshclient"
)

// VPS 主机页的实用功能（2026-10-15）：
//   - 连接测试 + 首次连接登记 SSH 指纹（一键安装等写操作要求指纹已登记）
//   - 到期日与月费（到期前 7 天在告警中心提醒，页面汇总月成本）
//   - 换 IP 后一键同步（主机地址、节点域名的 Cloudflare A 记录）
//   - 资源趋势（7 天按小时取均值）

func (s *Server) cloudflareClient() *cloudflare.Client {
	if s == nil || s.purityKeys == nil {
		return &cloudflare.Client{}
	}
	return &cloudflare.Client{Token: s.purityKeys.Get(ippurity.KeyCloudflare)}
}

// GET /api/cloudflare/zones —— 一键安装用：Token 能管理的域名列表。
// 没配 Token 时 configured=false，前端退回「手填完整域名」。
func (s *Server) cloudflareZones(w http.ResponseWriter, r *http.Request) {
	// 2026-11-06：与其它读端点一致走读限流。这一条每条请求都会打外部 Cloudflare API，
	// 循环拉取会耗尽 Token 配额，连带换 IP 时的 DNS 同步失败。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	c := s.cloudflareClient()
	if strings.TrimSpace(c.Token) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "zones": []string{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	zones, err := c.ListZones(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": true, "zones": []string{}, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "zones": zones})
}

// ---------------------------------------------------------------- 连接测试

// POST /api/servers/{id}/test-ssh
//
// 指纹采用「首次信任」：库里还没有指纹时接受对端当前的主机密钥并记下来 ——
// 与采集器、执行器首次连接时的行为一致；之后一律校验，指纹变了就拒绝连接。
func (s *Server) testServerSSH(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	serverID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	var host, user, pin, kind string
	var port int
	var encrypted []byte
	err := s.db.QueryRow(r.Context(), `SELECT s.host,s.ssh_port,s.ssh_user,COALESCE(s.host_fingerprint,''),COALESCE(sc.auth_type,''),sc.ciphertext FROM servers s LEFT JOIN LATERAL (SELECT auth_type,ciphertext FROM server_credentials WHERE server_id=s.id ORDER BY created_at DESC LIMIT 1) sc ON true WHERE s.id=$1::uuid AND s.deleted_at IS NULL`, serverID).Scan(&host, &port, &user, &pin, &kind, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if len(encrypted) == 0 || s.box == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机还没有 SSH 凭据"})
		return
	}
	secret, err := s.box.Open(encrypted)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "SSH 凭据解密失败"})
		return
	}
	defer clear(secret)
	if kind == "" {
		kind = "ssh_key"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	started := time.Now()
	client, fingerprint, err := sshclient.Dial(ctx, host, port, user, sshclient.Credential{Type: kind, Secret: secret}, pin)
	if err != nil {
		if m := fingerprintMismatch(err); m != nil {
			// 指纹变了：多半是重装了系统，也可能是中间人。交给操作者确认，绝不自动接受。
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "fingerprint_mismatch": true, "expected": m.Expected, "got": m.Got,
				"error": "SSH 主机指纹变了：如果你刚重装过系统，这是正常的，确认新指纹即可；否则可能存在中间人攻击，不要确认",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "SSH 连接失败: " + err.Error()})
		return
	}
	defer client.Close()
	latency := time.Since(started).Milliseconds()
	enrolled := false
	if pin == "" && fingerprint != "" {
		tag, err := s.db.Exec(r.Context(), `UPDATE servers SET host_fingerprint=$1 WHERE id=$2::uuid AND host_fingerprint IS NULL`, fingerprint, serverID)
		if err == nil && tag.RowsAffected() > 0 {
			enrolled = true
		}
	}
	switchedFrom := ""
	if want := forcedLoginUser(ctx, client); want != "" {
		if c2, _, derr := sshclient.Dial(ctx, host, port, want, sshclient.Credential{Type: kind, Secret: secret}, fingerprint); derr == nil {
			if s.switchSSHUser(r.Context(), serverID, want) == nil {
				client.Close()
				client, switchedFrom, user = c2, user, want
				defer client.Close()
			} else {
				c2.Close()
			}
		}
	}
	out, _, _, _ := sshclient.Run(ctx, client, `printf '%s|%s|%s|%s' "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")" "$(uname -r)" "$(id -u)" "$(sudo -n true 2>/dev/null && echo yes || echo no)"`)
	parts := strings.SplitN(strings.TrimSpace(sshclient.CleanCommandOutput(out)), "|", 4)
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	// 2026-11-06：审计里带上首次登记的指纹本身（采集器、执行器的首次信任都记了 host_key_first_trust 与指纹）。
	// 原来只记 enrolled=true：事后核对「当时信任的是哪把主机密钥」无从查起。
	details := map[string]any{"enrolled_fingerprint": enrolled}
	if enrolled {
		details["fingerprint"] = fingerprint
		s.audit(r.Context(), r, "host_key_first_trust", "server", serverID, map[string]any{"fingerprint": fingerprint, "source": "test_ssh"})
	}
	s.audit(r.Context(), r, "test_server_ssh", "server", serverID, details)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "latency_ms": latency, "ssh_user": user, "switched_from": switchedFrom, "fingerprint": fingerprint, "enrolled": enrolled,
		"os": parts[0], "kernel": parts[1], "is_root": parts[2] == "0",
		// 非 root 用户能否执行写操作（安装、预设任务）取决于有没有免密 sudo。
		"passwordless_sudo": parts[2] == "0" || parts[3] == "yes",
	})
}

// ---------------------------------------------------------------- 到期与成本

type serverBilling struct {
	ServerID    string   `json:"server_id"`
	ExpiresOn   *string  `json:"expires_on"`
	MonthlyCost *float64 `json:"monthly_cost"`
	Currency    string   `json:"currency"`
	AutoRenew   bool     `json:"auto_renew"`
	Note        string   `json:"note"`
}

// GET /api/servers/billing
func (s *Server) listServerBilling(w http.ResponseWriter, r *http.Request) {
	// 与其它读端点一致走读限流。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT b.server_id::text, to_char(b.expires_on, 'YYYY-MM-DD'), b.monthly_cost::float8, b.currency, b.auto_renew, b.note
FROM server_billing b JOIN servers s ON s.id = b.server_id AND s.deleted_at IS NULL`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	out := make([]serverBilling, 0, 16)
	for rows.Next() {
		var b serverBilling
		if err := rows.Scan(&b.ServerID, &b.ExpiresOn, &b.MonthlyCost, &b.Currency, &b.AutoRenew, &b.Note); err != nil {
			s.databaseError(w, r, err)
			return
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var billingCurrencies = map[string]bool{"USD": true, "CNY": true, "EUR": true, "HKD": true, "JPY": true}

// PUT /api/servers/{id}/billing
func (s *Server) putServerBilling(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in serverBilling
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if in.ExpiresOn != nil && *in.ExpiresOn != "" {
		if _, err := time.Parse("2006-01-02", *in.ExpiresOn); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "到期日格式应为 YYYY-MM-DD"})
			return
		}
	} else {
		in.ExpiresOn = nil
	}
	if in.MonthlyCost != nil && (*in.MonthlyCost < 0 || *in.MonthlyCost > 100000) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "月费需在 0-100000 之间"})
		return
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if !billingCurrencies[in.Currency] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "币种只支持 USD / CNY / EUR / HKD / JPY"})
		return
	}
	in.Note = truncateRunes(strings.TrimSpace(in.Note), 200)
	serverID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `
INSERT INTO server_billing (server_id, expires_on, monthly_cost, currency, auto_renew, note, updated_at)
SELECT s.id, $2::date, $3, $4, $5, $6, now() FROM servers s WHERE s.id = $1::uuid AND s.deleted_at IS NULL
ON CONFLICT (server_id) DO UPDATE SET expires_on = EXCLUDED.expires_on, monthly_cost = EXCLUDED.monthly_cost,
    currency = EXCLUDED.currency, auto_renew = EXCLUDED.auto_renew, note = EXCLUDED.note, updated_at = now()`,
		serverID, in.ExpiresOn, in.MonthlyCost, in.Currency, in.AutoRenew, in.Note)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	s.audit(r.Context(), r, "set_server_billing", "server", serverID, map[string]any{"expires_on": in.ExpiresOn, "auto_renew": in.AutoRenew})
	in.ServerID = serverID
	writeJSON(w, http.StatusOK, in)
}

// expiringServerConditions 给告警巡检用：7 天内到期 → warning，已过期 → critical。
// 自动续费的主机不提醒。
func (s *Server) expiringServerConditions(ctx context.Context) ([]alertCondition, error) {
	rows, err := s.db.Query(ctx, `
SELECT b.server_id::text, (b.expires_on - (now() AT TIME ZONE 'Asia/Shanghai')::date) AS days_left
FROM server_billing b JOIN servers s ON s.id = b.server_id AND s.deleted_at IS NULL
WHERE b.expires_on IS NOT NULL AND NOT b.auto_renew
  AND b.expires_on <= (now() AT TIME ZONE 'Asia/Shanghai')::date + 7`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []alertCondition
	for rows.Next() {
		var id string
		var days int
		if err := rows.Scan(&id, &days); err != nil {
			return nil, err
		}
		if days < 0 {
			out = append(out, alertCondition{alertKey{"server_expired", id, ""}, "critical"})
		} else {
			out = append(out, alertCondition{alertKey{"server_expiring", id, ""}, "warning"})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- 换 IP 同步

// POST /api/servers/{id}/change-ip  {"ip": "1.2.3.4", "update_dns": true}
//
// 云厂商换 IP（或重建实例）之后，面板里要跟着改的地方：主机地址、
// 节点域名的 DNS。节点接入地址用的是域名，DNS 改好后会自动恢复。
// SSH 指纹保留：同一台机器换 IP 主机密钥不变；若是重装系统导致密钥变化，
// 连接会被拒绝，需要在主机页清除指纹后重新测试连接 —— 这是有意的安全行为。
func (s *Server) changeServerIP(w http.ResponseWriter, r *http.Request) {
	// 改主机地址并改写节点域名的 DNS，会影响线上所有客户：与其他危险操作一样要二次确认。
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "更换主机 IP 需要 X-Confirm-Dangerous: yes"})
		return
	}
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		IP        string `json:"ip"`
		UpdateDNS bool   `json:"update_dns"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	in.IP = strings.TrimSpace(in.IP)
	if ip := net.ParseIP(in.IP); ip == nil || !ippurity.IsPublicIP(in.IP) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请填写新的公网 IP"})
		return
	}
	if in.UpdateDNS && net.ParseIP(in.IP).To4() == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "自动DNS同步当前仅支持IPv4；IPv6请取消DNS同步并手工配置AAAA记录"})
		return
	}
	serverID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	if busy, err := s.serverHasActiveHostJob(r.Context(), serverID); err != nil {
		s.databaseError(w, r, err)
		return
	} else if busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这台主机有安装/迁移任务在进行（或待核验结案），完成后再换 IP"})
		return
	}
	// 与安装/迁移和 executor 使用同一主机行锁。持有至 DNS 写入完成，避免
	// “检查没有租约”之后被另一个请求插入租约，再以旧 IP 启动远端操作。
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	var oldIP string
	if err := tx.QueryRow(r.Context(), `SELECT host FROM servers WHERE id=$1::uuid AND deleted_at IS NULL FOR UPDATE`, serverID).Scan(&oldIP); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		} else {
			s.databaseError(w, r, err)
		}
		return
	}
	if oldIP == in.IP {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新 IP 与当前地址相同"})
		return
	}
	// 锁内用新快照再查持久化租约，且拒绝正在执行的常规主机任务。
	var busy bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM host_operation_leases WHERE server_id=$1::uuid) OR EXISTS (SELECT 1 FROM operation_tasks WHERE server_id=$1::uuid AND status='running')`, serverID).Scan(&busy); err != nil {
		s.databaseError(w, r, err)
		return
	} else if busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这台主机有安装/迁移任务在进行（或待核验结案），完成后再换 IP"})
		return
	}

	type nd struct{ name, domain string }
	var nodes []nd
	if in.UpdateDNS {
		// 先把要改的节点读全，再动主机地址：读失败时什么都没改，可以放心重试。
		rows, err := tx.Query(r.Context(), `SELECT name, COALESCE(domain,'') FROM hysteria_nodes WHERE server_id=$1::uuid AND deleted_at IS NULL`, serverID)
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		for rows.Next() {
			var n nd
			if err := rows.Scan(&n.name, &n.domain); err != nil {
				rows.Close()
				s.databaseError(w, r, err)
				return
			}
			nodes = append(nodes, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			s.databaseError(w, r, err)
			return
		}
		for _, n := range nodes {
			if n.domain == "" || net.ParseIP(n.domain) != nil {
				continue
			}
			var taken bool
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM hysteria_nodes WHERE deleted_at IS NULL AND lower(rtrim(domain,'.'))=lower(rtrim($1,'.')) AND server_id<>$2::uuid)`, n.domain, serverID).Scan(&taken); err != nil {
				s.databaseError(w, r, err)
				return
			}
			if taken {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "域名 " + n.domain + " 仍被其他主机上的在用节点使用，不能改写 DNS"})
				return
			}
		}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE servers SET host=$2, updated_at=now() WHERE id=$1::uuid`, serverID, in.IP); err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-03：从这里起，DNS 写入与提交不再跟随客户端连接。原来都用 r.Context()：
	// 客户端中途断开 ⇒ 已写的 A 记录指向新 IP，而提交因 context canceled 失败、库里仍是旧 IP，
	// 也没有审计 —— 面板管旧地址、客户被解析到新地址。一旦开始改 DNS 就必须把这件事做完。
	// 2026-11-06：DNS 与提交共用一个总预算。原来每台节点各给 30 秒、commit 完全没有超时：
	// 主机行锁（上面 SELECT ... FOR UPDATE）会一直持到提交为止，Cloudflare 卡住或 TLS 黑洞时
	// 5 台节点最坏持锁 150s，节点更多则线性放大 —— 期间安装/迁移、第二次换 IP 全被这把行锁挡住；
	// 库侧 hang 时 Commit 还能把 goroutine 永久挂住。
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancelCommit()

	type dnsResult struct {
		Node    string `json:"node"`
		Domain  string `json:"domain"`
		Result  string `json:"result"`
		Failed  bool   `json:"failed,omitempty"`
		Warning string `json:"warning,omitempty"`
	}
	results := []dnsResult{}
	dnsOK := true
	if len(nodes) > 0 {
		cf := s.cloudflareClient()
		for _, n := range nodes {
			if n.domain == "" || net.ParseIP(n.domain) != nil {
				results = append(results, dnsResult{Node: n.name, Domain: n.domain, Result: "节点没有域名，跳过"})
				continue
			}
			ctx, cancel := context.WithTimeout(commitCtx, 30*time.Second)
			res, err := cf.UpsertARecords(ctx, n.domain, in.IP)
			cancel()
			item := dnsResult{Node: n.name, Domain: n.domain, Warning: res.Warning()}
			if err != nil {
				item.Result, item.Failed, dnsOK = "失败："+err.Error(), true, false
			} else {
				item.Result = map[string]string{"created": "已新建 A 记录", "updated": "已更新 A 记录", "unchanged": "记录本来就正确"}[res.Action]
				if res.Deleted > 0 {
					item.Result += fmt.Sprintf("（删除了 %d 条指向旧地址的多余 A 记录）", res.Deleted)
				}
			}
			results = append(results, item)
		}
	}
	if err := tx.Commit(commitCtx); err != nil {
		// DNS 可能已经改了：把逐条结果写进日志，便于人工回滚。
		s.logger.Error("change ip: commit failed after DNS writes", "server_id", serverID, "old_ip", oldIP, "new_ip", in.IP, "dns", results, "error", err)
		s.databaseError(w, r, err)
		return
	}
	// 更换主机地址后立即作废健康闸门与订阅构建缓存，避免无域名节点或恢复中的节点继续下发旧地址。
	invalidateHealthGateCache()
	InvalidateSubscriptionCache()
	// IP 纯净度与原生判定都跟 IP 绑定，换了 IP 就作废 —— 页面会显示「已变更，建议重测」。
	s.audit(commitCtx, r, "change_server_ip", "server", serverID, map[string]any{"old_ip": oldIP, "new_ip": in.IP, "dns": results, "dns_ok": dnsOK})
	status := http.StatusOK
	if !dnsOK {
		status = http.StatusMultiStatus
	}
	writeJSON(w, status, map[string]any{"old_ip": oldIP, "new_ip": in.IP, "dns": results, "dns_ok": dnsOK})
}

// serverHasActiveHostJob 判断主机是否被安装/迁移任务占用：内存里的任务表，
// 或持久化的 host_operation_leases（跨进程、重启后仍有效）。
func (s *Server) serverHasActiveHostJob(ctx context.Context, serverID string) (bool, error) {
	if norm, ok := canonicalUUID(serverID); ok {
		serverID = norm
	}
	installJobs.mu.Lock()
	_, running := installJobs.byServer[serverID]
	installJobs.mu.Unlock()
	if running {
		return true, nil
	}
	var leased bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_operation_leases WHERE server_id=$1::uuid)`, serverID).Scan(&leased)
	return leased, err
}

// ---------------------------------------------------------------- 资源趋势

// GET /api/servers/{id}/metrics/trend?days=7
// 按小时取均值，7 天最多 168 个点，前端画图不必再降采样。
func (s *Server) serverMetricsTrend(w http.ResponseWriter, r *http.Request) {
	// 与其它读端点一致走读限流。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	serverID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在"})
		return
	}
	days := 7
	if d := r.URL.Query().Get("days"); d == "1" || d == "3" || d == "7" || d == "30" {
		fmt.Sscanf(d, "%d", &days)
	}
	rows, err := s.db.Query(r.Context(), `
SELECT date_trunc('hour', sampled_at) AS h,
       AVG(cpu_percent)::float8,
       AVG(CASE WHEN memory_total_bytes > 0 THEN memory_used_bytes * 100.0 / memory_total_bytes END)::float8,
       AVG(CASE WHEN disk_total_bytes > 0 THEN disk_used_bytes * 100.0 / disk_total_bytes END)::float8
FROM server_metrics
WHERE server_id = $1::uuid AND sampled_at > now() - make_interval(days => $2)
GROUP BY h ORDER BY h`, serverID, days)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	type point struct {
		T    time.Time `json:"t"`
		CPU  *float64  `json:"cpu"`
		Mem  *float64  `json:"mem"`
		Disk *float64  `json:"disk"`
	}
	out := make([]point, 0, 168)
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.T, &p.CPU, &p.Mem, &p.Disk); err != nil {
			s.databaseError(w, r, err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "points": out})
}
