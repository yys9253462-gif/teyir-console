package httpapi

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodeops/server/internal/auth"
	"nodeops/server/internal/secrets"
)

// 2026-11-06：订阅锁的失败要分开回：被占用 409、本进程锁槽满 503、数据库连不上 500 且不回显连接信息。
func TestSubscriptionLockErrorsAreClassified(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()

	_, release, err := s.lockSubscription(ctx, fixSub)
	if err != nil {
		t.Fatal(err)
	}
	other := fixServer(db)
	_, _, busyErr := other.lockSubscription(context.WithValue(ctx, subscriptionLockKey{}, "someone-else"), fixSub)
	release()
	rec := httptest.NewRecorder()
	s.writeSubscriptionLockError(rec, httptest.NewRequest(http.MethodPost, "/x", nil), busyErr)
	if rec.Code != http.StatusConflict {
		t.Fatalf("锁被占用应回 409，实际 %d %s (err=%v)", rec.Code, rec.Body.String(), busyErr)
	}

	rec = httptest.NewRecorder()
	s.writeSubscriptionLockError(rec, httptest.NewRequest(http.MethodPost, "/x", nil), errSubscriptionLockSlots)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("锁槽满应回 503 + Retry-After，实际 %d", rec.Code)
	}

	// 指向一个不可达地址的连接池：pgxpool.New 不会立即连，lockSubscription 里的 pgx.Connect 才会失败。
	dead, err := pgxpool.New(ctx, "postgres://nodeops:secretpw@127.0.0.1:1/nodeops?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	broken := &Server{db: dead, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, dbErr := broken.lockSubscription(ctx, fixSub)
	if dbErr == nil {
		t.Fatal("连不上数据库时取锁必须失败")
	}
	rec = httptest.NewRecorder()
	broken.writeSubscriptionLockError(rec, httptest.NewRequest(http.MethodPost, "/x", nil), dbErr)
	body := rec.Body.String()
	if rec.Code != http.StatusInternalServerError || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "nodeops") || strings.Contains(body, "正在同步或回收") {
		t.Fatalf("数据库错误应回 500 且不回显连接信息、不说成「正在同步」，实际 %d %s", rec.Code, body)
	}
}

// 2026-11-06：「立即检测」只有探测点本身失败才回 502；没设探测点 409；数据库错误 500 且不回显原文。
func TestRunBlockChecksNowStatusCodes(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.runBlockChecksNow(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/block-checks/run", nil), adminA))
		return rec
	}
	if rec := call(); rec.Code != http.StatusConflict {
		t.Fatalf("没设国内探测点应回 409，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 设了探测点，但节点表查询会失败（fixDB 里的 hysteria_nodes 没有 api_url 以外的这些依赖不影响；
	// 这里把 system_settings 的值改成非法 UUID，让 ::uuid 转换报错 —— 一个纯数据库错误）。
	if _, err := db.Exec(context.Background(), `INSERT INTO system_settings(key,value) VALUES($1,'not-a-uuid')`, cnProbeSettingKey); err != nil {
		t.Fatal(err)
	}
	rec := call()
	if rec.Code == http.StatusBadGateway || strings.Contains(rec.Body.String(), "SQLSTATE") {
		t.Fatalf("数据库错误不能报成 502 或回显原文，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 2026-11-06：3X-UI 入站列表不能把客户端的 UUID / 密码 / subId 回给页面。
func TestXUIInboundsAreRedacted(t *testing.T) {
	in := []xuiInboundSummary{{ID: 1, Protocol: "vless", Clients: []xuiClientSnapshot{
		{"email": "a@b.com", "id": "11111111-2222-3333-4444-555555555555", "subId": "sub123", "flow": "xtls-rprx-vision", "up": 1, "down": 2, "totalGB": 0, "expiryTime": 0},
		{"email": "c@d.com", "password": "trojan-secret", "enable": true},
	}}}
	out := redactXUIInbounds(in)
	for _, c := range out[0].Clients {
		for _, k := range []string{"id", "password", "subId", "flow"} {
			if _, ok := c[k]; ok {
				t.Fatalf("入站客户端不能带 %s：%v", k, c)
			}
		}
	}
	if out[0].Clients[0]["email"] != "a@b.com" || out[0].Clients[0]["up"] != 1 || len(out[0].Clients) != 2 {
		t.Fatalf("展示字段要保留：%v", out[0].Clients)
	}
	if _, ok := in[0].Clients[0]["id"]; !ok {
		t.Fatal("不能改动原始数据（删除客户端时还要用）")
	}
	body := funcBlock(t, "xuihandlers.go", "func (s *Server) xuiInbounds(")
	if !strings.Contains(body, `"inbounds":   redactXUIInbounds(probe.Inbounds)`) || !strings.Contains(body, `isAdmin := claims.Role == "admin"`) {
		t.Fatal("xuiInbounds 必须返回脱敏后的入站，且只给管理员 client_id")
	}
}

// 2026-11-06：请求等计量锁有上限，取不到回 409，客户端断开时立即放弃。
func TestTrafficMeterRequestLockIsBounded(t *testing.T) {
	old := trafficMeterRequestWait
	trafficMeterRequestWait = 300 * time.Millisecond
	t.Cleanup(func() { trafficMeterRequestWait = old })
	const node = "00000000-0000-0000-0000-0000000000d1"
	v, _ := trafficMeterLocks.LoadOrStore(node, &sync.Mutex{})
	v.(*sync.Mutex).Lock()
	defer v.(*sync.Mutex).Unlock()

	start := time.Now()
	rec := httptest.NewRecorder()
	if _, ok := lockTrafficMeterForRequest(rec, httptest.NewRequest(http.MethodPut, "/x", nil), node); ok {
		t.Fatal("锁被占着时不能拿到")
	}
	if rec.Code != http.StatusConflict || time.Since(start) > 2*time.Second {
		t.Fatalf("应在上限内回 409，实际 %d，用时 %v", rec.Code, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec = httptest.NewRecorder()
	if _, ok := lockTrafficMeterForRequest(rec, httptest.NewRequest(http.MethodPut, "/x", nil).WithContext(ctx), node); ok || rec.Code != statusClientClosedRequest {
		t.Fatalf("客户端已断开应立即放弃，实际 ok=%v code=%d", ok, rec.Code)
	}
}

func TestTrafficMeterRequestLockAcquiresWhenFree(t *testing.T) {
	unlock, ok := lockTrafficMeterForRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/x", nil), "00000000-0000-0000-0000-0000000000d2")
	if !ok {
		t.Fatal("空闲时应立即拿到锁")
	}
	unlock()
}

type proxyListPortal struct{}

func (proxyListPortal) RoundTrip(r *http.Request) (*http.Response, error) {
	// portalCall 把整个响应体当作 Data（不是嵌套的 data 字段）。
	body := `{"ok":true,"count":1,"host":"203.0.113.9","services":[{"id":"abcdef123456","type":"socks5","port":1080,"username":"u1","password":"proxy-secret","url":"socks5://u1:proxy-secret@203.0.113.9:1080","format":"203.0.113.9:1080:u1:proxy-secret"}]}`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// 2026-11-06：只读账号看不到代理服务的密码与连接串；运维照常可见。
func TestProxyServicePasswordsHiddenFromViewer(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.readLimiter = newSlidingLimiter(time.Minute, 1000)
	s.box, _ = secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cipher, _ := s.box.Seal([]byte("fixture-node-api-key-at-least-16"))
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443',$2,'fixture.test',443,$3::uuid)`, fixNode, cipher, fixHost); err != nil {
		t.Fatal(err)
	}
	old := portalClient
	t.Cleanup(func() { portalClient = old })
	portalClient = &http.Client{Transport: proxyListPortal{}}
	call := func(role string) string {
		req := httptest.NewRequest(http.MethodGet, "/api/nodes/"+fixNode+"/proxy-services", nil)
		req.SetPathValue("id", fixNode)
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: adminA, Role: role}))
		rec := httptest.NewRecorder()
		s.portalProxyServices(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", role, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := call("viewer"); strings.Contains(body, "proxy-secret") || !strings.Contains(body, "abcdef123456") {
		t.Fatalf("只读账号应能看到代理服务、但看不到密码：%s", body)
	}
	if body := call("operator"); !strings.Contains(body, "proxy-secret") {
		t.Fatalf("运维应能看到代理密码：%s", body)
	}
}

// 2026-11-06：GET /api/nodes/{id}/api 与其它门户接口同一套判据：没配加密密钥不 panic，
// 解密失败不报成「database request failed」。
func TestRefreshNodeAPIUsesPortalTarget(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.readLimiter = newSlidingLimiter(time.Minute, 1000)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,domain,listen_port,server_id) VALUES($1::uuid,'n','https://8.8.8.8:8443','\x0102030405060708090a0b0c0d0e0f10111213'::bytea,'fixture.test',443,$2::uuid)`, fixNode, fixHost); err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/nodes/"+fixNode+"/api", nil)
		req.SetPathValue("id", fixNode)
		rec := httptest.NewRecorder()
		s.refreshNodeAPI(rec, asUser(req, adminA))
		return rec
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("没配加密密钥时不能 panic：%v", p)
			}
		}()
		if rec := call(); rec.Code != http.StatusInternalServerError {
			t.Fatalf("没配加密密钥应回 500，实际 %d %s", rec.Code, rec.Body.String())
		}
	}()
	s.box, _ = secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if rec := call(); strings.Contains(rec.Body.String(), "database request failed") {
		t.Fatalf("密钥解密失败不能说成数据库错误：%d %s", rec.Code, rec.Body.String())
	}
}
