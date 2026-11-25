package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GET /api/sub-ids —— 跨节点聚合子账号清单。
//
// 设计：一次请求拿到所有 in_store 节点上的 sub_* 账号（按 user_id → [node_name, ...]）。
// 实现：并发对每个 in_store 节点调 /users/list —— 与 getNode 的接口同一条路径，
// 不重写 portalCall 链路。⚠️ 故意**不在** /api/nodes/{id} 下，避免 Go ServeMux
// 的 {id} 占位符把 "sub-ids" 解释成某个节点的 id（2026-10-03 加）。
//
// ⚠️ 鉴权：admin-only —— 暴露的是 hysteria portal /users/list 全集，
// 不只是订阅范围内的账号。这与 /api/nodes/{id}/users 同口径。
func (s *Server) subIdsAggregate(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}

	// 收集所有 in_store 节点。
	type nodeRow struct {
		NodeID   string
		NodeName string
		BaseURL  string
		APIKey   string
	}
	var nodes []nodeRow
	var undecryptable, missingAPI []string
	rows, err := s.db.Query(r.Context(),
		`SELECT id, name, COALESCE(api_url,''), api_key
		 FROM hysteria_nodes
		 -- 2026-11-06：不再只看 in_store。开户侧对**自用订阅**允许在未上架节点上开户
		 -- （filterProvisionCandidates 只对买家订阅要求上架），而这里按 in_store=TRUE 过滤，
		 -- 那些账号在跨节点审计里永远看不到 —— 清理时判定「干净」，节点上账号其实还在用。
		 WHERE deleted_at IS NULL`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var n nodeRow
		var cipher []byte
		if err := rows.Scan(&n.NodeID, &n.NodeName, &n.BaseURL, &cipher); err != nil {
			s.databaseError(w, r, err)
			return
		}
		// 2026-11-06：未绑 API 的节点原来被 WHERE 条件静默排除，审计结果里连「这台没查」都看不到。
		// 现在列进 skipped_nodes，让「查过了没有」与「根本没查」可区分。
		if strings.TrimSpace(n.BaseURL) == "" || len(cipher) == 0 {
			missingAPI = append(missingAPI, n.NodeName)
			continue
		}
		if s.box == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "secrets box 未初始化"})
			return
		}
		apiKey, err := s.box.Open(cipher)
		if err != nil {
			s.logger.Warn("subIds: 解密 api_key 失败", "node", n.NodeName, "error", err)
			// 2026-11-03：原来直接 continue，结果里既没有这台节点的账号、也没有它的错误，
			// 看起来像「这台节点上没有任何买家账号」。记进 node_errors。
			undecryptable = append(undecryptable, n.NodeName)
			continue
		}
		n.APIKey = string(apiKey)
		clear(apiKey)
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}

	// 并发拉每个节点的 /users/list。⚠️ 复用 portalUsersListResponse 结构。
	type result struct {
		nodeID   string
		nodeName string
		users    []portalUser
		err      string
	}
	ch := make(chan result, len(nodes))
	// ⚠️ 2026-10-31：这里原先是"假限流"——创建了 sem 却只有一句 `_ = sem`，
	// 所有节点的 /users/list 仍然是**同时**打出去的。
	// 节点侧是单线程 Python http.server，节点一多就会重现
	// "unexpected EOF / 长时间无响应"。现在真的在 goroutine 里取槽位。
	sem := make(chan struct{}, provisioningConcurrency)
	for _, n := range nodes {
		go func(n nodeRow) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				ch <- result{nodeID: n.NodeID, nodeName: n.NodeName, err: "请求已取消"}
				return
			}
			// 每个节点施加独立超时预算，避免单台挂死节点拖垮整个聚合请求。
			callCtx, cancel := context.WithTimeout(r.Context(), provisionTimeout)
			envelope, err := portalCall(callCtx, n.BaseURL, n.APIKey, http.MethodGet, "/users/list", nil)
			cancel()
			if err != nil {
				ch <- result{nodeID: n.NodeID, nodeName: n.NodeName, err: err.Error()}
				return
			}
			var parsed portalUsersListResponse
			if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
				ch <- result{nodeID: n.NodeID, nodeName: n.NodeName, err: "节点返回的用户列表无法解析"}
				return
			}
			ch <- result{nodeID: n.NodeID, nodeName: n.NodeName, users: parsed.Users}
		}(n)
	}
	// 收敛。总上限按"批次数 × 单节点预算"给足：节点数超过并发档位时需要多轮，
	// 用固定 2× 会把后几批整体判成超时（2026-10-31 修）。
	results := make([]result, 0, len(nodes))
	collected := 0
	batches := (len(nodes) + provisioningConcurrency - 1) / provisioningConcurrency
	if batches < 1 {
		batches = 1
	}
	timeout := time.NewTimer(time.Duration(batches+1) * provisionTimeout)
	defer timeout.Stop()
	for collected < len(nodes) {
		select {
		case res := <-ch:
			results = append(results, res)
			collected++
		case <-r.Context().Done():
			writeJSON(w, statusClientClosedRequest, map[string]string{"error": "client closed request"})
			return
		case <-timeout.C:
			s.logger.Warn("subIds: 部分节点超时", "got", collected, "want", len(nodes))
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{
				"error": fmt.Sprintf("部分节点超时（%d/%d）", collected, len(nodes)),
			})
			return
		}
	}

	// 聚合：user_id → [{node_id, node_name}, ...]
	type member struct {
		NodeID   string `json:"node_id"`
		NodeName string `json:"node_name"`
	}
	found := make(map[string][]member)
	nodeErrors := make(map[string]string)
	for _, name := range undecryptable {
		nodeErrors[name] = "节点 API 密钥解密失败"
	}
	for _, name := range missingAPI {
		nodeErrors[name] = "未接入节点 API，无法查询该节点上的账号"
	}
	// 2026-11-06：collected_nodes 只能扣「results 里失败的」那些。解密失败的节点既不在 nodes 也不在
	// results 里，却算进 nodeErrors：5 台在架、1 台解密失败会报 collected=3，看起来还有一台没答。
	errorsInResults := 0
	for _, res := range results {
		if res.err != "" {
			key := res.nodeName
			if _, exists := nodeErrors[key]; exists {
				key = fmt.Sprintf("%s (%s)", res.nodeName, shortID(res.nodeID))
			}
			nodeErrors[key] = res.err
			errorsInResults++
			continue
		}
		// 2026-11-03：results 按 channel 到达顺序排列，与 nodes 的下标无关。
		// 原来用 nodes[i].NodeID，两台以上节点时账号会被记到别的节点名下。
		nodeID := res.nodeID
		for _, u := range res.users {
			uid := u.UserID
			if !strings.HasPrefix(uid, "sub_") {
				continue
			}
			found[uid] = append(found[uid], member{NodeID: nodeID, NodeName: res.nodeName})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"by_user_id":      found,
		"node_errors":     nodeErrors,
		"queried_nodes":   len(nodes),
		"collected_nodes": len(results) - errorsInResults,
		"skipped_nodes":   append(append([]string{}, undecryptable...), missingAPI...),
	})
}
