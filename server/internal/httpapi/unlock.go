package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 流媒体 / AI 解锁检测（2026-10-15）。
//
// 检测必须在**节点本机**发起 —— 解锁看的是节点出口 IP，面板所在主机直接探测
// 只能得到面板本机出口的结论。所以走节点 portal 的 GET /api/v1/unlock-check
// （portal 2.8+），目标站点硬编码在 portal 里，面板传不进任何 URL。
//
// 与 IP 纯净度一样只在用户点按钮时检测，不做后台轮询：每次检测会以节点 IP
// 访问 Netflix / OpenAI 等站点，频繁访问本身就可能让 IP 被标记。

// unlockCallTimeout：portal 侧单项 8s，ChatGPT 一项串行三次请求，最坏约 25s。
const unlockCallTimeout = 45 * time.Second

type unlockResult struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region string `json:"region"`
	Note   string `json:"note"`
}

var unlockStatuses = map[string]bool{"yes": true, "partial": true, "no": true, "unknown": true}

// parseUnlockResponse 校验 portal 返回并收窄成已知字段，不把节点侧的
// 任意 JSON 原样落库。
func parseUnlockResponse(raw []byte) ([]unlockResult, error) {
	var payload struct {
		Results []unlockResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("解锁检测返回格式无法解析")
	}
	if len(payload.Results) == 0 {
		return nil, errors.New("解锁检测没有返回任何结果")
	}
	out := make([]unlockResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		if r.Key == "" || len(r.Key) > 32 {
			continue
		}
		if !unlockStatuses[r.Status] {
			r.Status = "unknown"
		}
		r.Name = truncateRunes(r.Name, 40)
		r.Region = truncateRunes(strings.ToUpper(r.Region), 8)
		r.Note = truncateRunes(r.Note, 80)
		out = append(out, r)
	}
	return out, nil
}

// errUnlockNoAPI：这台节点没有可用的节点 API，解锁检测对它不适用（2026-11-12 提成哨兵，文案不变）。
// 定时巡检据此把「不适用」与「检测失败」分开：前者重试多少次都不会好，不该触发整轮重跑。
var errUnlockNoAPI = errors.New("该节点没有可用的节点 API（需接入 portal 并保存 api_key）")

// checkAndStoreUnlock 是检测 + 落库的唯一实现，单节点与批量共用。
// 失败只写 error，不覆盖上次成功的 results / checked_at。
func (s *Server) checkAndStoreUnlock(ctx context.Context, nodeID string) ([]unlockResult, error) {
	target, err := s.loadPortalTarget(ctx, nodeID)
	if err != nil {
		// 2026-11-06：照实说原因。原来一律「没有可用的节点 API」：节点只是正在安装 / 迁移，
		// 或密钥解密失败、数据库抖动，都被说成没接入，运维去重新粘 portal_access.json。
		switch {
		case errors.Is(err, errPortalNotBound), errors.Is(err, errPortalNodeNotFound):
			return nil, errUnlockNoAPI
		case errors.Is(err, errPortalNodeBusy):
			return nil, err
		default:
			s.logger.Warn("unlock: 读取节点凭据失败", "node", nodeID, "error", err)
			return nil, errors.New("读取节点 API 凭据失败，请查看服务端日志")
		}
	}
	env, err := portalCallWithin(ctx, unlockCallTimeout, target.BaseURL, target.APIKey, http.MethodGet, "/unlock-check", nil)
	var results []unlockResult
	if err == nil {
		results, err = parseUnlockResponse(env.Data)
	} else if isPortalEndpointMissing(err.Error()) {
		// ⚠️ 2026-10-25 五轮排查第三轮：与 capabilities / outbounds 统一判据。
		// 这里原来只匹配 "not found"，而同一种「端点不存在」在其它路径上
		// 既可能写成 "API endpoint not found"，也可能只剩状态码 404 ——
		// 两套判据并存必然漂移，运维会看到同一类问题两种说法。
		err = errors.New("节点门户版本过低，不支持解锁检测；请先「批量更新门户」到 2.8 及以上")
	}
	if err != nil {
		if _, dbErr := s.db.Exec(ctx, `
INSERT INTO node_unlock (node_id, error, updated_at) VALUES ($1::uuid, $2, now())
ON CONFLICT (node_id) DO UPDATE SET error = EXCLUDED.error, updated_at = now()`, nodeID, err.Error()); dbErr != nil {
			s.logger.Warn("unlock: record error failed", "node", nodeID, "error", dbErr)
		}
		return nil, err
	}
	encoded, _ := json.Marshal(results)
	if _, err := s.db.Exec(ctx, `
INSERT INTO node_unlock (node_id, results, error, checked_at, updated_at) VALUES ($1::uuid, $2, '', now(), now())
ON CONFLICT (node_id) DO UPDATE SET results = EXCLUDED.results, error = '', checked_at = now(), updated_at = now()`,
		nodeID, encoded); err != nil {
		// 2026-11-12：与上面读凭据失败同一做法 —— 原文只进日志，对外给通用文案。原来直接返回数据库错误：
		// 调用方把它写进响应（表名、约束名、SQLSTATE），定时巡检那条路径则整个丢掉，服务端没有任何记录可查。
		s.logger.Warn("unlock: store results failed", "node", nodeID, "error", err)
		return nil, errors.New("解锁检测已完成，但结果写入数据库失败，请查看服务端日志")
	}
	return results, nil
}

func (s *Server) refreshNodeUnlock(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	var exists bool
	if err := s.db.QueryRow(r.Context(), `SELECT true FROM hysteria_nodes WHERE id=$1::uuid AND deleted_at IS NULL`, nodeID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	results, err := s.checkAndStoreUnlock(r.Context(), nodeID)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errPortalNodeBusy) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), r, "refresh_node_unlock", "hysteria_node", nodeID, map[string]any{"count": len(results)})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "results": results})
}

func (s *Server) listUnlock(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	rows, err := s.db.Query(r.Context(), `
SELECT n.id, COALESCE(u.results, '[]'::jsonb), COALESCE(u.error, ''), u.checked_at
FROM hysteria_nodes n
JOIN node_unlock u ON u.node_id = n.id
WHERE n.deleted_at IS NULL`)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer rows.Close()
	type item struct {
		NodeID    string          `json:"node_id"`
		Results   json.RawMessage `json:"results"`
		Error     string          `json:"error"`
		CheckedAt *time.Time      `json:"checked_at"`
	}
	out := make([]item, 0, 32)
	for rows.Next() {
		var it item
		var raw []byte
		if err := rows.Scan(&it.NodeID, &raw, &it.Error, &it.CheckedAt); err != nil {
			s.databaseError(w, r, err)
			return
		}
		it.Results = raw
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
