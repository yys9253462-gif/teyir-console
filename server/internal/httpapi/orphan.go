package httpapi

// orphan.go — 待补删的孤儿节点账号。
//
// 背景见迁移 023 与 manage.go 的 recordOrphanAccounts：
// 删除节点时若某些买家账号没能从节点侧注销，凭据会随删除一并抹掉，
// 于是"重试"在物理上不可能。那里我们把「uid + 地址 + 加密后的 key」
// 一起登记到 orphan_node_accounts；这里提供把它取出来、真的补删的出口。
//
// 三条接口：
//   GET  /api/orphans          列出待补删的账号（默认只列未解决的）
//   POST /api/orphans/{id}/retry   用留存的凭据重试删除
//   POST /api/orphans/{id}/resolve 不补删、直接结案（机器已停用时用）
//
// 安全姿态与其它节点侧操作一致：管理员 + 写限流 + 不可逆操作要确认头。
// 特别注意：**接口永不回显 api_key**，只回"有没有留存凭据"。

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// retryOrphanTimeout 单次补删的超时。与 revokeNodeFromAllSubscriptions 里
// 调 /users/delete 时用的 10s 保持一致 —— 同一个节点端点，不该有两套预算。
const retryOrphanTimeout = 10 * time.Second

// orphanAccount 是待补删账号在 API 上的形态。
type orphanAccount struct {
	ID       string `json:"id"`
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	// NodeAPIURL 是删除那一刻记下的 portal 地址。
	// 对运维的意义：手动去机器上清理时，知道该看哪个门户。
	NodeAPIURL     string  `json:"node_api_url"`
	UserID         string  `json:"user_id"`
	SubscriptionID *string `json:"subscription_id"`
	// HasCredential 表示当时留到了凭据快照。
	// **没有它这条基本无解** —— 前端要据此区分"可一键补删"与"只能人工上机"。
	HasCredential bool       `json:"has_credential"`
	Reason        string     `json:"reason"`
	Attempts      int        `json:"attempts"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	ResolvedAt    *time.Time `json:"resolved_at"`
	ResolvedNote  string     `json:"resolved_note"`
	CreatedAt     time.Time  `json:"created_at"`
	// NodeDeleted：节点已从控制台删除（或节点行已不存在）。2026-11-08：只有这种节点才显示
	// 「确认机器已下线，整台结案」按钮，在役节点的账号还应该补删。
	NodeDeleted bool `json:"node_deleted"`
}

// GET /api/orphans?include_resolved=1
func (s *Server) listOrphans(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	includeResolved := r.URL.Query().Get("include_resolved") == "1"

	// api_key_cipher 只取"是否为空"（`IS NOT NULL`），**绝不 SELECT 出来**。
	// 它虽然加了密，但没有任何理由让它走出数据库。
	query := `
SELECT o.id, o.node_id, o.node_name, o.node_api_url, o.user_id, o.subscription_id,
       (api_key_cipher IS NOT NULL), o.reason, o.attempts, o.last_attempt_at,
       o.resolved_at, o.resolved_note, o.created_at,
       COALESCE((SELECT n.deleted_at IS NOT NULL FROM hysteria_nodes n WHERE n.id = o.node_id), TRUE)
FROM orphan_node_accounts o`
	if !includeResolved {
		query += ` WHERE o.resolved_at IS NULL`
	}
	query += ` ORDER BY o.created_at DESC LIMIT 500`

	rows, err := s.db.Query(r.Context(), query)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()

	out := make([]orphanAccount, 0)
	pending := 0
	for rows.Next() {
		var a orphanAccount
		if err := rows.Scan(&a.ID, &a.NodeID, &a.NodeName, &a.NodeAPIURL,
			&a.UserID, &a.SubscriptionID, &a.HasCredential, &a.Reason,
			&a.Attempts, &a.LastAttemptAt, &a.ResolvedAt, &a.ResolvedNote,
			&a.CreatedAt, &a.NodeDeleted); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if a.ResolvedAt == nil {
			pending++
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orphans":        out,
		"count":          len(out),
		"pending":        pending,
		"note":           "这些是删除节点时未能从节点侧注销的买家账号。留了凭据的可直接重试；没留的只能到机器上人工清理。",
		"how_it_happens": "节点不可达 / 凭据被拒 / 超时。失败时映射会保留，但节点凭据随删除一并抹掉，所以必须在这里留档才能补删。",
	})
}

// POST /api/orphans/{id}/retry — 用留存的凭据补删一个账号。
//
// 语义（照着铁律⑮来）：
//   - 节点返回「没有这个用户」**算成功**。这里和回收循环不同：
//     回收时要区分"这台没有"与"已删掉"（所以要遍历全部节点）；
//     而这里的目标是**确定的那一台节点上的那一个 uid**，
//     "它本来就不在"与"它被删掉了"对结果是等价的 —— 账号确实不存在了。
//   - 真的删到手才标 resolved。
//   - 失败要把原因写回行里（下次还能看），并让 HTTP 返回非 2xx。
func (s *Server) retryOrphan(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	// 删除节点用户是不可逆操作，与 deleteNode 保持同一约定。
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "补删节点账号需要 X-Confirm-Dangerous: yes",
		})
		return
	}
	id := r.PathValue("id")

	var (
		nodeID, nodeName, apiURL, userID string
		cipher                           []byte
	)
	err := s.db.QueryRow(r.Context(), `
SELECT node_id, node_name, node_api_url, user_id, COALESCE(api_key_cipher, ''::bytea)
FROM orphan_node_accounts WHERE id=$1::uuid AND resolved_at IS NULL`, id).
		Scan(&nodeID, &nodeName, &apiURL, &userID, &cipher)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.databaseError(w, r, err)
			return
		}
		// 已结案与"没有这条"要分开说：前者是运维已经人工处理过，
		// 后者是 id 填错；混在一起会让人对不存在的记录反复点重试。
		var resolvedAt *time.Time
		checkErr := s.db.QueryRow(r.Context(), `SELECT resolved_at FROM orphan_node_accounts WHERE id=$1::uuid`, id).Scan(&resolvedAt)
		if checkErr == nil && resolvedAt != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "这条记录已结案，无需重试"})
			return
		}
		if checkErr != nil && !errors.Is(checkErr, pgx.ErrNoRows) {
			s.databaseError(w, r, checkErr)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该待补删记录不存在"})
		return
	}

	// 无论成败都记一次尝试：attempts 是运维判断"是不是反复失败"的依据。
	markAttempt := func(reason string) {
		if _, err := s.db.Exec(r.Context(), `
UPDATE orphan_node_accounts SET attempts = attempts + 1, last_attempt_at = now(), reason = $2
WHERE id=$1::uuid`, id, reason); err != nil {
			s.logger.Warn("retryOrphan: 记录尝试失败", "id", id, "error", err)
		}
	}
	markResolved := func(note string) error {
		// 节点删号已确认完成后使用脱离客户端取消的独立短超时回写数据库，避免状态丢失。
		persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancelPersist()
		_, err := s.db.Exec(persistCtx, `
UPDATE orphan_node_accounts SET resolved_at = now(), resolved_note = $2,
       attempts = attempts + 1, last_attempt_at = now()
WHERE id=$1::uuid`, id, note)
		return err
	}

	if len(cipher) == 0 {
		markAttempt("没有留存凭据，无法自动补删")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "这条记录没有留存节点凭据，无法自动补删；请到 " + nodeName + " (" + apiURL + ") 上手工删除用户 " + userID,
		})
		return
	}
	if s.box == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "服务端未配置凭据加密密钥"})
		return
	}
	plain, err := s.box.Open(cipher)
	if err != nil {
		markAttempt("凭据解密失败: " + err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "凭据解密失败，无法补删"})
		return
	}

	// 2026-11-12：补删前核对这个账号是不是正被在役节点上的订阅使用。
	//
	// 节点账号名只由订阅决定（sub_<订阅id>），与节点无关。节点被删除、账号回收失败登记为待补删之后，
	// 同一台机器可能被重新接入（门户没重装，地址与密钥不变），补开户会在新节点上接管这个同名账号。
	// 此时再按这条旧记录留存的地址去删，删掉的就是买家正在用的账号：面板里的映射还在，补开户不认为缺账号，
	// 买家在这台节点上认证失败而面板显示一切正常。
	inUseNode, checkUseErr := s.orphanAccountInUse(r.Context(), apiURL, string(plain), userID)
	if checkUseErr != nil {
		s.databaseError(w, r, checkUseErr)
		return
	}
	if inUseNode != "" {
		markAttempt("账号正被在役节点 " + inUseNode + " 上的订阅使用，未补删")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "这个账号正被在役节点「" + inUseNode + "」上的订阅使用（同一台机器已重新接入），补删会让买家断线；这条记录请直接点「已解决」结案",
		})
		return
	}

	callCtx, cancel := context.WithTimeout(r.Context(), retryOrphanTimeout)
	defer cancel()
	_, callErr := portalCall(callCtx, apiURL, string(plain), http.MethodPost, "/users/delete", map[string]any{
		"user_id": userID,
	})
	if callErr != nil {
		// 「没有这个用户」= 账号确实不存在了 → 目标达成，标 resolved。
		// 这是本接口与回收循环最大的语义差别，理由见函数注释。
		if isUserNotFound(callErr) {
			if err := markResolved("节点返回用户不存在（账号已不在节点上）"); err != nil {
				// 2026-11-03：原来只记日志、照样回 resolved，列表刷新后这条又回来。与下面成功分支一致：如实说。
				s.logger.Error("retryOrphan: 标记已解决失败", "id", id, "error", err)
				writeJSON(w, http.StatusOK, map[string]any{
					"status": "already_gone", "user_id": userID,
					"warning": "节点上已无此账号，但状态写回失败，列表可能仍显示这条；再点一次即可自愈",
				})
				return
			}
			s.audit(r.Context(), r, "retry_orphan", "orphan_account", id,
				map[string]any{"node_id": nodeID, "user_id": userID, "result": "already_gone"})
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "resolved", "user_id": userID,
				"detail": "节点上已无此账号，视为已清理",
			})
			return
		}
		markAttempt(callErr.Error())
		s.logger.Warn("retryOrphan: 补删失败", "id", id, "node", nodeName, "user_id", userID, "error", callErr)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "补删失败：" + callErr.Error(),
		})
		return
	}

	if err := markResolved("补删成功"); err != nil {
		// 删成功了但没记上 —— 下次还会出现在列表里。这不算数据损坏
		// （再点一次会得到"用户不存在"→ 自动标 resolved），但要让调用方知道。
		s.logger.Error("retryOrphan: 补删成功但标记失败", "id", id, "error", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "deleted", "user_id": userID,
			"warning": "账号已从节点删除，但状态写回失败，列表可能仍显示这条；再点一次即可自愈",
		})
		return
	}
	s.audit(r.Context(), r, "retry_orphan", "orphan_account", id,
		map[string]any{"node_id": nodeID, "user_id": userID, "result": "deleted"})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "user_id": userID})
}

// orphanAccountInUse 报告待补删记录指向的账号是否正被「同一台机器重新接入后的在役节点」上的订阅使用；
// 是则返回那台节点的名字，否则返回空串。
//
// 「同一台机器」按两条判据任一成立：节点 API 地址相同（忽略大小写与末尾斜杠），或节点 API 密钥相同
// （同一个门户的密钥不变，重新接入时换了地址写法也能认出来）。地址条件不能省：所有节点上的账号名都是 sub_<订阅id>，
// 只按账号名判断会把别的节点上的同名账号也算进来，让补删永远做不了。
func (s *Server) orphanAccountInUse(ctx context.Context, apiURL, apiKey, userID string) (string, error) {
	rows, err := s.db.Query(ctx, `
SELECT DISTINCT n.name, COALESCE(n.api_url,''), COALESCE(n.api_key, ''::bytea)
FROM hysteria_nodes n
JOIN subscription_tokens st ON st.node_accounts ? n.id::text
WHERE n.deleted_at IS NULL AND st.deleted_at IS NULL
  AND st.node_accounts->n.id::text->>'user_id' = $1`, userID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	sameURL := func(a, b string) bool {
		return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
	}
	for rows.Next() {
		var name, liveURL string
		var cipher []byte
		if err := rows.Scan(&name, &liveURL, &cipher); err != nil {
			return "", err
		}
		if liveURL != "" && sameURL(liveURL, apiURL) {
			return name, nil
		}
		if len(cipher) > 0 && s.box != nil {
			if plain, openErr := s.box.Open(cipher); openErr == nil && subtle.ConstantTimeCompare(plain, []byte(apiKey)) == 1 {
				return name, nil
			}
		}
	}
	return "", rows.Err()
}

// resolveOrphan 不补删、直接把一条待补删记录结案。
//
// 用于机器已永久停用的情况：节点连不上也永远不会再连上，"重试"没有意义，
// 但记录一直挂在待办里会淹没真正需要处理的条目。
// 只改状态不碰节点，所以不需要凭据；但结案意味着"放弃清理这个账号"，
// 因此与 retry 一样要求管理员 + 确认头，并写审计。
func (s *Server) resolveOrphan(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "结案待补删账号需要 X-Confirm-Dangerous: yes",
		})
		return
	}
	id := r.PathValue("id")
	var nodeID, userID string
	err := s.db.QueryRow(r.Context(), `
UPDATE orphan_node_accounts SET resolved_at = now(), resolved_note = '人工标记已解决（节点已停用，不再补删）'
WHERE id=$1::uuid AND resolved_at IS NULL
RETURNING node_id, user_id`, id).Scan(&nodeID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该待补删记录不存在或已解决"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "resolve_orphan", "orphan_account", id,
		map[string]any{"node_id": nodeID, "user_id": userID})
	writeJSON(w, http.StatusOK, map[string]any{"status": "resolved", "user_id": userID})
}

// isUserNotFound 判断节点侧的错误是否表示"这个用户不存在"。
//
// 节点 portal 的 /users/delete 对不存在的 uid 会回
// {"ok":false,"error":"User not found"}，而 portalCall 把它包装成
// "节点 API 报错: User not found"。这里做**大小写不敏感的子串匹配**，
// 因为不同版本的 portal 措辞可能不同，而只认一种会让本该成功的补删被判成失败。
func isUserNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "user not found")
}
