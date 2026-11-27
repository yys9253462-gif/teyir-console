package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"nodeops/server/internal/sshclient"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
)

// serviceControl.go holds the read side of the systemd service control surface.
//
// The write side (start/stop/restart/reload/enable/disable) goes through the task
// queue so that every mutation is audited and rate-limited the same way as every
// other operation. Status, however, is read-only and cheap: pushing it through
// the executor would make the UI wait a full polling cycle (20s by default) just
// to render a badge. So it is answered synchronously here, over the same
// authenticated SSH path the collector uses.

// serviceStatusTimeout bounds one status round trip. It matches the collector's
// per-host budget so a hanging host cannot pin an HTTP connection longer than
// the collector would tolerate.
const serviceStatusTimeout = 25 * time.Second

// serviceUnitStatus is one unit's observed state on a host.
type serviceUnitStatus struct {
	Unit    string `json:"unit"`
	Active  string `json:"active"`  // active / inactive / failed / activating / unknown
	Enabled string `json:"enabled"` // enabled / disabled / static / masked / unknown
	// Since is the timestamp systemd reports for the last state change.
	Since string `json:"since"`
	// Error carries a per-unit note so one unreadable unit does not fail the
	// whole response.
	Error string `json:"error,omitempty"`
}

// readServiceStates reads ActiveState/UnitFileState/StateChangeTimestamp for
// every unit in a single SSH round trip. Batching matters: a host with five
// whitelisted units would otherwise need fifteen separate commands.
//
// The output is deliberately simple key=value text parsed in Go rather than
// column-formatted remote output — the same reasoning behind the collector
// using `df -P -B1`: locale and shell differences on the target make column
// parsing fragile, and an empty or translated header silently shifts fields.
func readServiceStates(ctx context.Context, client *ssh.Client, units []string) ([]serviceUnitStatus, error) {
	if len(units) == 0 {
		return []serviceUnitStatus{}, nil
	}

	var script strings.Builder
	for i, unit := range units {
		// Guard against a unit name smuggling shell syntax. Callers must have
		// validated it already, but this is the last point before execution, and
		// the whitelist is admin-editable data.
		if !safeUnitName.MatchString(unit) {
			continue
		}
		quoted := shellQuote(unit)
		script.WriteString("printf 'UNIT=%s\\n' " + quoted + "\n")
		// `systemctl show --value` prints one property per line in the order the
		// properties were requested, so awk can label them positionally. A unit
		// that does not exist yields nothing, which is why each record is closed
		// by an explicit separator rather than relying on line counts.
		script.WriteString("systemctl show " + quoted + " -p ActiveState -p UnitFileState -p StateChangeTimestamp --value 2>/dev/null | " +
			"awk 'NR==1{print \"ACTIVE=\"$0} NR==2{print \"ENABLED=\"$0} NR==3{print \"SINCE=\"$0}'\n")
		script.WriteString("printf 'SEP=" + strconv.Itoa(i) + "\\n'\n")
	}

	stdout, stderr, _, err := sshclient.Run(ctx, client, script.String())
	if err != nil {
		return nil, err
	}
	return parseServiceStates(stdout, stderr, units), nil
}

// parseServiceStates turns the key=value output of readServiceStates' script into
// rows. It is split out from the SSH call so the parsing rules — which carry the
// subtle cases (missing units, truncated output, units with no properties) — can
// be unit tested without a live host.
func parseServiceStates(stdout, stderr string, units []string) []serviceUnitStatus {
	results := make([]serviceUnitStatus, 0, len(units))
	current := serviceUnitStatus{}
	flush := func() {
		if current.Unit != "" {
			// systemctl may emit the unit name but no properties at all (unit not
			// found, or all three properties suppressed). Normalise here rather
			// than at render time so every row leaving this function has a
			// renderable state instead of an empty string.
			current.Active = normalizeState(current.Active)
			current.Enabled = normalizeState(current.Enabled)
			results = append(results, current)
		}
		current = serviceUnitStatus{}
	}

	for _, line := range strings.Split(sshclient.CleanCommandOutput(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "UNIT":
			current.Unit = strings.TrimSpace(value)
		case "ACTIVE":
			current.Active = normalizeState(value)
		case "ENABLED":
			current.Enabled = normalizeState(value)
		case "SINCE":
			current.Since = strings.TrimSpace(value)
		case "SEP":
			flush()
		}
	}
	// Flush whatever is pending. This covers both the normal case (the loop ends
	// right after a separator, so nothing is pending) and truncated output where
	// the final separator never arrived.
	flush()

	// A unit that produced no output at all still deserves a row, so the UI can
	// distinguish "systemd knows nothing about this unit" from "unit was never
	// requested".
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		seen[r.Unit] = true
	}
	for _, unit := range units {
		if !seen[unit] {
			results = append(results, serviceUnitStatus{
				Unit:    unit,
				Active:  "unknown",
				Enabled: "unknown",
				Error:   describeUnitReadFailure(stderr),
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Unit < results[j].Unit })
	return results
}

// describeUnitReadFailure turns raw stderr into something an operator can act on,
// without leaking the whole remote output into the API response.
func describeUnitReadFailure(stderr string) string {
	message := strings.TrimSpace(sshclient.CleanCommandOutput(stderr))
	if message == "" {
		return "systemd 未返回该 unit 的信息（可能不存在或名称拼写有误）"
	}
	// 按字符数截断：systemd 的报错里常有中文 unit 描述，按字节切会留下半个字，
	// 前端拿到的是乱码而不是可读的错误。
	if capped := truncateRunes(message, 200); capped != message {
		message = capped + "…"
	}
	return message
}

// normalizeState maps the empty string (systemd reporting nothing) to "unknown"
// so the UI never renders a blank badge.
func normalizeState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

// shellQuote wraps a value in single quotes for safe interpolation into a POSIX
// shell command, escaping any embedded single quote.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// serviceStatus answers GET /api/servers/{id}/services.
//
// Units come from the host's write whitelist: that list already expresses "which
// services on this host is this panel allowed to talk about", so reusing it keeps
// a single source of truth instead of introducing a second registry to drift.
func (s *Server) serviceStatus(w http.ResponseWriter, r *http.Request) {
	// 2026-11-03：每次请求都会建一条 SSH 会话，原来不限流（viewer 也能反复刷）。
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	id, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在或已删除"})
		return
	}

	var (
		host, user  string
		port        int
		fingerprint string
		authType    string
		secret      []byte
		writeUnits  []string
	)
	err := s.db.QueryRow(r.Context(), `
SELECT s.host, s.ssh_port, s.ssh_user, COALESCE(s.host_fingerprint, ''),
       COALESCE(s.write_units, '{}'),
       COALESCE(sc.auth_type, ''), sc.ciphertext
FROM servers s
LEFT JOIN LATERAL (
    SELECT auth_type, ciphertext FROM server_credentials x
    WHERE x.server_id = s.id ORDER BY x.created_at DESC LIMIT 1
) sc ON true
WHERE s.id = $1::uuid AND s.deleted_at IS NULL`, id).
		Scan(&host, &port, &user, &fingerprint, &writeUnits, &authType, &secret)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "主机不存在或已删除"})
			return
		}
		s.databaseError(w, r, err)
		return
	}

	if len(writeUnits) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"units": []serviceUnitStatus{},
			"note":  "该主机尚未设置服务白名单，请先在写操作设置中添加要管理的 unit",
		})
		return
	}
	if len(secret) == 0 || s.box == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"units": []serviceUnitStatus{},
			"note":  "该主机未配置 SSH 凭据，无法读取服务状态",
		})
		return
	}

	// 2026-11-03：还没登记主机指纹时不连。指纹为空时 Dial 会按首次信任接受对端密钥，
	// 并把已存的凭据（可能是明文口令）发过去 —— 一个只读的「看服务状态」不该替主机做
	// 首次信任；与 hostSSH / dialServer 一致，先在主机页做一次连接测试登记指纹。
	if fingerprint == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"units": []serviceUnitStatus{},
			"note":  "该主机尚未登记 SSH 主机指纹，请先在主机页完成一次连接测试",
		})
		return
	}

	plain, err := s.box.Open(secret)
	if err != nil {
		s.logger.Warn("service status: decrypt credential failed", "server_id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "凭据解密失败"})
		return
	}
	defer clear(plain)
	if authType == "" {
		authType = "ssh_key"
	}
	cred := sshclient.Credential{Type: authType, Secret: plain}

	ctx, cancel := context.WithTimeout(r.Context(), serviceStatusTimeout)
	defer cancel()

	client, _, err := sshclient.Dial(ctx, host, port, user, cred, fingerprint)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "SSH 连接失败: " + err.Error()})
		return
	}
	defer client.Close()

	units, err := readServiceStates(ctx, client, writeUnits)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "读取服务状态失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"units": units})
}
