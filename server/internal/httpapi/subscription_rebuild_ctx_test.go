package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// slowMetaPortal 慢一点回 node/meta，模拟「节点探测进行中」。
type slowMetaPortal struct{ delay time.Duration }

func (p slowMetaPortal) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(p.delay):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	body := `{"ok":true}`
	if strings.HasSuffix(r.URL.Path, "/node/meta") {
		body = `{"ok":true,"meta":{"public_ip":"8.8.8.8","server_name":"fixture.test","listen_port":443}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// 2026-11-04：重建结果进共享缓存，不能被某个请求的取消截断（否则残缺结果缓存 60 秒，所有买家少节点）。
func TestSubscriptionRebuildSurvivesRequestCancel(t *testing.T) {
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
	portalClient = &http.Client{Transport: slowMetaPortal{delay: 300 * time.Millisecond}}
	InvalidateSubscriptionCache()
	t.Cleanup(func() { portalClient = old; InvalidateSubscriptionCache() })

	reqCtx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := s.subscriptionBuildFor(reqCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter must return promptly: %v", err)
	}
	// A new waiter must join the surviving shared build, rather than observing
	// exclusions caused by the first client's cancellation.
	build, err := s.subscriptionBuildFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Nodes) != 1 {
		t.Fatalf("a client disconnect must not drop nodes from the shared build, got nodes=%d excluded=%v", len(build.Nodes), build.Excluded)
	}
}

// missingUserPortal 对 user-config 回 404 User not found（账号已从节点上消失）。
type missingUserPortal struct{}

func (missingUserPortal) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"User not found"}`)), Header: make(http.Header)}, nil
}

// 2026-11-04：映射还在、节点上账号却没了时，拉订阅必须排一次同步（重新开户），而不是让节点永久消失。
func TestMissingNodeAccountQueuesResync(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	basic, _ := box.Seal([]byte("basic-password"))
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,portal_basic_user,portal_basic_password,portal_path_token) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'u',$3,'0123456789abcdef0123456789abcdef')`, fixNode, key, basic); err != nil {
		t.Fatal(err)
	}
	createFixSub(t, db)
	if _, err := db.Exec(ctx, `UPDATE subscription_sync_jobs SET status='synced'`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: missingUserPortal{}}
	t.Cleanup(func() { portalClient = old })
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	build := []subscriptionNode{{ID: fixNode, Name: "n", Host: "8.8.8.8", Port: 443, InStore: true}}
	rec.nodeAccounts = map[string]subscriptionNodeAccount{fixNode: {UserID: "sub_x", Password: "p"}}
	if got := s.subscriptionNodesFor(ctx, build, rec); len(got) != 0 {
		t.Fatalf("a node whose account is gone must not be served, got %d", len(got))
	}
	// 2026-11-06：有候选节点却全部读取失败 = 暂时给不出，下发端回 503，不回一份空配置。
	if got, err := s.subscriptionNodesForChecked(ctx, build, rec); len(got) != 0 || !errors.Is(err, errSubscriptionTemporarilyEmpty) {
		t.Fatalf("all candidates failing must be reported as temporarily empty, got %d nodes err=%v", len(got), err)
	}
	// 真没有候选（范围内没节点）不是错误。
	if got, err := s.subscriptionNodesForChecked(ctx, nil, rec); len(got) != 0 || err != nil {
		t.Fatalf("no candidates is a plain empty subscription, got %d err=%v", len(got), err)
	}
	// 2026-11-06：范围内有节点、但一个账号都还没开好 = 暂时给不出（503），不是空订阅。
	noAccounts := *rec
	noAccounts.nodeAccounts = map[string]subscriptionNodeAccount{}
	if got, err := s.subscriptionNodesForChecked(ctx, build, &noAccounts); len(got) != 0 || !errors.Is(err, errSubscriptionTemporarilyEmpty) {
		t.Fatalf("candidates without any provisioned account must be temporarily empty, got %d err=%v", len(got), err)
	}
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("a missing node account must queue a resync, job status=%s", status)
	}
}

// 2026-11-06：读订阅时数据库出错不是「订阅不存在」：/sub 与交付页都要回 503，不能回 404。
func TestSubscriptionReadErrorIsNotNotFound(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	createFixSub(t, db)
	s := fixServer(db)
	s.publicLimiter = newSlidingLimiter(time.Minute, 1000)
	if _, err := db.Exec(ctx, `ALTER TABLE subscription_tokens RENAME TO subscription_tokens_gone`); err != nil {
		t.Fatal(err)
	}
	const token = "fixture-token-for-customer-only"
	for name, h := range map[string]http.HandlerFunc{"feed": s.subscriptionFeed, "view": s.subscriptionView} {
		r := httptest.NewRequest(http.MethodGet, "/x/"+token, nil)
		r.SetPathValue("token", token)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: a database error must be 503, got %d %s", name, w.Code, w.Body.String())
		}
	}
	if _, err := db.Exec(ctx, `ALTER TABLE subscription_tokens_gone RENAME TO subscription_tokens`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/x/unknown-token-unknown-token", nil)
	r.SetPathValue("token", "unknown-token-unknown-token")
	w := httptest.NewRecorder()
	s.subscriptionFeed(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown token stays 404, got %d", w.Code)
	}
}

// 2026-11-06：没有门户网页凭据的节点给不出客户端参数 —— 要在构建里如实排除并写明原因，不能在下发时静默消失。
func TestSubscriptionBuildExcludesNodesWithoutWebCredential(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port) VALUES($1::uuid,'no-basic','https://8.8.8.8:8443',$2,'fixture.test',443)`, fixNode, cipher); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: slowMetaPortal{delay: time.Millisecond}}
	InvalidateSubscriptionCache()
	t.Cleanup(func() { portalClient = old; InvalidateSubscriptionCache() })
	build, err := s.subscriptionBuildFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Nodes) != 0 || len(build.Excluded) != 1 || !strings.Contains(build.Excluded[0].Reason, "网页凭据") {
		t.Fatalf("a node without web credentials must be excluded with a reason, got nodes=%d excluded=%v", len(build.Nodes), build.Excluded)
	}
}

// wrongPasswordPortal 的 user-config 回一个口令与订阅记录不同的 Hy2 URI。
type wrongPasswordPortal struct{}

func (wrongPasswordPortal) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"ok":true,"uri":"hysteria2://other-password@8.8.8.8:443?sni=fixture.test#n"}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// 2026-11-06：节点上账号口令与订阅记录不一致是确定性失败：不能拿旧缓存兜底，要排同步重新开户。
func TestAccountPasswordMismatchQueuesResyncAndSkipsCache(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	key, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	basic, _ := box.Seal([]byte("basic-password"))
	if _, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN portal_basic_user text, ADD COLUMN portal_basic_password bytea, ADD COLUMN portal_path_token text`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,portal_basic_user,portal_basic_password,portal_path_token) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'u',$3,'0123456789abcdef0123456789abcdef')`, fixNode, key, basic); err != nil {
		t.Fatal(err)
	}
	createFixSub(t, db)
	if _, err := db.Exec(ctx, `UPDATE subscription_sync_jobs SET status='synced'`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: wrongPasswordPortal{}}
	t.Cleanup(func() { portalClient = old })
	rec, err := s.loadSubscriptionByID(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	node := subscriptionNode{ID: fixNode, Name: "n", Host: "8.8.8.8", Port: 443, InStore: true}
	rec.nodeAccounts = map[string]subscriptionNodeAccount{fixNode: {UserID: "sub_x", Password: "p"}}
	// 预先放一份「过期但仍在兜底窗口内」的缓存：原来口令不一致时会拿它继续下发。
	stamped := applySubscriptionAccounts([]subscriptionNode{node}, rec, nil)[0]
	sum := sha256.Sum256([]byte(stamped.ID + "\x00" + stamped.AccountUserID + "\x00" + stamped.Password + "\x00" + stamped.Host + "\x00" + strconv.Itoa(stamped.Port)))
	clientOptionsCache.Lock()
	clientOptionsCache.m[hex.EncodeToString(sum[:])] = clientOptionsEntry{node: stamped, storedAt: time.Now().Add(-clientOptionsFresh - time.Second)}
	clientOptionsCache.Unlock()
	if got := s.subscriptionNodesFor(ctx, []subscriptionNode{node}, rec); len(got) != 0 {
		t.Fatalf("a node whose account password differs must not be served from the stale cache, got %d", len(got))
	}
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM subscription_sync_jobs WHERE subscription_id=$1::uuid`, fixSub).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("a password mismatch must queue a resync, job status=%s", status)
	}
}
