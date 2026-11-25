package httpapi

import (
	"strings"
	"testing"
)

// TestSpeedtestFallsBackToPortalWhenSSHFails 钉住 2026-11-06 的修复：
// 有 SSH 凭据但 SSH 测速失败（凭据失效 / 指纹未登记 / sshd 不可达）时，
// 绑了节点 API 的节点要改走 portal 通道，定时与手动两条路径都要。
// 原来只有「没凭据」才走 portal，「凭据坏了」两条通道都不走，这些节点永远测不到。
func TestSpeedtestFallsBackToPortalWhenSSHFails(t *testing.T) {
	cases := []struct{ file, sig string }{
		{"monitoring_schedule.go", "func (s *Server) runSpeedtestRound("},
		{"monitoring.go", "func (s *Server) speedtestRun("},
	}
	for _, c := range cases {
		body := funcBlock(t, c.file, c.sig)
		i := strings.Index(body, "RunOnServer(")
		if i < 0 {
			t.Fatalf("%s: 找不到 RunOnServer 调用", c.file)
		}
		rest := body[i:]
		j := strings.Index(rest, "err != nil")
		if j < 0 {
			t.Fatalf("%s: RunOnServer 之后没有错误分支", c.file)
		}
		branch := rest[j:]
		if !strings.Contains(branch[:min(len(branch), 900)], "RunOnPortalAndStore(") {
			t.Fatalf("%s: SSH 测速失败后没有改走节点 portal", c.file)
		}
		if !strings.Contains(body, ".fallback = append(") {
			t.Fatalf("%s: 有凭据的主机没有为绑了 API 的节点准备 portal 兜底", c.file)
		}
	}
}
