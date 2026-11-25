package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"nodeops/server/internal/ippurity"
)

// IP 检测密钥的面板设置（2026-10-15）。
//
// 存 system_settings，值是 secrets.Box 加密后的 base64 —— 与主机 SSH 凭据同一把钥匙，
// 数据库被拖走也拿不到明文。GET 永远不回显明文，只给「是否配置 / 来源 / 末四位」。
// 保存后写进 KeyStore，provider 下一次查询就用新值，无需重启。

var purityKeySettings = map[string]string{
	ippurity.KeyIPAPIIs:    "ip_purity_key_ipapi_is",
	ippurity.KeyIPQS:       "ip_purity_key_ipqs",
	ippurity.KeyCloudflare: "integration_key_cloudflare",
	ippurity.KeyTelegram:   "integration_key_telegram",
}

// 允许冒号：Telegram Bot Token 形如 123456:ABC-def。
var purityKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// LoadPurityKeys 启动时把库里保存的密钥载入 KeyStore。
func (s *Server) LoadPurityKeys(ctx context.Context) {
	if s.purityKeys == nil || s.box == nil {
		return
	}
	for name, settingKey := range purityKeySettings {
		var value string
		if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, settingKey).Scan(&value); err != nil || value == "" {
			continue
		}
		cipher, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			s.logger.Warn("purity key: stored value is not base64", "key", settingKey)
			continue
		}
		plain, err := s.box.Open(cipher)
		if err != nil {
			s.logger.Warn("purity key: decrypt failed", "key", settingKey)
			continue
		}
		s.purityKeys.SetWeb(name, string(plain))
		clear(plain)
	}
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

func (s *Server) purityKeysGet(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for name := range purityKeySettings {
		key := s.purityKeys.Get(name)
		entry := map[string]any{"configured": key != "", "source": s.purityKeys.Source(name)}
		if key != "" {
			entry["masked"] = maskKey(key)
		}
		out[name] = entry
	}
	writeJSON(w, http.StatusOK, out)
}

// PUT body: {"ipapi_is": "...", "ipqs": "..."}。
// 字段缺省 = 不改；空串 = 清除面板里的值（回落到环境变量）。
func (s *Server) purityKeysPut(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	if s.purityKeys == nil || s.box == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "未配置加密密钥，无法安全保存"})
		return
	}
	var input map[string]*string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	// 2026-11-04：先校验、加密全部字段，再在一个事务里写库。原来逐个字段「校验通过就写」，
	// 而 map 的遍历顺序是随机的：同时提交两个密钥、其中一个格式不对时，另一个可能已经存进库并生效，
	// 接口却回 400，界面仍显示它「未配置」。
	type pendingKey struct{ name, settingKey, value, stored string }
	var pending []pendingKey
	for name, ptr := range input {
		settingKey, ok := purityKeySettings[name]
		if !ok || ptr == nil {
			continue
		}
		value := strings.TrimSpace(*ptr)
		if value != "" && !purityKeyPattern.MatchString(value) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": name + " 的密钥格式不对：只能是 8-128 位字母、数字、- 或 _（本次没有保存任何密钥）"})
			return
		}
		stored := ""
		if value != "" {
			cipher, err := s.box.Seal([]byte(value))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "加密失败"})
				return
			}
			stored = base64.StdEncoding.EncodeToString(cipher)
		}
		pending = append(pending, pendingKey{name, settingKey, value, stored})
	}
	changed := map[string]any{}
	if len(pending) > 0 {
		tx, err := s.db.Begin(r.Context())
		if err != nil {
			s.databaseError(w, r, err)
			return
		}
		defer tx.Rollback(r.Context())
		for _, p := range pending {
			if _, err := tx.Exec(r.Context(), `
INSERT INTO system_settings (key, value, description) VALUES ($1, $2, 'IP 检测第三方密钥（加密）')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, p.settingKey, p.stored); err != nil {
				s.databaseError(w, r, err)
				return
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			s.databaseError(w, r, err)
			return
		}
		for _, p := range pending {
			s.purityKeys.SetWeb(p.name, p.value)
			if p.value == "" {
				changed[p.name] = "cleared"
			} else {
				changed[p.name] = "set"
			}
		}
	}
	s.audit(r.Context(), r, "set_purity_keys", "system_setting", "ip_purity_keys", changed)
	s.purityKeysGet(w, r)
}
