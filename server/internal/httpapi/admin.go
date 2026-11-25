package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"nodeops/server/internal/auth"
)

func (s *Server) updateNode(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Name       *string `json:"name"`
		ServerID   *string `json:"server_id"`
		ListenPort *int    `json:"listen_port"`
		Version    *string `json:"version"`
		Domain     *string `json:"domain"`
		InStore    *bool   `json:"in_store"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}

	nodeID := r.PathValue("id")
	var prevInStore, hasNodeAPI, hasPortalBasic bool
	var prevServerID string
	var prevPort int
	if err := s.db.QueryRow(r.Context(), `SELECT in_store,
       (COALESCE(api_url,'') <> '' AND api_key IS NOT NULL),
       (portal_basic_user IS NOT NULL AND portal_basic_password IS NOT NULL AND COALESCE(portal_path_token,'') <> ''),
       COALESCE(server_id::text, ''), COALESCE(listen_port, 0)
       FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&prevInStore, &hasNodeAPI, &hasPortalBasic, &prevServerID, &prevPort); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}

	fields := make([]string, 0, 6)
	args := make([]any, 0, 7)
	add := func(column string, value any) {
		args = append(args, value)
		fields = append(fields, column+"=$"+strconv.Itoa(len(args)))
	}
	if input.Name != nil && strings.TrimSpace(*input.Name) != "" {
		add("name", strings.TrimSpace(*input.Name))
	}
	resetStatus := false
	if input.ServerID != nil && strings.TrimSpace(*input.ServerID) != "" {
		target := strings.TrimSpace(*input.ServerID)
		// Moving a node to a host that does not exist (or was soft-deleted) would
		// otherwise fail later with an opaque FK error, and the node would be left
		// pointing at nothing. Check up front and answer with something actionable.
		var exists bool
		if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM servers WHERE id=$1::uuid AND deleted_at IS NULL)`, target).Scan(&exists); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "server_id 不是合法的 UUID"})
			return
		}
		if !exists {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "目标主机不存在或已删除"})
			return
		}
		add("server_id", target)
		// 2026-11-06：只在真的换了主机时才重置状态。编辑表单每次保存都带上 server_id / listen_port：
		// 原来改个名字、切一下商城开关，离线 / 不稳定的节点就变回「未知」、连续失败计数清零，告警要重新累计。
		if !strings.EqualFold(target, prevServerID) {
			resetStatus = true
		}
	}
	if input.ListenPort != nil {
		if *input.ListenPort < 1 || *input.ListenPort > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需为 1-65535"})
			return
		}
		add("listen_port", *input.ListenPort)
		if *input.ListenPort != prevPort {
			resetStatus = true
		}
	}
	if input.Version != nil {
		add("version", strings.TrimSpace(*input.Version))
	}
	if input.Domain != nil {
		add("domain", strings.TrimSpace(*input.Domain))
	}
	closingStore := false
	if input.InStore != nil {
		// ⚠️ 2026-10-31：上架必须真的能被交付。
		//
		// 没有节点 API 密钥的节点，购物车里的买家永远开不出账号：
		//   applySubscriptionAccounts 要求专属账号 → 没有 api_key 就开不了；
		//   needsAccountProvisioning 因此对每条订阅都恒为 true →
		//   每小时的 gap-fill 把每条订阅排进同步队列、每轮都失败；
		//   更要命的是商城那张订单的 delivery_status 会一直停在 pending，
		//   买家看到的是"已付款但未交付"，而运维在节点页看不出哪里不对。
		// 这类节点只适合「自用」。要上架，先走「接入节点」绑定 API。
		if *input.InStore && !prevInStore && !hasNodeAPI {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":    "该节点没有绑定节点 API 密钥，无法给买家开户，不能上架商城；请先用「接入节点」绑定 portal_access.json",
				"need_api": true,
				"node_id":  nodeID,
			})
			return
		}
		// 2026-11-04：买家订阅里的每台节点都要经门户网页会话（Basic 凭据）读出该账号的客户端参数；
		// 只绑了 API 密钥、没填门户账号密码的节点能开户，却永远进不了任何买家的配置 ——
		// 订单照样显示已交付（账号确实开了），交付页也不列出它，范围里全是这种节点时买家拿到 0 个节点。
		if *input.InStore && !prevInStore && !hasPortalBasic {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":      "该节点没有保存门户网页账号密码，买家订阅里生成不出它的配置，不能上架商城；请在「换绑节点 API」里补填门户账号密码",
				"need_basic": true,
				"node_id":    nodeID,
			})
			return
		}
		add("in_store", *input.InStore)
		if prevInStore && !*input.InStore {
			closingStore = true
		}
	}
	if len(fields) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有可更新字段"})
		return
	}

	args = append(args, nodeID)
	statusClause := ""
	if resetStatus {
		// ⚠️ 2026-10-31：改动端口 / 换主机之后，**连续失败计数必须一起清零**。
		//
		// 否则一台原本连续失败 3 次的节点，被运维换了端口（说明确实动过它）、
		// 下一次探测只要再失败一次，collector 就会立刻把它判成 warning ——
		// 但这次失败很可能是端口刚改、服务还没起来的正常过渡。
		// 状态已经回到 unknown（"等下一轮探测"），计数还留在旧节点上，两者互相矛盾。
		statusClause = ", status='unknown', last_checked_at=NULL, consecutive_probe_failures=0"
	}
	query := "UPDATE hysteria_nodes SET " + strings.Join(fields, ", ") + statusClause + ", updated_at=now() WHERE id=$" + strconv.Itoa(len(args)) + "::uuid AND deleted_at IS NULL"
	tag, err := s.db.Exec(r.Context(), query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该主机上已存在同端口节点"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}

	InvalidateSubscriptionCache()

	if closingStore {
		// 节点仍在库中（只是不再下发给买家），凭据可以正常取到。
		// 但回收失败仍必须显式回给操作者：映射保留着可重试，
		// 而「已下架」若被当成「已回收」，买家账号就还在节点上能用。
		outcome := s.revokeNodeFromBuyerSubscriptions(r.Context(), nodeID)
		if outcome.Err != nil {
			s.logger.Error("节点已下架但无法完成账号回收", "node_id", nodeID, "error", outcome.Err)
			s.audit(r.Context(), r, "update_node", "hysteria_node", nodeID, map[string]any{"fields": fields, "revoke_error": outcome.Err.Error()})
			writeJSON(w, http.StatusMultiStatus, map[string]any{
				"status": "updated_with_revoke_failures",
				"error":  "节点已移出商城，但无法确认买家账号是否已回收，请检查并重试",
			})
			return
		}
		if len(outcome.Failed) > 0 {
			s.audit(r.Context(), r, "update_node", "hysteria_node", nodeID, map[string]any{"fields": fields, "revoke_failed": len(outcome.Failed)})
			writeJSON(w, http.StatusMultiStatus, map[string]any{
				"status":        "updated_with_revoke_failures",
				"revoked_count": outcome.Revoked,
				"failed_count":  len(outcome.Failed),
				"failed":        outcome.Failed,
				"error": fmt.Sprintf("节点已移出商城，但有 %d 个买家账号未能从节点侧注销，映射已保留可直接重试: %s",
					len(outcome.Failed), outcome.FailedSummary()),
			})
			return
		}
	}
	s.audit(r.Context(), r, "update_node", "hysteria_node", nodeID, map[string]any{"fields": fields})
	if input.InStore != nil && *input.InStore && !prevInStore {
		// 重新上架：给存量订阅补开这台节点的账号（见 provision_gapfill.go）。
		s.queueProvisioningGapsAsync("node_listed")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除凭据需要 X-Confirm-Dangerous: yes"})
		return
	}
	serverID := r.PathValue("id")
	// 2026-11-06：安装 / 迁移进行中（或中断待核验）不能删凭据。任务每一步都要重新 SSH 上去：
	// 删到一半，迁移的回滚连不上旧主机、安装的收尾登记不了节点，主机停在「装了一半」的状态。
	// 与换 IP、删主机同一判据。
	if busy, err := s.serverHasActiveHostJob(r.Context(), serverID); err != nil {
		s.databaseError(w, r, err)
		return
	} else if busy {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "这台主机有安装/迁移任务在进行（或待核验结案），完成后再删除凭据"})
		return
	}
	tag, err := s.db.Exec(r.Context(), `DELETE FROM server_credentials WHERE server_id=$1::uuid`, serverID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "该主机没有已保存的凭据"})
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE servers SET status='unknown', latency_ms=NULL, updated_at=now() WHERE id=$1::uuid`, serverID); err != nil {
		s.logger.Warn("reset server status failed", "error", err)
	}
	s.audit(r.Context(), r, "delete_credential", "server", serverID, map[string]any{"removed": tag.RowsAffected()})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "removed": tag.RowsAffected()})
}

func (s *Server) retryTask(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	sourceID := r.PathValue("id")
	var serverID, nodeID, operation string
	var parameters []byte
	err := s.db.QueryRow(r.Context(), `SELECT server_id, COALESCE(node_id::text, ''), operation_type, parameters FROM operation_tasks WHERE id=$1::uuid`, sourceID).
		Scan(&serverID, &nodeID, &operation, &parameters)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在"})
		return
	}
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	// 2026-11-03：与新建任务同一张白名单。历史行可能是 deploy / config_apply / upgrade 这类
	// 执行器从没实现过的类型；原来重试照样入队、回 201，任务注定在执行时失败（谎报成功）。
	if !supportedOperations[operation] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该任务类型（" + operation + "）已不再支持，无法重试"})
		return
	}
	// 2026-11-12：与 createTask 同一门槛 —— 写类任务（含 system_preset）仅限管理员。createTask 在 11-06 加了这条，
	// 重试这条入口漏了：operator 不能新建 kernel_reboot / uninstall_hy2，却能对着管理员跑过的那条点「重试」，
	// 以 root 原样再跑一遍。只读任务（health_check / logs）的重试对 operator 照常可用。
	if !readOnlyOperations[operation] && claims.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "重试写类主机任务（重启服务、系统预设等）仅限管理员；只读任务（健康检查、拉取日志）不受限制"})
		return
	}
	if operation != "health_check" && operation != "logs" && r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "重试危险操作需要 X-Confirm-Dangerous: yes"})
		return
	}
	var id string
	// 2026-11-04：同 createTask —— 主机已删除时不再入队（执行器不会领取，任务永远排着）。
	if err := s.db.QueryRow(r.Context(), `INSERT INTO operation_tasks (server_id, node_id, operation_type, parameters)
SELECT s.id, NULLIF($2, '')::uuid, $3, $4 FROM servers s WHERE s.id = $1::uuid AND s.deleted_at IS NULL RETURNING id`, serverID, nodeID, operation, parameters).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务所在主机已删除，无法重试"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "retry_task", "operation_task", id, map[string]any{"source_task": sourceID})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "queued"})
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	// 2026-11-06：与其它写接口一样走写限流（原来改角色、删账号两处漏了）。
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Role *string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil || input.Role == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role 必填"})
		return
	}
	role := strings.TrimSpace(*input.Role)
	if role != "admin" && role != "operator" && role != "viewer" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role 需为 admin/operator/viewer"})
		return
	}
	// 2026-11-03：先规范成小写标准 UUID 再与本人 id 比较。原来直接比字符串，
	// PostgreSQL 的 ::uuid 却接受大写 / 花括号 / 无连字符写法 ⇒ 唯一的管理员用大写 id 就能把自己降级。
	userID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	if userID == claims.Subject && role != "admin" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不能取消自己的管理员角色"})
		return
	}
	// 「至少留一个管理员」：两个管理员同时互相降级时各自的本人检查都会通过，
	// 所以在事务里持一把全局锁再数管理员、再改。
	found, applied, err := s.changeUserKeepingAdmin(r.Context(), userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET token_version=token_version+CASE WHEN role<>$1 THEN 1 ELSE 0 END, role=$1 WHERE id=$2::uuid`, role, userID)
		return err
	}, role != "admin")
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	if !applied {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "至少要保留一个管理员"})
		return
	}
	s.audit(r.Context(), r, "update_user", "user", userID, map[string]any{"role": role})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 与创建用户和本人改密使用同一口令策略。
	if err := auth.ValidatePassword(input.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 2026-11-04：不能用「重置他人密码」改自己的密码。这条路径不要求当前密码 ——
	// 被盗用的管理员会话可以借它直接把密码换掉、把真正的主人锁在外面（本人改密必须验证当前密码）；
	// 而且 token_version+1 会立刻作废本次会话，操作者当场被踢回登录页。改自己的密码请走「修改密码」。
	userID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	if userID == claims.Subject {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "修改自己的密码请使用「修改密码」（需要验证当前密码）"})
		return
	}
	hash, err := auth.HashPassword(input.Password)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE users SET password_hash=$1, token_version=token_version+1 WHERE id=$2::uuid`, hash, userID)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	s.audit(r.Context(), r, "reset_password", "user", userID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// changeUserKeepingAdmin 在「至少保留一个管理员」的约束下对一个用户执行 change。
//
// removesAdmin 表示这次操作会让目标不再是管理员（降级或删除）。全局 advisory 锁让
// 并发的降级 / 删除串行化：后到的那次会看到前一次已提交的结果，数到的管理员数是真实的。
// 返回 found=false 表示用户不存在；applied=false 表示被约束拒绝（未做任何修改）。
func (s *Server) changeUserKeepingAdmin(ctx context.Context, userID string, change func(context.Context, pgx.Tx) error, removesAdmin bool) (found, applied bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('hy2-ops:users:admin-guard'))`); err != nil {
		return false, false, err
	}
	var targetRole string
	if err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1::uuid`, userID).Scan(&targetRole); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	if removesAdmin && targetRole == "admin" {
		var admins int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
			return true, false, err
		}
		if admins <= 1 {
			return true, false, nil
		}
	}
	if err = change(ctx, tx); err != nil {
		return true, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return true, false, err
	}
	return true, true, nil
}

// canonicalUUID 把路径里的用户 id 规范成小写标准 UUID；不是标准格式时返回 false。
func canonicalUUID(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	if !uuidPattern.MatchString(v) {
		return "", false
	}
	return strings.ToLower(v), true
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除用户需要 X-Confirm-Dangerous: yes"})
		return
	}
	userID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	if userID == claims.Subject {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "不能删除当前登录账号"})
		return
	}
	// 2026-11-04：portal_update_batches.created_by 是 ON DELETE CASCADE。删掉一个正在跑批量门户更新的
	// 账号，批次行随之消失：后台下一次 savePortalBatch 返回「任务已中断」，滚动更新停在半路
	// （部分节点已更新、部分没有），进度记录也一并没了。等它跑完再删。
	var running bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM portal_update_batches WHERE created_by=$1::uuid AND status='running')`, userID).Scan(&running); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该账号发起的批量门户更新仍在进行，完成后再删除"})
		return
	}
	found, deleted, err := s.changeUserKeepingAdmin(r.Context(), userID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1::uuid`, userID)
		return err
	}, true)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "用户不存在"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "至少要保留一个管理员"})
		return
	}
	s.audit(r.Context(), r, "delete_user", "user", userID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
