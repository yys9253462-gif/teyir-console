package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// portalusers.go drives the node-side portal's user management REST API.
//
// These endpoints are Bearer-authenticated (the same api_key the panel already
// stores to read node/meta), so no additional credential is required. They are
// answered synchronously rather than through the task queue: the portal itself
// is a local HTTP service on the node, the round trip is sub-second, and the
// caller needs the generated password / URI in the response to hand to a
// customer. Queueing would make the operator poll a task row for a value that
// has to be shown once and never again.
//
// Every mutation is audited. The `password` and full subscription URIs are
// deliberately NOT written to the audit row — the audit table is admin-readable
// and a subscription URI is a ready-to-use credential.

// portalUserIDPattern mirrors VALID_USER_ID_RE in portal.py. The id is
// interpolated into the request path, so this is a security boundary, not a
// cosmetic check: anything outside this set is refused before a request is built.
var portalUserIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// portalUser is one row of the portal's user list. `used_bytes` and
// `limit_bytes` come from the portal's own bookkeeping, which only advances when
// a client re-authenticates — never present them as a live traffic rate.
type portalUser struct {
	UserID               string          `json:"user_id"`
	OnlineDevices        *int64          `json:"online_devices"`
	MeasuredBytes        *int64          `json:"measured_bytes"`
	MeasurementOK        bool            `json:"measurement_ok"`
	MeasurementStartedAt string          `json:"measurement_started_at"`
	MeasurementSources   json.RawMessage `json:"measurement_sources"`
	MeasurementGaps      json.RawMessage `json:"measurement_gaps"`
	ExpiresAt            int64           `json:"expires_at"`
	Active               bool            `json:"active"`
	LimitBytes           int64           `json:"traffic_limit_bytes"`
	UsedBytes            int64           `json:"traffic_used_bytes"`
	IPLimit              int             `json:"ip_limit"`
	CreatedAt            int64           `json:"created_at"`
	Status               string          `json:"status"`
}

// portalUsersListResponse is the GET /api/v1/users/list envelope.
type portalUsersListResponse struct {
	OK    bool         `json:"ok"`
	Count int          `json:"count"`
	Users []portalUser `json:"users"`
	// 新版 portal 只在信封里放一次节点级 measurement_gaps（见 collector/usage.go 同名字段）。
	MeasurementGaps json.RawMessage `json:"measurement_gaps"`
	// measurement_sources 同理：节点门户只在信封里放一次（2026-10-30）。
	MeasurementSources json.RawMessage `json:"measurement_sources"`
}

// portalUserCreateResponse is the POST /api/v1/users/create envelope. The three
// subscription artifacts (uri / clash / sing_box) are returned verbatim so the
// operator can copy or download them straight from the panel.
type portalUserCreateResponse struct {
	OK       bool   `json:"ok"`
	UserID   string `json:"user_id"`
	Password string `json:"password"`
	IPLimit  int    `json:"ip_limit"`
	// TrafficGB is a float on the wire. portal.py computes it as
	// `float(params.get('traffic_gb', 0))`, so a plain 0 serialises as `0.0`,
	// and decoding `0.0` into an int fails the whole response — which took out
	// provisioning entirely (the node reported ok, we called it unparseable, and
	// the buyer silently fell back to the shared password). flexInt accepts
	// either shape.
	TrafficGB  flexInt `json:"traffic_gb"`
	ExpiresAt  int64   `json:"expires_at"`
	URI        string  `json:"uri"`
	RealityURI string  `json:"reality_uri"`
	Clash      string  `json:"clash"`
	SingBox    string  `json:"sing_box"`
}

// flexInt decodes a JSON number that the peer may send as either an integer or
// a float. Node-side code is Python, where every number is a float if it went
// through `float()`; Go's encoding/json is strict about the distinction.
//
// Non-numeric input is tolerated as zero rather than an error: a quota field is
// never the reason to fail an otherwise-good provisioning response.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(v)
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(int(v))
		return nil
	}
	*f = 0
	return nil
}

// portalUserRenewResponse is the POST /api/v1/users/renew envelope.
type portalUserRenewResponse struct {
	OK         bool   `json:"ok"`
	UserID     string `json:"user_id"`
	ExpiresAt  int64  `json:"expires_at"`
	LimitBytes int64  `json:"limit_bytes"`
}

// GET /api/nodes/{id}/users — list users from the node's portal.
func (s *Server) portalUsers(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceReadLimit(w, claims.Subject) {
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}
	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodGet, "/users/list", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed portalUsersListResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的用户列表无法解析"})
		return
	}
	// Normalise a nil slice to an empty one so the UI renders "0 users" rather
	// than treating null as an error.
	if parsed.Users == nil {
		parsed.Users = []portalUser{}
	}
	// 前端按用户显示计量缺口：新版 portal 不再逐用户复制，这里补回去，界面不用改。
	if len(parsed.MeasurementGaps) > 0 {
		for i := range parsed.Users {
			if len(parsed.Users[i].MeasurementGaps) == 0 {
				parsed.Users[i].MeasurementGaps = parsed.MeasurementGaps
			}
		}
	}
	if own := string(parsed.MeasurementSources); own != "" && own != "{}" && own != "null" {
		for i := range parsed.Users {
			mine := strings.TrimSpace(string(parsed.Users[i].MeasurementSources))
			if mine == "" || mine == "{}" || mine == "null" {
				parsed.Users[i].MeasurementSources = parsed.MeasurementSources
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":   target.NodeID,
		"node_name": target.NodeName,
		"count":     len(parsed.Users),
		"users":     parsed.Users,
	})
}

// POST /api/nodes/{id}/users — create a user on the node's portal.
func (s *Server) portalCreateUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var input struct {
		UserID      string `json:"user_id"`
		DurationDay int    `json:"duration_days"`
		IPLimit     int    `json:"ip_limit"`
		TrafficGB   int    `json:"traffic_gb"`
		Note        string `json:"note"`
		// Password 可选：指定就转发给节点，不指定则由节点随机生成。
		//
		// ⚠️ 2026-10-03 修的真 BUG：这个字段**原来根本不存在**，
		// 于是调用方传的 password 被**静默丢弃**，节点侧收到没有 password 的
		// 请求后每次 `secrets.token_hex(16)` 随机生成一个新密码。
		//
		// 后果（实测）：任何"再创建一次"的操作都会让节点上的密码变掉，
		// 而面板库里记的还是旧值 → 客户端报
		// `authentication error, HTTP status code: 200`。
		// 更糟的是它**看起来像成功的**：create 返回 200 + 一个新密码。
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	userID := strings.TrimSpace(input.UserID)
	// An omitted user_id is legitimate: the portal generates one. But a supplied
	// one must be safe to put in a URL path.
	if userID != "" && (userID == "." || userID == ".." || !portalUserIDPattern.MatchString(userID)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户 ID 只能是 1-64 位的字母、数字、下划线、短横线或点，且不能为 . 或 .."})
		return
	}
	// 2026-11-03：手工开户不能落到订阅账号（sub_*）或机主账号上。节点的 /users/create 对已存在的
	// user_id 是覆盖语义：不带密码就随机换一个新密码，而 node_accounts 里记的还是旧值 ⇒ 买家客户端
	// 立刻 authentication error。订阅账号只由开户 / 对账流程（确定性密码）管理。
	if strings.HasPrefix(userID, "sub_") || userID == "admin_master" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sub_ 开头的是订阅账号、admin_master 是机主账号，不能在这里手工创建或覆盖；订阅账号请用「账号对账」"})
		return
	}
	if input.DurationDay < 0 || input.IPLimit < 0 || input.TrafficGB < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "有效期、IP 限制与流量配额不能为负数"})
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}

	payload := map[string]any{}
	if userID != "" {
		payload["user_id"] = userID
	}
	// ⚠️ password 必须转发。不传它的话，节点侧
	//     pwd = params.get('password') or secrets.token_hex(16)
	// 会在**每次创建时随机生成新密码**，而 /users/create 对已存在 user_id
	// 是覆盖语义 —— 重复创建就把节点上的密码换掉，面板库里记的旧值随即失效。
	// 这是 2026-10-03 那个"节点认证失败"的直接成因。
	if pwd := strings.TrimSpace(input.Password); pwd != "" {
		payload["password"] = pwd
	}
	if input.DurationDay > 0 {
		payload["duration_days"] = input.DurationDay
	}
	if input.IPLimit > 0 {
		payload["ip_limit"] = input.IPLimit
	}
	if input.TrafficGB > 0 {
		payload["traffic_gb"] = input.TrafficGB
	}
	if note := strings.TrimSpace(input.Note); note != "" {
		payload["note"] = note
	}

	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/users/create", payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed portalUserCreateResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的创建结果无法解析"})
		return
	}
	if parsed.UserID == "" || parsed.Password == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点未返回新用户的口令，请在 portal 上确认"})
		return
	}
	s.refreshNodeUsersCount(target)
	s.audit(r.Context(), r, "create_node_user", "hysteria_node", target.NodeID, map[string]any{
		"user_id":       parsed.UserID,
		"duration_days": input.DurationDay,
		"traffic_gb":    input.TrafficGB,
		"ip_limit":      input.IPLimit,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"node_id":     target.NodeID,
		"user_id":     parsed.UserID,
		"password":    parsed.Password,
		"expires_at":  parsed.ExpiresAt,
		"ip_limit":    parsed.IPLimit,
		"traffic_gb":  parsed.TrafficGB,
		"uri":         parsed.URI,
		"reality_uri": parsed.RealityURI,
		"clash":       parsed.Clash,
		"sing_box":    parsed.SingBox,
	})
}

// POST /api/nodes/{id}/users/{uid}/renew — extend a user's expiry and/or quota.
func (s *Server) portalRenewUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	userID, ok := s.portalUserIDFromPath(w, r)
	if !ok {
		return
	}
	if refuseManagedNodeAccount(w, userID, "续期") {
		return
	}
	var input struct {
		ExpiresAt    *int64 `json:"expires_at"`
		ExtendDays   int    `json:"extend_days"`
		AddTrafficGB int    `json:"add_traffic_gb"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if input.ExtendDays < 0 || input.AddTrafficGB < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "续期天数与流量不能为负数"})
		return
	}
	if input.ExpiresAt != nil && (*input.ExpiresAt <= time.Now().Unix() || input.ExtendDays != 0 || input.AddTrafficGB != 0) {
		writeJSON(w, 400, map[string]string{"error": "绝对续期必须为未来时间且不能混用相对天数或流量"})
		return
	}
	if input.ExpiresAt == nil && input.ExtendDays == 0 && input.AddTrafficGB == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请至少填写续期天数或新增流量"})
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}

	if input.ExpiresAt != nil {
		// 2026-11-03：只改到期时间，不动启用状态。原来固定带 "status":"active"，
		// 对一个被「停用订阅」停掉的账号续期就会把它重新启用 —— 订阅仍是停用，客户却又能连了。
		envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/users/update", map[string]any{"user_id": userID, "expires_at": *input.ExpiresAt})
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		var result struct {
			OK   bool `json:"ok"`
			User struct {
				ExpiresAt  int64 `json:"expires_at"`
				LimitBytes int64 `json:"traffic_limit_bytes"`
			} `json:"user"`
		}
		if json.Unmarshal(envelope.Data, &result) != nil || !result.OK || result.User.ExpiresAt != *input.ExpiresAt {
			writeJSON(w, 502, map[string]string{"error": "节点未确认绝对续期目标"})
			return
		}
		s.audit(r.Context(), r, "renew_node_user_absolute", "hysteria_node", target.NodeID, map[string]any{"user_id": userID, "expires_at": *input.ExpiresAt})
		writeJSON(w, 200, map[string]any{"node_id": target.NodeID, "user_id": userID, "expires_at": result.User.ExpiresAt, "limit_bytes": result.User.LimitBytes})
		return
	}
	payload := map[string]any{"user_id": userID}
	if input.ExtendDays > 0 {
		payload["extend_days"] = input.ExtendDays
	}
	if input.AddTrafficGB > 0 {
		payload["add_traffic_gb"] = input.AddTrafficGB
	}

	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/users/renew", payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed portalUserRenewResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的续期结果无法解析"})
		return
	}
	if !parsed.OK || parsed.UserID != userID || parsed.ExpiresAt <= 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点未确认目标用户的续期结果"})
		return
	}
	s.audit(r.Context(), r, "renew_node_user", "hysteria_node", target.NodeID, map[string]any{
		"user_id":        userID,
		"extend_days":    input.ExtendDays,
		"add_traffic_gb": input.AddTrafficGB,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":     target.NodeID,
		"user_id":     userID,
		"expires_at":  parsed.ExpiresAt,
		"limit_bytes": parsed.LimitBytes,
	})
}

// POST /api/nodes/{id}/users/{uid}/status — 停用/启用节点上的一个账号。
//
// 为什么需要它（2026-10-03 管理员实测反馈）
// -------------------------------------
// 面板点「停用订阅」后，**已拉取到配置的客户端仍然能连** ——
// 实测四个节点 4/4 全部仍连通。
//
// 根因：「停用」只挡住了订阅下发端点（/sub/{token} 返回 403），
// 而客户端每次连接都要去**节点的鉴权回调**重新认证，
// 节点上那个账号仍然是 active ⇒ 认证照常通过。
// 换句话说原来「停用」的语义只是「不再向新客户端发放配置」。
//
// 现在有了本端点，「停用订阅」可以联动把节点账号置为 disabled，
// 实现**立刻断连且可恢复**（与 delete 的关键区别：delete 不可逆、
// 且会清零已用流量；本操作只改 status 一个字段）。
//
// 与 delete 采用同一条危险操作保护：需要 X-Confirm-Dangerous: yes。
// 理由相同 —— 它会立刻影响正在使用的客户。
func (s *Server) portalSetUserStatus(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	// 与删除同一级别的保护：这会让正在用的客户端立刻掉线，
	// 不该因为一次误点就发生。
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{
			"error": "停用/启用会立刻影响正在使用的客户端，请带 X-Confirm-Dangerous: yes 确认",
		})
		return
	}
	userID, ok := s.portalUserIDFromPath(w, r)
	if !ok {
		return
	}
	if refuseManagedNodeAccount(w, userID, "停用或启用") {
		return
	}
	var input struct {
		// 用指针区分「没传」与「传了 false」——
		// 传 false 是"停用"，漏传则是调用方写错了，必须分开处理。
		Active *bool `json:"active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	if input.Active == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 active 字段"})
		return
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}

	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost,
		"/users/set_status", map[string]any{"user_id": userID, "active": *input.Active})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var parsed struct {
		OK      bool   `json:"ok"`
		UserID  string `json:"user_id"`
		Status  string `json:"status"`
		Changed bool   `json:"changed"`
	}
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点返回的状态变更结果无法解析"})
		return
	}
	want := "disabled"
	if *input.Active {
		want = "active"
	}
	if !parsed.OK || parsed.Status != want {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "节点未确认目标状态：" + want})
		return
	}
	s.audit(r.Context(), r, "set_node_user_status", "hysteria_node", target.NodeID, map[string]any{
		"user_id":   userID,
		"active":    *input.Active,
		"new_state": parsed.Status,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": target.NodeID,
		"user_id": userID,
		"status":  parsed.Status,
		// changed=false 表示本来就是该状态（幂等）——
		// 调用方据此区分"真的改了"与"重复调用"，
		// 这在"停用订阅"的联动里很重要：重复停用不该被当成失败。
		"changed": parsed.Changed,
	})
}

// DELETE /api/nodes/{id}/users/{uid} — remove a user from the node's portal.
func (s *Server) portalDeleteUser(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if r.Header.Get("X-Confirm-Dangerous") != "yes" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "删除节点用户需要 X-Confirm-Dangerous: yes"})
		return
	}
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	userID, ok := s.portalUserIDFromPath(w, r)
	if !ok {
		return
	}
	// 2026-11-04：机主账号不能删（页面早就禁用了这个按钮，接口却照删 —— 删掉它节点就失去自己的主账号）。
	// 仍被在用订阅映射着的订阅账号也不能在这里删：映射还在，买家客户端立刻认证失败，
	// 而下发、回收、对账都按映射走，面板以为账号还在。没有订阅再引用的残留 sub_* 账号照常可删（孤儿清理要用）。
	if userID == "admin_master" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "admin_master 是机主账号，不能删除"})
		return
	}
	if strings.HasPrefix(userID, "sub_") {
		// 2026-11-06：映射的键是小写标准 UUID。原来用路径里的原样 id 去查：大写 / 花括号写法查不到映射，
		// 检查被绕过，而 loadPortalTarget 的 ::uuid 照样认 —— 在用订阅的账号被删掉。
		nodeKey, ok := canonicalUUID(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
			return
		}
		var mapped bool
		if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM subscription_tokens
			WHERE deleted_at IS NULL AND node_accounts->($1::text)->>'user_id' = $2)`, nodeKey, userID).Scan(&mapped); err != nil {
			s.databaseError(w, r, err)
			return
		}
		if mapped {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "这个账号属于一条在用的订阅；请在订阅中心停用或删除该订阅，不要直接删节点账号"})
			return
		}
	}
	target, err := s.loadPortalTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writePortalTargetError(w, err)
		return
	}
	envelope, err := portalCall(r.Context(), target.BaseURL, target.APIKey, http.MethodPost, "/users/delete", map[string]any{"user_id": userID})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	_ = envelope
	s.refreshNodeUsersCount(target)
	s.audit(r.Context(), r, "delete_node_user", "hysteria_node", target.NodeID, map[string]any{"user_id": userID})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "user_id": userID})
}

// refuseManagedNodeAccount 拒绝在「节点用户」页直接改订阅账号（sub_*）与机主账号（2026-11-04）。
//
// 订阅账号的到期、启用状态由订阅流程统一管理（续费 / 停用 / 到期回收都会联动各节点）：
//   - 在这里给 sub_* 续期，会把节点侧到期时间推到订阅之外 —— 订阅到期回收后账号仍然能用；
//   - 在这里「启用」一个已停用订阅的账号，买家又能连上，而面板认为订阅是停用的。
//
// 开户与删除早就拒绝了这两类账号，续期与停用/启用原来漏了。
func refuseManagedNodeAccount(w http.ResponseWriter, userID, action string) bool {
	if userID == "admin_master" || strings.HasPrefix(userID, "sub_") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "admin_master 是机主账号、sub_ 开头的是订阅账号，不能在这里" + action + "；订阅账号请在订阅中心续费、停用或启用"})
		return true
	}
	return false
}

// portalUserIDFromPath extracts and validates the {uid} path segment, writing a
// 400 and returning false when it is unusable.
func (s *Server) portalUserIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	userID := strings.TrimSpace(r.PathValue("uid"))
	if userID == "" || userID == "." || userID == ".." || !portalUserIDPattern.MatchString(userID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户 ID 只能是 1-64 位的字母、数字、下划线、短横线或点，且不能为 . 或 .."})
		return "", false
	}
	return userID, true
}

// refreshNodeUsersCount keeps the cached users_count on the node row honest after
// a mutation. It is best-effort: a failure here must not fail an operation that
// already succeeded on the node, so the error is logged and dropped.
func (s *Server) refreshNodeUsersCount(target *portalTarget) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	envelope, err := portalCall(ctx, target.BaseURL, target.APIKey, http.MethodGet, "/users/list", nil)
	if err != nil {
		s.logger.Warn("node portal: refresh users count failed", "node_id", target.NodeID, "error", err)
		return
	}
	var parsed portalUsersListResponse
	if err := json.Unmarshal(envelope.Data, &parsed); err != nil {
		s.logger.Warn("node portal: parse users count failed", "node_id", target.NodeID, "error", err)
		return
	}
	if _, err := s.db.Exec(ctx, `UPDATE hysteria_nodes SET users_count=$1, api_checked_at=now(), updated_at=now() WHERE id=$2::uuid AND deleted_at IS NULL`, len(parsed.Users), target.NodeID); err != nil {
		s.logger.Warn("node portal: store users count failed", "node_id", target.NodeID, "error", err)
	}
}
