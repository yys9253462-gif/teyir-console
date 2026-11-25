package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// deliveryViewFixture 建一台能进全站构建的上架节点，并返回调用交付页接口的函数。
// 订阅由各用例自己插入（没有节点账号映射，即「账号已收回」或「还没开好」的状态）。
func deliveryViewFixture(t *testing.T) (*Server, func(token string) *httptest.ResponseRecorder) {
	t.Helper()
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,portal_basic_user,portal_basic_password,portal_path_token) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'fixture.test',443,'u',decode('00','hex'),'0123456789abcdef0123456789abcdef')`, fixNode, cipher); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: slowMetaPortal{delay: time.Millisecond}}
	InvalidateSubscriptionCache()
	t.Cleanup(func() { portalClient = old; InvalidateSubscriptionCache() })
	view := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/api/sub/view/"+token, nil)
		r.SetPathValue("token", token)
		w := httptest.NewRecorder()
		s.subscriptionView(w, r)
		return w
	}
	return s, view
}

// 2026-11-12：已失效的订阅（保留型到期、或已停用）在节点账号被收回后，交付页必须照常回 200，
// 让买家看到「已到期 / 已暂停」和续费入口。原来「范围内节点全都没有账号」被当成临时故障，一律 503。
func TestDeliveryViewRendersInactiveSubscriptions(t *testing.T) {
	s, view := deliveryViewFixture(t)
	ctx := context.Background()
	const expiredToken = "fixture-expired-retained-subscription"
	const disabledToken = "fixture-disabled-revoked-subscription"
	const pendingToken = "fixture-active-not-provisioned-subscription"
	if _, err := s.db.Exec(ctx, `INSERT INTO subscription_tokens(token,order_id,enabled,expires_at,auto_delete) VALUES
 ($1,'order-expired',false,now()-interval '1 hour',false),
 ($2,'order-disabled',false,now()+interval '30 days',false),
 ($3,'order-pending',true,now()+interval '30 days',false)`, expiredToken, disabledToken, pendingToken); err != nil {
		t.Fatal(err)
	}

	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode view: %v %s", err, w.Body.String())
		}
		return body
	}

	w := view(expiredToken)
	if w.Code != http.StatusOK {
		t.Fatalf("an expired, retained subscription must render the delivery page, got %d %s", w.Code, w.Body.String())
	}
	body := decode(w)
	if body["expired"] != true || body["enabled"] != false || body["node_count"] != float64(0) {
		t.Fatalf("expired view: expired=%v enabled=%v node_count=%v", body["expired"], body["enabled"], body["node_count"])
	}
	if _, leaked := body["subscription_url"]; leaked {
		t.Fatal("an expired subscription must not be handed working links")
	}

	w = view(disabledToken)
	if w.Code != http.StatusOK {
		t.Fatalf("a disabled subscription must render the delivery page, got %d %s", w.Code, w.Body.String())
	}
	body = decode(w)
	if body["expired"] != false || body["enabled"] != false {
		t.Fatalf("disabled view: expired=%v enabled=%v", body["expired"], body["enabled"])
	}
	if _, leaked := body["subscription_url"]; leaked {
		t.Fatal("a disabled subscription must not be handed working links")
	}

	// 生效但还没开好号的订阅仍是「暂时给不出」：不能因为这次修改变成 200 加 0 个节点。
	if w = view(pendingToken); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("an active subscription with no provisioned account must stay 503, got %d %s", w.Code, w.Body.String())
	}
}

// 2026-11-12：生效订阅的节点构建出错时，交付页与 /sub 一样回 503（固定文案），不再回 200 加 0 个节点；
// 已失效的订阅不需要节点清单，照常渲染。
func TestDeliveryViewBuildFailure(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	// fixDB 的 hysteria_nodes 没有门户网页凭据三列，subscriptionBuildFor 的查询必然报错。
	const activeToken = "fixture-active-subscription-build-error"
	const expiredToken = "fixture-expired-subscription-build-error"
	if _, err := db.Exec(ctx, `INSERT INTO subscription_tokens(token,order_id,enabled,expires_at,auto_delete) VALUES
 ($1,'order-active',true,now()+interval '30 days',false),
 ($2,'order-expired',true,now()-interval '1 hour',false)`, activeToken, expiredToken); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	InvalidateSubscriptionCache()
	t.Cleanup(InvalidateSubscriptionCache)
	view := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://fixture.invalid/api/sub/view/"+token, nil)
		r.SetPathValue("token", token)
		w := httptest.NewRecorder()
		s.subscriptionView(w, r)
		return w
	}

	w := view(activeToken)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a build failure must not render an active subscription as 0 nodes, got %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("the 503 must carry Retry-After")
	}
	// 买家能直接打开这个端点：响应里不能带数据库错误原文。
	for _, internal := range []string{"portal_basic_user", "SQLSTATE", "column", "hysteria_nodes"} {
		if strings.Contains(w.Body.String(), internal) {
			t.Fatalf("the public response leaked internal error text %q: %s", internal, w.Body.String())
		}
	}

	if w = view(expiredToken); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"expired":true`) {
		t.Fatalf("an expired subscription must still render when the build fails, got %d %s", w.Code, w.Body.String())
	}
}

// 2026-11-12：/sub 与交付页对同一条订阅讲同一个故事。到期清扫把保留型订阅置为 enabled=false 之后，
// /sub 必须继续答「已到期」，不能改答「已被暂停」。
func TestFeedReportsExpiryAfterSweepDisabledTheRow(t *testing.T) {
	s := repairSubscriptionDB(t)
	repairExec(t, s, `INSERT INTO subscription_tokens(id,token,ip_limit,enabled,expires_at,auto_delete) VALUES($1,$2,1,false,now()-interval '1 hour',false)`, repairFirst, repairToken)
	w := repairFeed(s, repairToken, "198.51.100.10:1234", "mihomo/1.0")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "subscription expired") {
		t.Fatalf("an expired subscription the sweeper disabled must still answer expired, got %d %s", w.Code, w.Body.String())
	}
	// 未到期的停用仍然是 disabled。
	repairExec(t, s, `UPDATE subscription_tokens SET expires_at=now()+interval '30 days'`)
	if w = repairFeed(s, repairToken, "198.51.100.10:1234", "mihomo/1.0"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "subscription disabled") {
		t.Fatalf("a disabled, unexpired subscription must answer disabled, got %d %s", w.Code, w.Body.String())
	}
}
