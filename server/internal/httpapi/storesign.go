package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 商城签名v2绑定时间戳、方法、解码后的路径、规范化查询串、原始请求体哈希和独立nonce。
// msg = ts + "\n" + METHOD + "\n" + PATH_QUERY + "\n" + hex(sha256(body)) + "\n" + nonce
// PATH_QUERY 使用URL.Query().Encode()规范化查询；nonce为每次请求独立随机的32-64位十六进制值。
// 签名有效期±5分钟，nonce跨进程持久化防重放。v1仅在显式迁移窗口内兼容。

const (
	storeRequireSignatureKey = "store_require_signature"
	storeSignatureSkew       = 5 * time.Minute
	storeNonceTTL            = 10 * time.Minute
	storeMaxBodyBytes        = 1 << 20
	storeNonceCacheLimit     = 16384
)

var storeNonceHexPattern = regexp.MustCompile(`^[a-fA-F0-9]{32,64}$`)

type storeNonceEntry struct {
	key string
	exp time.Time
}

var storeNonces = struct {
	sync.Mutex
	seen  map[string]time.Time
	order []storeNonceEntry
	head  int
}{seen: map[string]time.Time{}}

// pruneExpiredLocked 按插入顺序从队头均摊 O(1) 弹出已过期的重放键，避免每次验签持全局锁全表扫描。
func pruneStoreNoncesLocked(now time.Time) {
	for storeNonces.head < len(storeNonces.order) {
		front := storeNonces.order[storeNonces.head]
		curExp, ok := storeNonces.seen[front.key]
		if !ok || !curExp.Equal(front.exp) {
			storeNonces.head++
			continue
		}
		if !now.After(front.exp) {
			break
		}
		delete(storeNonces.seen, front.key)
		storeNonces.head++
	}
	if storeNonces.head >= 1024 && storeNonces.head*2 >= len(storeNonces.order) {
		remaining := make([]storeNonceEntry, len(storeNonces.order)-storeNonces.head)
		copy(remaining, storeNonces.order[storeNonces.head:])
		storeNonces.order = remaining
		storeNonces.head = 0
	}
}

func storeReplayCacheKey(secret, raw string) string {
	sum := sha256.Sum256([]byte(secret + "\x00" + raw))
	return hex.EncodeToString(sum[:])
}

func storeSignatureMessage(ts, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	return ts + "\n" + strings.ToUpper(method) + "\n" + path + "\n" + hex.EncodeToString(sum[:])
}

func storeSign(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyStoreSignature：signed=false 表示请求没带签名头；err 非空表示带了但不合法。
// 会读出请求体并放回去，后面的处理器照常解析。
func verifyStoreSignature(r *http.Request, secret string, now time.Time) (signed bool, err error) {
	sig := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Store-Signature")))
	tsRaw := strings.TrimSpace(r.Header.Get("X-Store-Timestamp"))
	if sig == "" && tsRaw == "" {
		return false, nil
	}
	if sig == "" || tsRaw == "" {
		return true, errors.New("签名头不完整：需要同时带 X-Store-Timestamp 与 X-Store-Signature")
	}
	ts, perr := strconv.ParseInt(tsRaw, 10, 64)
	if perr != nil {
		return true, errors.New("X-Store-Timestamp 必须是 Unix 秒")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > storeSignatureSkew || d < -storeSignatureSkew {
		return true, errors.New("请求时间戳超出 ±5 分钟（检查商城服务器时间）")
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, storeMaxBodyBytes+1))
		if err != nil {
			return true, errors.New("读取请求体失败")
		}
		if len(body) > storeMaxBodyBytes {
			return true, errors.New("请求体超过 1 MiB 限制")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	message := storeSignatureMessage(tsRaw, r.Method, r.URL.Path, body)
	replayKey := storeReplayCacheKey(secret, "v1:"+sig)
	if r.Header.Get("X-Store-Signature-Version") == "2" {
		nonce := r.Header.Get("X-Store-Nonce")
		if !storeNonceHexPattern.MatchString(nonce) {
			return true, errors.New("签名v2缺少有效的独立nonce")
		}
		message = storeSignatureMessageV2(tsRaw, r.Method, r.URL.Path, r.URL.Query().Encode(), nonce, body)
		replayKey = storeReplayCacheKey(secret, "v2:"+strings.ToLower(nonce))
	}
	want := storeSign(secret, message)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return true, errors.New("签名不匹配")
	}
	storeNonces.Lock()
	defer storeNonces.Unlock()
	pruneStoreNoncesLocked(now)
	if exp, used := storeNonces.seen[replayKey]; used {
		if now.After(exp) {
			delete(storeNonces.seen, replayKey)
		} else {
			return true, errors.New("签名已被使用过（疑似重放）")
		}
	}
	for len(storeNonces.seen) >= storeNonceCacheLimit {
		// 极端流量下从队头均摊 O(1) 淘汰最早过期的条目，避免进程内存无界增长。
		if storeNonces.head < len(storeNonces.order) {
			front := storeNonces.order[storeNonces.head]
			storeNonces.head++
			if curExp, ok := storeNonces.seen[front.key]; ok && curExp.Equal(front.exp) {
				delete(storeNonces.seen, front.key)
			}
			continue
		}
		// 兜底：外部测试直接向 seen 注入条目而未走队列时，退化为扫描淘汰一条。
		var oldestKey string
		var oldestExp time.Time
		for k, exp := range storeNonces.seen {
			if oldestKey == "" || exp.Before(oldestExp) {
				oldestKey, oldestExp = k, exp
			}
		}
		if oldestKey == "" {
			break
		}
		delete(storeNonces.seen, oldestKey)
	}
	exp := now.Add(storeNonceTTL)
	storeNonces.seen[replayKey] = exp
	storeNonces.order = append(storeNonces.order, storeNonceEntry{key: replayKey, exp: exp})
	return true, nil
}

// forgetStoreReplayKey 撤掉 verifyStoreSignature 记下的进程内重放标记（键的算法与那里一致）。
func forgetStoreReplayKey(r *http.Request, secret string) {
	key := storeReplayCacheKey(secret, "v1:"+strings.ToLower(strings.TrimSpace(r.Header.Get("X-Store-Signature"))))
	if r.Header.Get("X-Store-Signature-Version") == "2" {
		key = storeReplayCacheKey(secret, "v2:"+strings.ToLower(strings.TrimSpace(r.Header.Get("X-Store-Nonce"))))
	}
	storeNonces.Lock()
	delete(storeNonces.seen, key)
	storeNonces.Unlock()
}

func (s *Server) storeSignatureRequired(r *http.Request) (bool, error) {
	var v string
	if err := s.db.QueryRow(r.Context(), `SELECT value FROM system_settings WHERE key=$1`, storeRequireSignatureKey).Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(v) == "1", nil
}

// GET /api/settings/store-signature
func (s *Server) getStoreSignature(w http.ResponseWriter, r *http.Request) {
	required, err := s.storeSignatureRequired(r)
	if err != nil {
		s.databaseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"required":     required,
		"algorithm":    "HMAC-SHA256(key, ts + newline + METHOD + newline + PATH_CANONICAL_QUERY + newline + SHA256(body) + newline + nonce)",
		"version":      2,
		"headers":      []string{"X-Store-Key", "X-Store-Timestamp", "X-Store-Signature-Version", "X-Store-Nonce", "X-Store-Signature"},
		"skew_seconds": int(storeSignatureSkew.Seconds()),
	})
}

// PUT /api/settings/store-signature  {"required": true}
func (s *Server) setStoreSignature(w http.ResponseWriter, r *http.Request) {
	claims, _ := claimsFrom(r.Context())
	if !s.enforceWriteLimit(w, claims.Subject) {
		return
	}
	var in struct {
		Required bool `json:"required"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}
	v := "0"
	if in.Required {
		v = "1"
	}
	// ⚠️ 2026-10-31：这是一条**安全控制**，写失败绝不能回 200。
	// 原来 `s.putSetting(...)` 的返回值被丢掉：数据库抖一下，接口照样返回成功，
	// 运营者以为签名已强制，实际仍是关着的 —— 商城那边的未签名请求继续被接受。
	// 先落库、后审计、最后才回读展示。
	if err := s.putSetting(r.Context(), storeRequireSignatureKey, v, "商城接口是否强制签名（1 是 / 0 否）"); err != nil {
		s.logger.Error("store: 保存签名开关失败", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存失败，签名开关未改变，请查看服务端日志"})
		return
	}
	s.audit(r.Context(), r, "set_store_signature", "system_setting", storeRequireSignatureKey, map[string]any{"required": in.Required})
	s.getStoreSignature(w, r)
}

// v2绑定完整查询参数与独立随机nonce；相同秒内的不同请求不再碰撞。
func storeSignatureMessageV2(ts, method, path, query, nonce string, body []byte) string {
	if query != "" {
		path += "?" + query
	}
	return storeSignatureMessage(ts, method, path, body) + "\n" + nonce
}
func (s *Server) acceptStoreProtocol(r *http.Request) (bool, error) {
	if r.Header.Get("X-Store-Signature-Version") == "2" {
		return true, nil
	}
	var raw string
	err := s.db.QueryRow(r.Context(), `SELECT value FROM system_settings WHERE key='store_signature_legacy_until'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	until, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		if s.logger != nil {
			s.logger.Warn("store: store_signature_legacy_until 配置非整数，已按关闭兼容处理", "value", raw)
		}
		return false, nil
	}
	return time.Now().Unix() < until, nil
}
// errStoreNonceUnavailable：重放表写不进去（数据库问题）。2026-11-06：与「nonce 已使用」分开 ——
// 原来两者都回 409，商城无法区分「这是重放，别再发」与「面板暂时不可用，稍后重试」。
var errStoreNonceUnavailable = errors.New("签名重放存储不可用，请稍后重试")

func (s *Server) persistStoreNonce(r *http.Request, secret string) error {
	// 2026-11-06：重放键必须与校验时的规范化一致（小写、去空白）。签名 / nonce 是十六进制，校验时大小写不敏感：
	// 原来按原样入库，截获一条请求后把签名改成大写再发，校验照样通过，库里却查不到 —— 持久化的防重放被绕过。
	nonce := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Store-Nonce")))
	if r.Header.Get("X-Store-Signature-Version") != "2" {
		// 2026-11-03：v1 没有独立 nonce，原来只在进程内存里防重放 ——
		// 换个副本或重启后，5 分钟窗口内截获的 v1 请求可以原样重放。
		// 用签名本身当重放键持久化（与 v2 同一张表，前缀区分）。
		nonce = "v1:" + strings.ToLower(strings.TrimSpace(r.Header.Get("X-Store-Signature")))
	}
	if nonce == "" || nonce == "v1:" {
		return nil
	}
	sum := sha256.Sum256([]byte(secret + "\x00" + nonce))
	tag, err := s.db.Exec(r.Context(), `INSERT INTO store_signature_nonces(nonce_hash,expires_at) VALUES($1,now()+interval '10 minutes') ON CONFLICT DO NOTHING`, hex.EncodeToString(sum[:]))
	if err != nil {
		s.logger.Warn("store: 写入签名 nonce 失败", "error", err)
		// 回 503 让商城原样重试：进程内的「已见过」标记必须撤掉，否则重试落到同一进程就被当成重放（401）。
		forgetStoreReplayKey(r, secret)
		return errStoreNonceUnavailable
	}
	if tag.RowsAffected() != 1 {
		return errors.New("nonce已使用，请勿重放请求")
	}
	return nil
}
