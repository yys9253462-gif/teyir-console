package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestSignerRejectsEmptySecret(t *testing.T) {
	signer := NewSigner("", time.Hour)
	if _, err := signer.Issue("u1", "a@example.com", "admin"); err == nil {
		t.Fatal("空密钥签发必须失败")
	}
	if _, err := signer.Verify("a.b.c"); err == nil {
		t.Fatal("空密钥验签必须失败")
	}
}

func TestSignerIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	signer := NewSigner("test-only-signing-secret-0123456789", 10*time.Minute)
	signer.now = func() time.Time { return now }

	token, err := signer.IssueVersion("user-1", "admin@example.local", "admin", 3)
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	claims, err := signer.Verify(token)
	if err != nil {
		t.Fatalf("验证合法令牌失败: %v", err)
	}
	if claims.Subject != "user-1" || claims.Role != "admin" || claims.Version != 3 {
		t.Fatalf("解析声明不匹配: %+v", claims)
	}

	// 推进时钟至到期时刻，必须判过期。
	signer.now = func() time.Time { return now.Add(10 * time.Minute) }
	if _, err := signer.Verify(token); err == nil {
		t.Fatal("已过期令牌必须被拒绝")
	}
}

func TestSignerRejectsUnsupportedHeaderEvenWithValidMAC(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	secret := "test-only-signing-secret-0123456789"
	signer := NewSigner(secret, time.Hour)
	signer.now = func() time.Time { return now }

	badHeader, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(Claims{
		Subject: "user-1",
		Role:    "admin",
		Expires: now.Add(time.Hour).Unix(),
		Issued:  now.Unix(),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(badHeader) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	forged := signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if _, err := signer.Verify(forged); err == nil {
		t.Fatal("非允许算法头部即使签名匹配也必须被拒绝")
	}
}

// 验证空接收者、未设时钟的零值 Signer 与非正 TTL 均安全返回错误且不触发空指针崩溃。
func TestSignerNilAndNonPositiveTTLRejected(t *testing.T) {
	var nilSigner *Signer
	if nilSigner.TTL() != 0 {
		t.Fatal("空 Signer 的 TTL 应为 0")
	}
	if _, err := nilSigner.Issue("u1", "a@example.invalid", "admin"); err == nil {
		t.Fatal("空 Signer 签发必须报错")
	}
	if _, err := nilSigner.Verify("a.b.c"); err == nil {
		t.Fatal("空 Signer 验签必须报错")
	}
	badTTL := NewSigner("test-only-signing-secret-0123456789", 0)
	if _, err := badTTL.Issue("u1", "a@example.invalid", "admin"); err == nil {
		t.Fatal("非正 TTL 签发必须报错")
	}
	// 手工构造未初始化 now 函数的 Signer 也应回退系统时钟而不崩溃。
	manual := &Signer{secret: []byte("test-only-signing-secret-0123456789"), ttl: time.Minute}
	tok, err := manual.Issue("u1", "a@example.invalid", "admin")
	if err != nil {
		t.Fatalf("未显式设 now 的 Signer 应回退系统时钟签发: %v", err)
	}
	if _, err := manual.Verify(tok); err != nil {
		t.Fatalf("未显式设 now 的 Signer 应正常验签: %v", err)
	}
}
