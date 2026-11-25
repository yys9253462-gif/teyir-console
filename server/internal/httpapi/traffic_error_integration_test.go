package httpapi

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// 2026-11-03：读不到节点 API 凭据时计量原来直接 return，traffic_error 留空 ⇒
// 有月限额的节点悄悄停止计量、超额也不会被停用，界面却显示正常。
func TestTrafficMeteringRecordsCredentialFailure(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN traffic_error text NOT NULL DEFAULT '';
INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port) VALUES('`+fixNode+`','n','https://8.8.8.8:8443','not-a-valid-ciphertext'::bytea,'fixture.test',443)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box, _ = secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	s.meterOneNode(ctx, trafficNode{id: fixNode, name: "n"}, time.Now())
	var reason string
	if err := db.QueryRow(ctx, `SELECT traffic_error FROM hysteria_nodes`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "解密失败") {
		t.Fatalf("metering failure must be visible in traffic_error, got %q", reason)
	}
}
