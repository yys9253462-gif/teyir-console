package sshclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"strings"
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
	// 新版 OpenSSH 加密私钥不含 ENCRYPTED 字样，仍须准确识别为带口令私钥并统一拒绝。
	encBlock, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("fake-passphrase-for-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateKey(pem.EncodeToMemory(encBlock)); err == nil || !strings.Contains(err.Error(), "passphrase-protected") {
		t.Fatalf("新版 OpenSSH 带口令私钥必须返回统一拒绝错误，实际: %v", err)
	}
	// 未加密但注释含 ENCRYPTED 字样的合法私钥不得被误杀。
	noteBlock, err := ssh.MarshalPrivateKey(priv, "note-ENCRYPTED-comment")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateKey(pem.EncodeToMemory(noteBlock)); err != nil {
		t.Fatalf("未加密私钥注释含 ENCRYPTED 不应被误拒: %v", err)
	}
}

func TestDialRejectsInvalidHostAndPort(t *testing.T) {
	cred := Credential{Type: "password", Secret: []byte("fake-test-pass")}
	if _, _, err := Dial(context.Background(), "   ", 22, "root", cred, ""); err == nil {
		t.Fatal("空主机地址必须被拒绝")
	}
	for _, port := range []int{0, -1, 65536} {
		if _, _, err := Dial(context.Background(), "127.0.0.1", port, "root", cred, ""); err == nil {
			t.Fatalf("非法端口 %d 必须被拒绝", port)
		}
	}
}

// 验证当远端完成 SSH 握手但故意不响应新会话请求（模拟网络黑洞）时，Run 能在上下文超时内返回而不永久卡死。
func TestRunRespectsContextWhenNewSessionHangs(t *testing.T) {
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

	stopServer := make(chan struct{})
	t.Cleanup(func() { close(stopServer) })
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer c.Close()
		_, chans, reqs, srvErr := ssh.NewServerConn(c, cfg)
		if srvErr != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		// 故意挂起通道接收，不回复 Accept 也不回复 Reject，模拟远端网络黑洞。
		<-stopServer
		for range chans {
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	client, _, err := Dial(dialCtx, h, port, "root", Credential{Type: "password", Secret: []byte("x")}, Fingerprint(signer.PublicKey()))
	if err != nil {
		t.Fatalf("连接测试 SSH 服务失败: %v", err)
	}
	defer client.Close()

	runCtx, runCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer runCancel()
	start := time.Now()
	_, _, _, err = Run(runCtx, client, "echo hi")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("挂起的 NewSession 应按上下文超时返回 DeadlineExceeded，实际: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("超时返回耗时过长: %v", time.Since(start))
	}
}

// 验证 Dial 拒绝空用户名，且在上下文已取消时不会返回悬空连接。
func TestDialRejectsEmptyUserAndRespectsCanceledContext(t *testing.T) {
	cred := Credential{Type: "password", Secret: []byte("fake-test-pass")}
	if _, _, err := Dial(context.Background(), "127.0.0.1", 22, "   ", cred, ""); err == nil {
		t.Fatal("空用户名必须被拒绝")
	}

	host, port, real := startTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if client, _, err := Dial(ctx, host, port, "root", cred, real); err == nil {
		client.Close()
		t.Fatal("已取消上下文下 Dial 不得返回成功连接")
	}
}
