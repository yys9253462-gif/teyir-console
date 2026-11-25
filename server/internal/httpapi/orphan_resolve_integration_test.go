package httpapi

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

type userNotFoundTransport struct{}

func (userNotFoundTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"User not found"}`)), Header: make(http.Header)}, nil
}

// 2026-11-03：节点报「用户不存在」但结案写回失败时，原来照样回 status=resolved，
// 刷新后这条又出现在列表里。必须带 warning 如实说明。
func TestRetryOrphanReportsResolveWriteFailure(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	const orphan = "00000000-0000-0000-0000-0000000f0001"
	if _, err := db.Exec(ctx, `CREATE TABLE orphan_node_accounts(id uuid PRIMARY KEY,node_id uuid NOT NULL,node_name text NOT NULL DEFAULT '',node_api_url text NOT NULL DEFAULT '',user_id text NOT NULL,subscription_id uuid,api_key_cipher bytea,reason text NOT NULL DEFAULT '',attempts int NOT NULL DEFAULT 0,last_attempt_at timestamptz,resolved_at timestamptz,resolved_note text NOT NULL DEFAULT '',created_at timestamptz NOT NULL DEFAULT now());
CREATE FUNCTION fail_resolve() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.resolved_at IS NOT NULL THEN RAISE EXCEPTION 'simulated outage'; END IF; RETURN NEW; END $$;
CREATE TRIGGER fail_resolve BEFORE UPDATE ON orphan_node_accounts FOR EACH ROW EXECUTE FUNCTION fail_resolve();`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO orphan_node_accounts(id,node_id,node_name,node_api_url,user_id,api_key_cipher) VALUES($1::uuid,$2::uuid,'n','https://8.8.8.8:8443','sub_x',$3)`, orphan, fixNode, key); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	old := portalClient
	portalClient = &http.Client{Transport: userNotFoundTransport{}}
	t.Cleanup(func() { portalClient = old })

	req := httptest.NewRequest(http.MethodPost, "/api/orphans/"+orphan+"/retry", nil)
	req.SetPathValue("id", orphan)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.retryOrphan(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, `"status":"resolved"`) || !strings.Contains(body, `"warning"`) {
		t.Fatalf("resolve write failed but response claims resolved: %d %s", rec.Code, body)
	}
}
