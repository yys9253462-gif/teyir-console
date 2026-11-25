package executor

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestAudit20261122_ExecutorIntervalGuardAndPreDialValidation(t *testing.T) {
	e := New(nil, nil, 0, nil)
	if e.interval <= 0 {
		t.Fatalf("非正执行器轮询间隔必须回退为正默认值以防 NewTicker panic，实际 %v", e.interval)
	}
	// 参数非法时必须在拨 SSH 之前就拒绝（hasCred=false 不会先报未配凭据）
	_, _, err := e.healthCheck(context.Background(), task{
		parameters: []byte(`{"unit":"sshd"}`),
		writeUnits: []string{"hysteria-server"},
		hasCred:    false,
	})
	if err == nil || err.Error() == "该主机未配置 SSH 凭据（私钥或密码），无法执行" {
		t.Fatalf("healthCheck 应在拨 SSH 前拦截白名单外服务名，实际错误: %v", err)
	}
	raw, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "func (e *Executor) Wait()") {
		t.Fatal("Executor 必须提供 Wait() 供优雅停机等待在途任务写回")
	}
}
