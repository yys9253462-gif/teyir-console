package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// store_adopt.go —— 手工开通的订阅接入商城续费（2026-10-30）。
//
// 商城续费按「原订单号」找订阅，手工开通的订阅 order_id 为空，原来只能去商城新买一份
// （会生成新链接）。现在第一次续费时由商城下一笔新订单，付款后把这笔订单号「挂」到
// 这个手工订阅上（adopt），再走现有的 /api/store/renew 延期。此后它就和商城买的订阅
// 完全一样：续费、退款、到期回收都走原有流程，不再另起一套。
//
// 只挂订单号，不改设备数、节点分类、标签 —— 运营手工设定的配置保持原样。

// GET /api/store/adopt-info?token= —— 商城下单前确认：这个订阅存在、是手工开通、可以续费。
func (s *Server) storeAdoptInfo(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if len(token) < 20 || len(token) > 128 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到订阅"})
		return
	}
	rec, err := s.loadSubscriptionByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	var isPanelMerged bool
	if err := s.db.QueryRow(r.Context(), `SELECT COALESCE(is_panel_merged, FALSE) FROM subscription_tokens WHERE id=$1::uuid`, rec.ID).Scan(&isPanelMerged); err != nil {
		s.databaseError(w, r, err)
		return
	}
	var hasUnlisted bool
	if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (
		SELECT 1 FROM hysteria_nodes n
		WHERE n.deleted_at IS NULL AND NOT n.in_store
		  AND (n.id::text IN (SELECT jsonb_object_keys(COALESCE(st.node_accounts, '{}'::jsonb)) FROM subscription_tokens st WHERE st.id = $1::uuid)
		       OR n.id = ANY(COALESCE((SELECT node_ids FROM subscription_tokens WHERE id = $1::uuid), '{}'::uuid[])))
	)`, rec.ID).Scan(&hasUnlisted); err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"manual":             strings.TrimSpace(rec.OrderID) == "" && !isPanelMerged,
		"panel_merged":       isPanelMerged,
		"has_unlisted_nodes": hasUnlisted,
		"expired":            rec.expired(time.Now()),
		"order_id":           rec.OrderID,
		"perpetual":          rec.ExpiresAt == nil,
		"enabled":            rec.Enabled,
		"expires_at":         epochOrZero(rec.ExpiresAt),
		"ip_limit":           rec.IPLimit,
	})
}

// POST /api/store/adopt {"token": "...", "order_id": "OPS-prdord_..."}
//
// 幂等：已挂的是同一个订单号时直接成功（商城重试开户会再调一次）；
// 已挂了别的订单号则拒绝，绝不改写已有关联。永久订阅不需要续费，也拒绝。
func (s *Server) storeAdopt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token   string `json:"token"`
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	token, orderID := strings.TrimSpace(in.Token), strings.TrimSpace(in.OrderID)
	if len(token) < 20 || len(token) > 128 || !shopOrderIDPattern.MatchString(orderID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token 或 order_id 不合法"})
		return
	}
	rec, err := s.loadSubscriptionByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	lockedCtx, release, err := s.lockSubscription(r.Context(), rec.ID)
	if err != nil {
		s.writeSubscriptionLockError(w, r, err)
		return
	}
	defer release()
	// 在订阅锁内重读最新状态，彻底消除与并发修改/删除/转永久的 TOCTOU 窗口。
	fresh, err := s.loadSubscriptionByID(lockedCtx, rec.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "未找到订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	rec = fresh
	var isPanelMerged bool
	if err := s.db.QueryRow(lockedCtx, `SELECT COALESCE(is_panel_merged, FALSE) FROM subscription_tokens WHERE id=$1::uuid`, rec.ID).Scan(&isPanelMerged); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if isPanelMerged {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "面板默认合并订阅不可被商城订单接管"})
		return
	}
	if rec.ExpiresAt == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "永久订阅无需续费"})
		return
	}
	// 2026-11-04：挂上订单号后它就成了「买家订阅」，下发与开户只认已上架节点。
	// 原来照挂不误：手工订阅里用着的未上架（自用）节点会从订阅里消失，而那台节点上的账号
	// 既不回收也不再下发 —— 老配置照样能连，新导入的又少一台。用到未上架节点就拒绝，让运营先决定。
	if strings.TrimSpace(rec.OrderID) == "" {
		var unlisted []string
		if err := s.db.QueryRow(lockedCtx, `
SELECT COALESCE(array_agg(n.name ORDER BY n.name), '{}')
  FROM hysteria_nodes n
 WHERE n.deleted_at IS NULL AND NOT n.in_store
   AND (n.id::text IN (SELECT jsonb_object_keys(COALESCE(st.node_accounts, '{}'::jsonb)) FROM subscription_tokens st WHERE st.id = $1::uuid)
        OR n.id = ANY(COALESCE((SELECT node_ids FROM subscription_tokens WHERE id = $1::uuid), '{}'::uuid[])))`, rec.ID).Scan(&unlisted); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if len(unlisted) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该订阅用到了未上架的节点（" + strings.Join(unlisted, "、") + "），关联订单后这些节点会被移出订阅；请先在面板里处理这些节点或改为新开订单"})
			return
		}
	}
	// 2026-11-06：商城撤销过的订单号不能再挂到别的订阅上（与 storeCreateOrder 同一判据，见迁移 056）。
	var revoked bool
	if err := s.db.QueryRow(lockedCtx, `SELECT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id=$1)`, orderID).Scan(&revoked); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if revoked {
		writeJSON(w, http.StatusGone, map[string]string{"error": "该订单已被商城撤销（退款 / 取消），不能再关联订阅"})
		return
	}
	var id string
	err = s.db.QueryRow(lockedCtx, `UPDATE subscription_tokens SET order_id=$2
		WHERE id=$1::uuid AND deleted_at IS NULL AND NOT COALESCE(is_panel_merged, FALSE)
		  AND (COALESCE(order_id,'')='' OR order_id=$2)
		  AND NOT EXISTS (SELECT 1 FROM store_revoked_orders WHERE order_id=$2)
		RETURNING id::text`, rec.ID, orderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "该订阅已关联其他订单，请从原订单续费"})
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "该订单号已关联其他订阅"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if strings.TrimSpace(rec.OrderID) == "" {
		s.audit(r.Context(), r, "store_adopt_subscription", "subscription_token", rec.ID, map[string]any{"order_id": orderID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "order_id": orderID, "expires_at": epochOrZero(rec.ExpiresAt)})
}
