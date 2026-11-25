package httpapi

import (
	"context"
	"strings"
	"testing"
)

// 2026-11-03：主机地址登记成域名的节点原来被静默跳过，被墙检测页永远停在旧结论 / 「尚未检测」。
func TestBlockCheckRecordsUnprobeableHosts(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const probe = "00000000-0000-0000-0000-0000000b0001"
	if _, err := db.Exec(ctx, `CREATE TABLE node_block_checks(node_id uuid PRIMARY KEY,probe_server_id uuid,target_ip text NOT NULL DEFAULT '',tcp_port int,tcp_ok boolean,ping_ok boolean,blocked boolean,detail text NOT NULL DEFAULT '',checked_at timestamptz);
INSERT INTO servers(id,host) VALUES('`+probe+`','1.1.1.1'),('`+fixHost+`','node.example.com');
INSERT INTO hysteria_nodes(id,name,server_id) VALUES('`+fixNode+`','n','`+fixHost+`');
INSERT INTO node_block_checks(node_id,target_ip,blocked,detail,checked_at) VALUES('`+fixNode+`','old',true,'old verdict',now()-interval '3 hours');
INSERT INTO system_settings(key,value) VALUES('`+cnProbeSettingKey+`','`+probe+`')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	if _, err := s.runBlockChecks(ctx); err != nil {
		t.Fatal(err)
	}
	var detail string
	var blocked *bool
	var fresh bool
	if err := db.QueryRow(ctx, `SELECT detail, blocked, checked_at > now() - interval '1 minute' FROM node_block_checks WHERE node_id=$1::uuid`, fixNode).Scan(&detail, &blocked, &fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh || blocked != nil || !strings.Contains(detail, "不是 IP") {
		t.Fatalf("unprobeable node must get a fresh NULL result with a reason, got fresh=%v blocked=%v detail=%q", fresh, blocked, detail)
	}
}
