package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Box encrypts node credentials at rest with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
	// Version lets a future key rotation tell which master key made a ciphertext.
	Version string
}

func NewBox(rawKey string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, fmt.Errorf("CREDENTIAL_KEY must be base64: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("CREDENTIAL_KEY must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, Version: "aesgcm-v1"}, nil
}

func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < b.aead.NonceSize()+b.aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, body := ciphertext[:b.aead.NonceSize()], ciphertext[b.aead.NonceSize():]
	return b.aead.Open(nil, nonce, body, nil)
}
