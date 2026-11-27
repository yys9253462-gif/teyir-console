package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Claims struct {
	Version int64  `json:"ver,omitempty"`
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Role    string `json:"role"`
	Expires int64  `json:"exp"`
	Issued  int64  `json:"iat"`
}

type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewSigner(secret string, ttl time.Duration) *Signer {
	return &Signer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// TTL reports how long issued sessions stay valid.
func (s *Signer) TTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.ttl
}

func (s *Signer) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Signer) Issue(subject, email, role string) (string, error) {
	return s.IssueVersion(subject, email, role, 0)
}

func (s *Signer) IssueVersion(subject, email, role string, version int64) (string, error) {
	if s == nil || len(s.secret) == 0 {
		return "", errors.New("auth: missing signing secret")
	}
	if s.ttl <= 0 {
		return "", errors.New("auth: invalid signing ttl")
	}
	now := s.currentTime()
	claims := Claims{
		Version: version,
		Subject: subject,
		Email:   email,
		Role:    role,
		Expires: now.Add(s.ttl).Unix(),
		Issued:  now.Unix(),
	}
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + signature, nil
}

func (s *Signer) Verify(token string) (Claims, error) {
	if s == nil || len(s.secret) == 0 {
		return Claims{}, errors.New("auth: missing signing secret")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("auth: malformed token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, fmt.Errorf("auth: decode header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Claims{}, fmt.Errorf("auth: parse header: %w", err)
	}
	if header.Alg != "HS256" || (header.Typ != "" && header.Typ != "JWT") {
		return Claims{}, errors.New("auth: unsupported token header")
	}
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(signingInput))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return Claims{}, errors.New("auth: invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("auth: decode payload: %w", err)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, fmt.Errorf("auth: parse payload: %w", err)
	}
	if s.currentTime().Unix() >= claims.Expires {
		return Claims{}, errors.New("auth: token expired")
	}
	return claims, nil
}
