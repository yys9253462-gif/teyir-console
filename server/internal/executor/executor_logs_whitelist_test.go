package executor

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 2026-11-06：日志只能读 hysteria-server 或白名单内的服务；越界的在拨号之前就拒绝。
func TestFetchLogsRestrictedToWhitelist(t *testing.T) {
	e := &Executor{}
	_, _, err := e.fetchLogs(context.Background(), task{parameters: []byte(`{"unit":"sshd"}`), writeUnits: []string{"hysteria-server"}})
	if err == nil || !strings.Contains(err.Error(), "sshd") {
		t.Fatalf("不在白名单的服务必须拒绝：%v", err)
	}
	// 白名单内 / 默认服务：越过白名单检查，走到拨号（这里没有凭据，失败在拨号而不是白名单）。
	for _, params := range []string{`{}`, `{"unit":"hysteria-server"}`, `{"unit":"xray"}`} {
		_, _, err := e.fetchLogs(context.Background(), task{parameters: []byte(params), writeUnits: []string{"xray"}})
		if err == nil || !strings.Contains(err.Error(), "未配置 SSH 凭据") {
			t.Fatalf("%s 应通过白名单检查、停在拨号：%v", params, err)
		}
	}
}

// 2026-11-06：健康检查同样受白名单约束。原来只做格式校验，任何合法 unit 都能 systemctl is-active ——
// 白名单只有 hysteria-server 的主机上也能以 root 探测 sshd 等服务的状态。
func TestHealthCheckHonoursUnitWhitelist(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (e *Executor) healthCheck(")
	end := strings.Index(body[start:], "func (e *Executor) fetchLogs(")
	if start < 0 || end < 0 {
		t.Fatal("cannot locate healthCheck")
	}
	fn := body[start : start+end]
	if !strings.Contains(fn, `unit != "hysteria-server" && !containsUnit(t.writeUnits, unit)`) {
		t.Fatal("healthCheck must refuse units outside the host whitelist, like controlService and fetchLogs do")
	}
}
