package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// subIdsTransport 让第一台节点慢回，制造「结果到达顺序 ≠ 节点顺序」。
type subIdsTransport struct{ slowHost string }

func (t subIdsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	if host == t.slowHost {
		time.Sleep(150 * time.Millisecond)
	}
	body := `{"ok":true,"users":[{"user_id":"sub_on_` + host + `"}]}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// 2026-11-03：/api/sub-ids 原来按 nodes[i] 取节点 id，而 results 是按 channel 到达顺序排的，
// 两台以上节点时账号会被记到别的节点名下。
func TestSubIdsAttributesAccountsToTheRightNode(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	const slowNode = "00000000-0000-0000-0000-0000000d0001"
	const fastNode = "00000000-0000-0000-0000-0000000d0002"
	for _, n := range []struct{ id, url string }{{slowNode, "https://8.8.8.8:8443"}, {fastNode, "https://8.8.4.4:8443"}} {
		if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id,created_at)
			VALUES($1::uuid,$1,$2,$3,'fixture.test',443,$4::uuid, now() - CASE WHEN $1 = '`+slowNode+`' THEN interval '1 hour' ELSE interval '0' END)`, n.id, n.url, key, fixHost); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.box = box
	s.readLimiter = newSlidingLimiter(time.Minute, 100)
	old := portalClient
	portalClient = &http.Client{Transport: subIdsTransport{slowHost: "8.8.8.8"}}
	t.Cleanup(func() { portalClient = old })

	rec := httptest.NewRecorder()
	s.subIdsAggregate(rec, httptest.NewRequest(http.MethodGet, "/api/sub-ids", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ByUserID map[string][]struct {
			NodeID string `json:"node_id"`
		} `json:"by_user_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for uid, want := range map[string]string{"sub_on_8.8.8.8": slowNode, "sub_on_8.8.4.4": fastNode} {
		got := out.ByUserID[uid]
		if len(got) != 1 || got[0].NodeID != want {
			t.Errorf("%s attributed to %+v, want node %s", uid, got, want)
		}
	}
}
