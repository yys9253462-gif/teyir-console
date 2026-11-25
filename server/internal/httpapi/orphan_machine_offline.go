package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// machineOfflineResolvedNote 是按节点批量结案时写进 resolved_note 的固定文案。
// 与单条结案（resolveOrphan）区分开，事后能看出是「整台机器确认下线」一次结的。
const machineOfflineResolvedNote = "人工确认机器已下线，整台节点的待补删一并结案（账号未从节点上删除）"

// POST /api/orphans/nodes/{nodeID}/resolve-machine-offline
//
// 2026-11-08：节点已删除、机器也已下线时，把这台节点名下所有未结案的待补删记录一次结案。
// 原来只能逐条点「已解决」（雅加达积压了 20 条）。
//
// 约束：
//   - 管理员 + X-Confirm-Dangerous: yes（与单条结案、删除节点同一约定）。
//   - 只接受**已软删**的节点：在役节点的账号还能补删，不能用这个入口放弃回收。
//     在役节点请走删除节点并勾选「机器已永久停用」。
//   - 只结案 resolved_at IS NULL 的行，重复调用安全。
//   - 一条审计记录，带结案条数与节点名。
func (s *Server) resolveOrphansMachineOffline(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "按节点结案待补删需要 X-Confirm-Dangerous: yes",
		})
		return
	}
	nodeID := strings.TrimSpace(r.PathValue("nodeID"))
	if !uuidPattern.MatchString(nodeID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "节点 ID 格式无效"})
		return
	}

	var name string
	var deleted bool
	err := s.db.QueryRow(r.Context(),
		`SELECT name, deleted_at IS NOT NULL FROM hysteria_nodes WHERE id=$1::uuid`, nodeID).Scan(&name, &deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// 节点行已物理删除：以待补删记录里留存的节点名为准，仍允许结案。
		if qerr := s.db.QueryRow(r.Context(),
			`SELECT COALESCE(max(node_name), '') FROM orphan_node_accounts WHERE node_id=$1::uuid`, nodeID).Scan(&name); qerr != nil {
			s.databaseError(w, r, qerr)
			return
		}
		deleted = true
	case err != nil:
		s.databaseError(w, r, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "节点 " + name + " 仍在役。请先在节点列表删除它并勾选「机器已永久停用」，再处理待补删。",
		})
		return
	}

	tag, err := s.db.Exec(r.Context(), `
UPDATE orphan_node_accounts SET resolved_at = now(), resolved_note = $2
WHERE node_id = $1::uuid AND resolved_at IS NULL`, nodeID, machineOfflineResolvedNote)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	resolved := tag.RowsAffected()
	s.audit(r.Context(), r, "resolve_orphans_machine_offline", "node", nodeID,
		map[string]any{"node_name": name, "resolved": resolved})
	writeJSON(w, http.StatusOK, map[string]any{"status": "resolved", "node_name": name, "resolved": resolved})
}
