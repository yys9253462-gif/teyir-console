package httpapi

import (
	"context"
	"testing"
	"time"
)

// 2026-11-04：按节点清理后，过期行（含软删节点的）照样被删，未过期的保留。
func TestPruneTrafficSamplesPerNode(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS traffic_samples(id bigserial PRIMARY KEY, node_id uuid NOT NULL, sampled_at timestamptz NOT NULL, rx_bytes bigint NOT NULL DEFAULT 0, tx_bytes bigint NOT NULL DEFAULT 0, source text NOT NULL DEFAULT 'x', UNIQUE(node_id, sampled_at, source));
INSERT INTO hysteria_nodes(id,name) VALUES('`+fixNode+`','live');
INSERT INTO hysteria_nodes(id,name,deleted_at) VALUES('00000000-0000-0000-0000-0000000d0f01','gone',now());
INSERT INTO traffic_samples(node_id,sampled_at) VALUES
 ('`+fixNode+`', now()-interval '40 days'), ('`+fixNode+`', now()-interval '1 day'),
 ('00000000-0000-0000-0000-0000000d0f01', now()-interval '40 days')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	if n := s.pruneTrafficSamples(ctx, time.Now().Add(-30*24*time.Hour)); n != 2 {
		t.Fatalf("want 2 expired rows pruned (live + soft-deleted node), got %d", n)
	}
	var left int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM traffic_samples`).Scan(&left)
	if left != 1 {
		t.Fatalf("recent row must stay, %d rows left", left)
	}
}
