package httpapi

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 2026-11-06：一键安装持有自己主机的租约，接入节点时内部调用的 bindNodeAPI 必须豁免 nodeMaintenance，
// 否则每次安装都在「绑定门户凭据」这一步 409 失败。
func TestInstallJobBypassesItsOwnMaintenanceLease(t *testing.T) {
	raw, err := os.ReadFile(sourceFile(t, "installhy2.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	i := strings.Index(src, "func (s *Server) runInstallHy2(")
	if i < 0 {
		t.Fatal("找不到 runInstallHy2")
	}
	// 函数体里有 shell 脚本里的花括号，funcBlock 配不了对；只看开头一段（标记必须在任何内部调用之前）。
	body := src[i:min(len(src), i+1200)]
	if !strings.Contains(body, "context.WithValue(ctx, migrationRequestKey{}, true)") {
		t.Fatal("runInstallHy2 必须给内部调用标记 migrationRequestKey（与 runMigrateNode 一致）")
	}
	// nodeMaintenance 对带标记的 ctx 必须放行（即使没有数据库也不能走到查询）。
	s := &Server{}
	ctx := context.WithValue(context.Background(), migrationRequestKey{}, true)
	if s.nodeMaintenance(ctx, "00000000-0000-0000-0000-000000000001") {
		t.Fatal("带 migrationRequestKey 的内部调用不应被维护检查拦截")
	}
}

// 2026-11-06：迁移切到新机器、回滚指回旧机器时都要清掉流量计量基线并与计量互斥（同 bindNodeAPI）。
func TestMigrationResetsTrafficBaseline(t *testing.T) {
	raw, err := os.ReadFile(sourceFile(t, "migratenode.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if n := strings.Count(src, "trafficBaselineResetSQL"); n < 2 {
		t.Fatalf("both the cut-over and the rollback UPDATE must clear the traffic baseline, found %d", n)
	}
	if n := strings.Count(src, "lockTrafficMeterCtx("); n < 2 {
		t.Fatalf("both writes must hold the node's traffic meter lock, found %d", n)
	}
	if strings.Contains(src, "SET server_id=$2::uuid,api_url=$3,updated_at=now()") {
		t.Fatal("a migration UPDATE still moves the node without clearing the baseline")
	}
}
