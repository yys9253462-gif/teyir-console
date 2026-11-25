package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 2026-11-03：v1 签名的重放键原来只存在进程内存里，换副本 / 重启后可在 5 分钟窗口内重放。
// 现在与 v2 一样落库：同一签名第二次提交必须被拒绝，即使内存记录已经不在。
func TestStoreV1SignatureReplayIsPersisted(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	req := httptest.NewRequest(http.MethodPost, "/api/store/orders", nil)
	req.Header.Set("X-Store-Signature", "fixture-v1-signature")
	if err := s.persistStoreNonce(req, "fixture-secret"); err != nil {
		t.Fatalf("first use must be accepted: %v", err)
	}
	if err := s.persistStoreNonce(req, "fixture-secret"); err == nil {
		t.Fatal("replaying a v1 signature must be refused from the database, not only process memory")
	}
	// v2 照旧按 nonce。
	v2 := httptest.NewRequest(http.MethodPost, "/api/store/orders", nil)
	v2.Header.Set("X-Store-Signature-Version", "2")
	v2.Header.Set("X-Store-Nonce", "0123456789abcdef0123456789abcdef")
	if err := s.persistStoreNonce(v2, "fixture-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.persistStoreNonce(v2, "fixture-secret"); err == nil || errors.Is(err, errStoreNonceUnavailable) {
		t.Fatalf("v2 nonce replay must be refused as a replay, got %v", err)
	}
	// 2026-11-06：改写签名的大小写 / 加空白不能绕过持久化的防重放（校验本身大小写不敏感）。
	upper := httptest.NewRequest(http.MethodPost, "/api/store/orders", nil)
	upper.Header.Set("X-Store-Signature", "  FIXTURE-V1-SIGNATURE ")
	if err := s.persistStoreNonce(upper, "fixture-secret"); err == nil {
		t.Fatal("a v1 signature replayed with different case must still be refused")
	}
	// 2026-11-06：重放表不可用要与「重放」区分（503 可重试 vs 409 别再发）。
	if _, err := db.Exec(context.Background(), `DROP TABLE store_signature_nonces`); err != nil {
		t.Fatal(err)
	}
	fresh := httptest.NewRequest(http.MethodPost, "/api/store/orders", nil)
	fresh.Header.Set("X-Store-Signature-Version", "2")
	fresh.Header.Set("X-Store-Nonce", "fedcba9876543210fedcba9876543210")
	replayKey := storeReplayCacheKey("fixture-secret", "v2:fedcba9876543210fedcba9876543210")
	storeNonces.Lock()
	storeNonces.seen[replayKey] = time.Now().Add(time.Minute)
	storeNonces.Unlock()
	if err := s.persistStoreNonce(fresh, "fixture-secret"); !errors.Is(err, errStoreNonceUnavailable) {
		t.Fatalf("a storage failure must be reported as unavailable, got %v", err)
	}
	// 503 之后商城原样重试：进程内的「已见过」标记必须已撤掉，否则重试被当成重放。
	storeNonces.Lock()
	_, stillSeen := storeNonces.seen[replayKey]
	storeNonces.Unlock()
	if stillSeen {
		t.Fatal("a 503 from nonce storage must forget the in-memory replay mark so the retry is accepted")
	}
}
