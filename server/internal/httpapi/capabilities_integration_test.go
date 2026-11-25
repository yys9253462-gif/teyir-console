package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

type refusedTransport struct{}

func (refusedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connect: connection refused")
}

func resetNodeCapsCache() {
	nodeCapsCache.Lock()
	nodeCapsCache.result = nodeCapabilitiesResult{}
	nodeCapsCache.fetchedAt = time.Time{}
	nodeCapsCache.inflight = nil
	nodeCapsCache.lastErr = nil
	nodeCapsCache.Unlock()
}

// 2026-11-03：连不上的节点原来 supported=false ⇒ 前端显示「门户待升级」并计入 outdated。
// 同时：TTL 内一次失败的强制刷新不能让之后的普通读取全报错。
func TestCapabilitiesUnreachableIsNotOutdatedAndCacheSurvivesFailure(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'fixture.test',443)`, fixNode, key); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: refusedTransport{}}
	resetNodeCapsCache()
	t.Cleanup(func() { portalClient = old; resetNodeCapsCache() })

	res, err := s.cachedNodeCapabilities(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Reachable || !res.Nodes[0].Supported || res.Outdated != 0 {
		t.Fatalf("unreachable node must be supported=true and not outdated: %+v", res)
	}

	// 一次强制刷新撞上数据库故障。
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes RENAME TO hysteria_nodes_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cachedNodeCapabilities(ctx, true); err == nil {
		t.Fatal("forced refresh on a broken database must report the error to its caller")
	}
	got, err := s.cachedNodeCapabilities(ctx, false)
	if err != nil || len(got.Nodes) != 1 {
		t.Fatalf("cached result within TTL must still be served after a failed refresh: %v %+v", err, got)
	}
}
