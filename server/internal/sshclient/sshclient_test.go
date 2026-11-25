package sshclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestServer 起一个只做握手、接受任意密码的 SSH 服务端。
func startTestServer(t *testing.T) (host string, port int, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "test")
				}
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port, Fingerprint(signer.PublicKey())
}

func TestDialFingerprintMismatchIsTyped(t *testing.T) {
	host, port, real := startTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cred := Credential{Type: "password", Secret: []byte("x")}

	_, _, err := Dial(ctx, host, port, "root", cred, "SHA256:not-the-real-one")
	var m *HostKeyMismatchError
	if !errors.As(err, &m) {
		t.Fatalf("want HostKeyMismatchError, got %v", err)
	}
	if m.Got != real || m.Expected != "SHA256:not-the-real-one" {
		t.Fatalf("mismatch fields wrong: %+v (real %s)", m, real)
	}

	client, got, err := Dial(ctx, host, port, "root", cred, real)
	if err != nil {
		t.Fatalf("correct pin must connect: %v", err)
	}
	client.Close()
	if got != real {
		t.Fatalf("returned fingerprint %s, want %s", got, real)
	}
}

// 2026-11-06：上传凭据时用连接时同一个解析器校验私钥。
func TestValidatePrivateKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	good := pem.EncodeToMemory(block)
	if err := ValidatePrivateKey(good); err != nil {
		t.Fatalf("有效私钥应通过：%v", err)
	}
	if err := ValidatePrivateKey(good[:len(good)/2]); err == nil {
		t.Fatal("截断的私钥必须被拒绝")
	}
	if err := ValidatePrivateKey([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot a key\n-----END OPENSSH PRIVATE KEY-----\n")); err == nil {
		t.Fatal("只有 PRIVATE KEY 字样的内容必须被拒绝")
	}
}
