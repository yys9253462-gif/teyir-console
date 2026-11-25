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
