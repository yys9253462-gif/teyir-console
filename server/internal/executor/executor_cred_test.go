package executor

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 2026-11-06：凭据密文存在但解不开，要报「解密失败」而不是「未配置 SSH 凭据」——
// 后者把运维引去重新上传凭据，而问题在面板的加密密钥。
func TestDialDistinguishesBrokenCredentialFromMissing(t *testing.T) {
	e := &Executor{}
	_, err := e.dial(context.Background(), task{credBroken: true})
	if err == nil || !strings.Contains(err.Error(), "解密失败") {
		t.Fatalf("broken credential must say decryption failed, got %v", err)
	}
	_, err = e.dial(context.Background(), task{})
	if err == nil || !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("missing credential must say not configured, got %v", err)
	}
	src, rerr := os.ReadFile("executor.go")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(src), "t.credBroken = true") {
		t.Fatal("readClaimed 必须在解密失败时标记 credBroken")
	}
}
