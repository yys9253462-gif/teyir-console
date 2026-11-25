package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-11-08：已删除节点的待补删可按节点一次结案；在役节点必须拒绝，
// 不能借这个入口放弃还能补删的账号。只结案该节点、只结案未结案的行。
func TestResolveOrphansMachineOffline(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const goneNode = "00000000-0000-0000-0000-0000000d0001"
	const liveNode = "00000000-0000-0000-0000-0000000d0002"
	if _, err := db.Exec(ctx, `CREATE TABLE orphan_node_accounts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid NOT NULL,node_name text NOT NULL DEFAULT '',node_api_url text NOT NULL DEFAULT '',user_id text NOT NULL,subscription_id uuid,api_key_cipher bytea,reason text NOT NULL DEFAULT '',attempts int NOT NULL DEFAULT 0,last_attempt_at timestamptz,resolved_at timestamptz,resolved_note text NOT NULL DEFAULT '',created_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES ($1::uuid,'雅加达',now()),($2::uuid,'东京',NULL)`,
		`INSERT INTO orphan_node_accounts(node_id,node_name,user_id,api_key_cipher) VALUES ($1::uuid,'雅加达','sub_a',''),($1::uuid,'雅加达','sub_b',''),($1::uuid,'雅加达','sub_c',NULL),($2::uuid,'东京','sub_d','')`,
		`INSERT INTO orphan_node_accounts(node_id,node_name,user_id,resolved_at,resolved_note) VALUES ($1::uuid,'雅加达','sub_old',now()-interval '1 day','早先结案'),($2::uuid,'东京','sub_dummy',now(),'x')`,
	} {
		if _, err := db.Exec(ctx, q, goneNode, liveNode); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)

	call := func(nodeID string, confirm bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/orphans/nodes/"+nodeID+"/resolve-machine-offline", nil)
		req.SetPathValue("nodeID", nodeID)
		if confirm {
			req.Header.Set("X-Confirm-Dangerous", "yes")
		}
		rec := httptest.NewRecorder()
		s.resolveOrphansMachineOffline(rec, asUser(req, adminA))
		return rec
	}
	open := func(nodeID string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM orphan_node_accounts WHERE node_id=$1::uuid AND resolved_at IS NULL`, nodeID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if rec := call(goneNode, false); rec.Code != http.StatusPreconditionRequired || open(goneNode) != 3 {
		t.Fatalf("without confirm header: want 428 and nothing resolved, got %d open=%d", rec.Code, open(goneNode))
	}
	if rec := call("not-a-uuid", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: want 400, got %d", rec.Code)
	}
	if rec := call(liveNode, true); rec.Code != http.StatusConflict || open(liveNode) != 1 {
		t.Fatalf("live node must be refused: got %d open=%d %s", rec.Code, open(liveNode), rec.Body.String())
	}
	rec := call(goneNode, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved":3`) || open(goneNode) != 0 {
		t.Fatalf("deleted node: want 200 resolved=3, got %d open=%d %s", rec.Code, open(goneNode), rec.Body.String())
	}
	if open(liveNode) != 1 {
		t.Fatal("another node's orphans were touched")
	}
	var note string
	if err := db.QueryRow(ctx, `SELECT resolved_note FROM orphan_node_accounts WHERE user_id='sub_old'`).Scan(&note); err != nil || note != "早先结案" {
		t.Fatalf("already-resolved row was rewritten: %q %v", note, err)
	}
	var audits int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='resolve_orphans_machine_offline'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("want exactly one audit entry, got %d %v", audits, err)
	}
	if rec := call(goneNode, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resolved":0`) {
		t.Fatalf("second call must be a no-op: %d %s", rec.Code, rec.Body.String())
	}
}
