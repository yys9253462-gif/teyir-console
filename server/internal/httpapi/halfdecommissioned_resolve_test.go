package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 半下线登记的孤儿说明写的是「已直接结案」，就必须真的结案（设 resolved_at），
// 否则永远挂在未结案列表里。
func TestHalfDecommissionedOrphansAreResolved(t *testing.T) {
	body := funcBlock(t, "halfdecommissioned.go", "func (s *Server) sweepHalfDecommissioned(")
	for _, want := range []string{"resolved_at=now()", "halfDecommissionedReason", "halfDecommissionedResolvedNote"} {
		if !strings.Contains(body, want) {
			t.Fatalf("半下线孤儿登记后没有结案：缺 %s", want)
		}
	}
	rec := strings.Index(body, "s.recordOrphanAccounts(")
	res := strings.Index(body, "resolved_at=now()")
	if rec < 0 || res < rec {
		t.Fatal("必须先登记、后结案")
	}
}

// 2026-11-04：半下线巡检只能结案没有凭据快照的记录；带 key 的那条仍可补删，必须留在待办里。
func TestHalfDecommissionedKeepsRetryableOrphans(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("../../migrations", "023_orphan_node_accounts.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 023: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO hysteria_nodes(id,name,server_id,deleted_at) VALUES('`+fixNode+`','gone','`+fixHost+`',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO orphan_node_accounts(node_id,node_name,node_api_url,user_id,api_key_cipher,reason)
VALUES($1::uuid,'gone','https://8.8.8.8:8443','sub_keep','key'::bytea,'删除节点时回收失败')`, fixNode); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(id,token,node_accounts) VALUES($1::uuid,'fixture-token-half-decom-0001',$2::jsonb)`,
		fixSub, `{"`+fixNode+`":{"user_id":"sub_keep","password":"p"},"00000000-0000-0000-0000-0000000001b9":{"user_id":"sub_other","password":"p"}}`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	if _, err := s.sweepHalfDecommissioned(ctx); err != nil {
		t.Fatal(err)
	}
	var resolved bool
	if err := db.QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM orphan_node_accounts WHERE user_id='sub_keep'`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved {
		t.Fatal("an orphan that still holds a key snapshot must stay pending for retry")
	}
}
