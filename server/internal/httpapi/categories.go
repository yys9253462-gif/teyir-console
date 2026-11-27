package httpapi

// categories.go —— 节点分类（AI 专用 / 跨境专用 / 普通自用…）。
//
// ## 需求（2026-10-03 管理员）
//
// 节点原来只有一个布尔 `in_store`（商城/自用），买家拿到的是**全部已上架节点**。
// 管理员要按用途分类，并让分类同时决定**上架范围**：
//   * 商城商品选「AI 专用」→ 买家只拿到 AI 专用的节点
//   * 商城商品选「跨境专用」→ 买家只拿到跨境专用的节点
//   * 分类本身可在后台增删改，节点的分类可随时切换
//
// 所以分类同时是「分组」和「上架范围」，不是单纯的标签。
//
// ## 表的取舍（见 migrations/026）
//
// - 分类单独建表：要能增删改、要显示描述与排序、商城按 slug 引用它。
// - 节点↔分类**多对多**：一个节点既可能是 AI 专用又可能是跨境专用，
//   这两个用途不互斥，用单选会被迫二选一、之后必然要改。
// - slug 作为商城侧的引用键：可读、排障时一眼看出这个商品卖的是哪类。
// - 订阅的 category_id **可空**：NULL = 不圈定 = 全部分类 = 改之前的行为。
//   这让迁移零风险 —— 现有订阅与订单的行为一个字都不变。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// categorySlugPattern 与迁移 026 的 CHECK 约束保持一致。
//
// ⚠️ 两处必须同步：数据库挡住的是"绕过 API 直接写库"，
// 这里挡住的是"给用户一个可读的错误"。两边不一致会出现
// "API 说可以、数据库拒绝"的 500。
var categorySlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type nodeCategory struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SortOrder   int       `json:"sort_order"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// NodeCount 是**当前在役**节点里属于该分类的数量。
	// 由查询时统计而不是存列：节点会删、分类会改，
	// 存一份计数必然漂移（本项目已因"两份数据各算一遍"栽过）。
	NodeCount int `json:"node_count"`
}

// listCategories 读取全部分类（含停用的 —— 后台要能看到并重新启用）。
func (s *Server) listCategories(ctx context.Context) ([]nodeCategory, error) {
	rows, err := s.db.Query(ctx, `
SELECT c.id, c.slug, c.name, c.description, c.sort_order, c.enabled, c.created_at, c.updated_at,
       (SELECT count(*) FROM node_category_members m
          JOIN hysteria_nodes n ON n.id = m.node_id
         WHERE m.category_id = c.id AND n.deleted_at IS NULL) AS node_count
FROM node_categories c
ORDER BY c.sort_order, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]nodeCategory, 0)
	for rows.Next() {
		var c nodeCategory
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.SortOrder,
			&c.Enabled, &c.CreatedAt, &c.UpdatedAt, &c.NodeCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// loadCategoryNodeIDs 返回某个分类下**在役**节点的 id 集合。
//
// 这是订阅过滤的核心查询：生成订阅时用它圈定范围。
// 用 map 而不是 slice：调用方要对构建出来的节点做 O(1) 归属判断。
func (s *Server) loadCategoryNodeIDs(ctx context.Context, categoryID string) (map[string]bool, error) {
	rows, err := s.db.Query(ctx, `
SELECT m.node_id
FROM node_category_members m
JOIN hysteria_nodes n ON n.id = m.node_id
WHERE m.category_id = $1::uuid AND n.deleted_at IS NULL`, categoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// loadNodeCategoryMap 读全部节点的分类归属，用于节点列表展示。
// 一次查完，避免每个节点一次查询（列表页 N+1）。
func (s *Server) loadNodeCategoryMap(ctx context.Context) (map[string][]nodeCategory, error) {
	rows, err := s.db.Query(ctx, `
SELECT m.node_id, c.id, c.slug, c.name, c.sort_order, c.enabled
FROM node_category_members m
JOIN node_categories c ON c.id = m.category_id
ORDER BY c.sort_order, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]nodeCategory{}
	for rows.Next() {
		var nodeID string
		var c nodeCategory
		if err := rows.Scan(&nodeID, &c.ID, &c.Slug, &c.Name, &c.SortOrder, &c.Enabled); err != nil {
			return nil, err
		}
		out[nodeID] = append(out[nodeID], c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// GET /api/categories
func (s *Server) categoriesList(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	cats, err := s.listCategories(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": cats})
}

// POST /api/categories
func (s *Server) categoryCreate(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
		SortOrder   int    `json:"sort_order"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	slug := strings.ToLower(strings.TrimSpace(input.Slug))
	name := strings.TrimSpace(input.Name)
	if !categorySlugPattern.MatchString(slug) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "slug 只能用小写字母、数字、下划线与短横线，且以字母或数字开头（最长 32 位）",
		})
		return
	}
	if name == "" || len([]rune(name)) > 40 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称需为 1-40 个字符"})
		return
	}
	var id string
	err := s.db.QueryRow(r.Context(), `
INSERT INTO node_categories (slug, name, description, sort_order)
VALUES ($1, $2, $3, $4)
RETURNING id`, slug, name, truncateRunes(input.Description, 200), input.SortOrder).Scan(&id)
	if err != nil {
		// 唯一约束冲突要给可读提示，而不是 500。
		if strings.Contains(err.Error(), "node_categories_slug_key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "slug「" + slug + "」已被占用，换一个",
			})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "create_node_category", "node_category", id,
		map[string]any{"slug": slug, "name": name})
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "slug": slug, "name": name})
}

// PATCH /api/categories/{id}
func (s *Server) categoryUpdate(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	id := r.PathValue("id")
	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		SortOrder   *int    `json:"sort_order"`
		Enabled     *bool   `json:"enabled"`
		// slug 允许改，但**必须提醒会影响商城**：商城的 product_node_plans
		// 存的是 slug 文本，改了那边就对不上了。
		Slug *string `json:"slug"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}

	sets := []string{"updated_at = now()"}
	args := []any{}
	add := func(frag string, v any) {
		args = append(args, v)
		sets = append(sets, frag+"$"+itoa(len(args)))
	}

	if input.Name != nil {
		n := strings.TrimSpace(*input.Name)
		if n == "" || len([]rune(n)) > 40 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "名称需为 1-40 个字符"})
			return
		}
		add("name = ", n)
	}
	if input.Description != nil {
		add("description = ", truncateRunes(strings.TrimSpace(*input.Description), 200))
	}
	if input.SortOrder != nil {
		add("sort_order = ", *input.SortOrder)
	}
	if input.Enabled != nil {
		add("enabled = ", *input.Enabled)
	}
	if input.Slug != nil {
		sl := strings.ToLower(strings.TrimSpace(*input.Slug))
		if !categorySlugPattern.MatchString(sl) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "slug 只能用小写字母、数字、下划线与短横线，且以字母或数字开头（最长 32 位）",
			})
			return
		}
		add("slug = ", sl)
	}
	if len(args) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有要修改的字段"})
		return
	}
	args = append(args, id)
	tag, err := s.db.Exec(r.Context(),
		"UPDATE node_categories SET "+strings.Join(sets, ", ")+" WHERE id = $"+itoa(len(args))+"::uuid", args...)
	if err != nil {
		if strings.Contains(err.Error(), "node_categories_slug_key") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "slug 已被占用"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "分类不存在"})
		return
	}
	cats, err := s.listCategories(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "update_node_category", "node_category", id, map[string]any{
		"slug": input.Slug, "enabled": input.Enabled,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "categories": cats})
}

// DELETE /api/categories/{id}
//
// 已被订阅或节点使用的分类**不允许硬删** —— 那会让引用悬空。
// 返回明确的失败原因，让操作者改用「停用」。
func (s *Server) categoryDelete(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "删除分类不可恢复，请带 X-Confirm-Dangerous: yes 确认",
		})
		return
	}
	id := r.PathValue("id")

	// 先在事务内锁定分类行再校验在役引用与清理软删引用，避免并发把分类分配给节点或订阅造成 TOCTOU 竞态。
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	var lockedID string
	if err := tx.QueryRow(r.Context(), `SELECT id::text FROM node_categories WHERE id = $1::uuid FOR UPDATE`, id).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "分类不存在"})
			return
		}
		s.databaseError(w, r, err)
		return
	}
	var nodeCount, subCount int
	// 2026-11-03：只数在役节点。删节点是软删除，成员关系还在；原来列表里显示 0 个节点，
	// 删除却报「N 个节点仍属于它」。
	if err := tx.QueryRow(r.Context(),
		`SELECT count(*) FROM node_category_members m JOIN hysteria_nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
		 WHERE m.category_id = $1::uuid`, id).Scan(&nodeCount); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if err := tx.QueryRow(r.Context(),
		`SELECT count(*) FROM subscription_tokens WHERE category_id = $1::uuid AND deleted_at IS NULL`, id).Scan(&subCount); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if nodeCount > 0 || subCount > 0 {
		var reasons []string
		if nodeCount > 0 {
			reasons = append(reasons, itoa(nodeCount)+" 个节点仍属于它")
		}
		if subCount > 0 {
			reasons = append(reasons, itoa(subCount)+" 条订阅仍在用它")
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "不能删除：" + strings.Join(reasons, "、") + "。请改用「停用」。",
		})
		return
	}
	// 2026-11-03：已软删的订阅（商城撤单、到期回收）仍通过外键引用这个分类，
	// 外键没有 ON DELETE 动作 ⇒ 直接 DELETE 报 23503、接口返回 500。
	// 在同一事务里先解除这些已删记录与已删节点的引用，再删分类。
	if _, err := tx.Exec(r.Context(), `UPDATE subscription_tokens SET category_id = NULL WHERE category_id = $1::uuid AND deleted_at IS NOT NULL`, id); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM node_category_members m USING hysteria_nodes n
		WHERE m.category_id = $1::uuid AND n.id = m.node_id AND n.deleted_at IS NOT NULL`, id); err != nil {
		s.databaseError(w, r, err)
		return
	}
	tag, err := tx.Exec(r.Context(), `DELETE FROM node_categories WHERE id = $1::uuid`, id)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "分类不存在"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.databaseError(w, r, err)
		return
	}
	s.audit(r.Context(), r, "delete_node_category", "node_category", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PUT /api/nodes/{id}/categories —— 覆盖式设置某个节点的分类。
//
// 用**覆盖**而不是增删两个接口：界面上是勾选框，
// 提交的就是"最终应该是什么样"。分成 add/remove 会让
// "勾选后网络失败重试"变成不确定的结果（重复 add 幂等，但漏掉的 remove 不会自己补上）。
func (s *Server) nodeSetCategories(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	nodeID, ok := canonicalUUID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	var input struct {
		CategoryIDs []string `json:"category_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 去重：前端可能因为重复点击提交同一 id 两次，
	// 而 (node_id, category_id) 是主键，重复插入会报错。
	//
	// 2026-11-04：先规范成小写标准 UUID 再去重 / 比较。下面用「改动前」（库里 ::text，恒为小写）与
	// 本次提交比较算出被移出的分类：原来原样比较，提交同一分类的大写形式（API / Telegram 调用）时，
	// 节点仍在分类里，却被判成「移出」而回收了该分类订阅在这台节点上的全部买家账号。
	seen := map[string]bool{}
	ids := make([]string, 0, len(input.CategoryIDs))
	for _, raw := range input.CategoryIDs {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if !uuidPattern.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "分类 ID 格式不正确"})
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}

	tx, err := s.db.Begin(r.Context())
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())

	var exists bool
	if err := tx.QueryRow(r.Context(),
		`SELECT true FROM hysteria_nodes WHERE id = $1::uuid AND deleted_at IS NULL`, nodeID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	} else if err != nil {
		s.databaseError(w, r, err)
		return
	}

	// 记下改动前的分类：被移出的分类要回收账号，新加入的分类要补开户。
	var before []string
	if err := tx.QueryRow(r.Context(),
		`SELECT COALESCE(array_agg(category_id::text), '{}') FROM node_category_members WHERE node_id = $1::uuid`, nodeID).Scan(&before); err != nil {
		s.databaseError(w, r, err)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM node_category_members WHERE node_id = $1::uuid`, nodeID); err != nil {
		s.databaseError(w, r, err)
		return
	}
	for _, cid := range ids {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO node_category_members (node_id, category_id)
			 VALUES ($1::uuid, $2::uuid)
			 ON CONFLICT DO NOTHING`, nodeID, cid); err != nil {
			// 分类 id 不存在时外键会报错 —— 转成可读信息而不是 500。
			// 2026-11-06：只有外键冲突（23503）才是「分类不存在」；连接断开等数据库错误照实回 500，
			// 不能让运维去核对一个其实存在的分类。
			if isForeignKeyViolation(err) {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "分类 " + cid + " 不存在",
				})
				return
			}
			s.databaseError(w, r, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.databaseError(w, r, err)
		return
	}
	InvalidateSubscriptionCache()

	// 2026-11-03：订阅按分类实时圈定节点，所以移出分类后新拉的订阅里就没有这台了；
	// 可是买家在这台节点上的账号还开着，已导入的配置照样能连。与「下架」同理，
	// 必须把这些分类订阅在这台节点上的账号回收；新加入分类则立即补开户（原来要等每小时一次的补开）。
	now := map[string]bool{}
	for _, id := range ids {
		now[id] = true
	}
	var removed []string
	added := false
	for _, id := range before {
		if !now[id] {
			removed = append(removed, id)
		}
	}
	prev := map[string]bool{}
	for _, id := range before {
		prev[id] = true
	}
	for _, id := range ids {
		if !prev[id] {
			added = true
		}
	}
	if added {
		s.queueProvisioningGapsAsync("node_categories_changed")
	}
	resp := map[string]any{"ok": true, "category_ids": ids}
	if len(removed) > 0 {
		outcome := s.revokeNodeFromSubscriptions(r.Context(), nodeID, nil, removed, false)
		resp["revoked_count"] = outcome.Revoked
		if outcome.Err != nil || len(outcome.Failed) > 0 {
			msg := "分类已更新，但无法确认被移出分类的订阅账号已从该节点回收"
			if outcome.Err == nil {
				msg = fmt.Sprintf("分类已更新，但有 %d 个订阅账号未能从该节点注销，映射已保留可重试: %s",
					len(outcome.Failed), outcome.FailedSummary())
			}
			s.audit(r.Context(), r, "set_node_categories", "hysteria_node", nodeID,
				map[string]any{"category_ids": ids, "revoke_failed": len(outcome.Failed)})
			resp["ok"] = false
			resp["status"] = "updated_with_revoke_failures"
			resp["failed_count"] = len(outcome.Failed)
			resp["error"] = msg
			writeJSON(w, http.StatusMultiStatus, resp)
			return
		}
	}
	s.audit(r.Context(), r, "set_node_categories", "hysteria_node", nodeID,
		map[string]any{"category_ids": ids})
	writeJSON(w, http.StatusOK, resp)
}

// resolveCategoryBySlug 供商城按 slug 下单时使用。
// 返回 (id, enabled, true) 或 ("", false, false)。
//
// 停用的语义是「不再用于新订单」，不是「让老客户断线」：已有订阅保留 category_id，
// 照常按分类下发。这里只服务于**新**订单（同一 order_id 的重复请求在调用前就已经返回），
// 所以调用方必须拒绝停用的分类。
//
// 2026-11-03：原来停用的分类照样解析成功，前端提示「商城新订单不能再选它」却从未生效。
// 2026-11-04：查询失败作为 error 返回。原来一律当成「不存在」：数据库抖一下，商城就收到
// 400「分类不存在，请先建好」—— 一个可重试的临时故障被报成永久的配置错误。
func (s *Server) resolveCategoryBySlug(ctx context.Context, slug string) (string, bool, bool, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return "", false, false, nil
	}
	var id string
	var enabled bool
	if err := s.db.QueryRow(ctx,
		`SELECT id, enabled FROM node_categories WHERE slug = $1`, slug).Scan(&id, &enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	return id, enabled, true, nil
}

// categoryExists 判断 id 是否存在（新建订阅时校验用）。
func (s *Server) categoryExists(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, nil
	}
	var ok bool
	err := s.db.QueryRow(ctx,
		`SELECT true FROM node_categories WHERE id = $1::uuid`, id).Scan(&ok)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// categoryNameOf 从 id→name 映射里安全取值。
//
// 为什么单独写：订阅的 CategoryID 是 *string，nil 表示「未圈定」。
// 直接 *id 取映射会 panic；而「未圈定」应当得到空名称
// （前端据此显示「全部节点」而不是一个空白）。
func categoryNameOf(m map[string]string, id *string) string {
	if id == nil || *id == "" {
		return ""
	}
	return m[*id]
}
