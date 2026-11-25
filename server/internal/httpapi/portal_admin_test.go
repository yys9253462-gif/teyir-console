package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

// 2026-11-03：以 root 跑远端脚本的节点动词与批量升级门户仅限管理员；operator 仍可用开关类动词。
func TestPortalScriptVerbsAreAdminOnly(t *testing.T) {
	for _, c := range []struct {
		verb      string
		params    map[string]string
		adminOnly bool
	}{
		{verb: "reboot", adminOnly: true}, {verb: "upgrade_portal", adminOnly: true}, {verb: "upgrade_core", adminOnly: true}, {verb: "install_warp", adminOnly: true},
		{verb: "install_awg", adminOnly: true}, {verb: "install_xray", adminOnly: true}, {verb: "install_gost", adminOnly: true}, {verb: "manage_outbounds", adminOnly: true},
		{verb: "vmess_disable", adminOnly: true},
		{verb: "vmess_enable", adminOnly: false}, {verb: "set_bbr", adminOnly: false}, {verb: "warp_toggle", adminOnly: false}, {verb: "reality_toggle", adminOnly: false},
		// 2026-11-06：manage_awg 的 update / set_line / stop 以 root 换二进制、让所有客户端重导配置、直接断服务。
		{verb: "manage_awg", params: map[string]string{"action": "update"}, adminOnly: true},
		{verb: "manage_awg", params: map[string]string{"action": "set_line"}, adminOnly: true},
		{verb: "manage_awg", params: map[string]string{"action": "stop"}, adminOnly: true},
		{verb: "manage_awg", params: map[string]string{"action": "peer_add"}, adminOnly: false},
		{verb: "manage_awg", params: map[string]string{"action": "state"}, adminOnly: false},
		// manage_proxy 能在节点上开任意端口出口（按流量改道），与 manage_outbounds 同级。
		{verb: "manage_proxy", params: map[string]string{"action": "create"}, adminOnly: true},
	} {
		spec, ok := portalActions[c.verb]
		if !ok {
			t.Fatalf("unknown verb %s", c.verb)
		}
		if got := portalActionAdminOnly(c.verb, spec, c.params); got != c.adminOnly {
			t.Errorf("%s%s: adminOnly=%v want %v", c.verb, c.params, got, c.adminOnly)
		}
	}

	// 处理器里的判断必须先于任何数据库 / 网络访问：operator 调 reboot 直接 403。
	s := fixServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/x/portal-action", strings.NewReader(`{"verb":"reboot"}`))
	req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
	req.Header.Set("X-Confirm-Dangerous", "yes")
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: "op", Role: "operator"}))
	rec := httptest.NewRecorder()
	s.portalNodeAction(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator reboot: want 403, got %d %s", rec.Code, rec.Body.String())
	}

	src := funcBlock(t, "server.go", "func New(")
	for _, want := range []string{
		`"POST /api/portal-update-batches", s.authenticated(s.requireAdmin(s.startPortalBatch))`,
		`"POST /api/portal-update-batches/{id}/cancel", s.authenticated(s.requireAdmin(s.cancelPortalBatch))`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("route must be admin-only: %s", want)
		}
	}
}

// 2026-11-03：手工开户不能覆盖订阅账号 / 机主账号；绝对续期不能顺手把账号启用。
func TestPortalUserEndpointsProtectManagedAccounts(t *testing.T) {
	s := fixServer(nil)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	for _, uid := range []string{"sub_0f3c2a1b-aaaa-bbbb-cccc-000000000001", "admin_master"} {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/x/users", strings.NewReader(`{"user_id":"`+uid+`"}`))
		req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: "op", Role: "admin"}))
		rec := httptest.NewRecorder()
		s.portalCreateUser(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create %s: want 400, got %d %s", uid, rec.Code, rec.Body.String())
		}
	}
	body := funcBlock(t, "portalusers.go", "func (s *Server) portalRenewUser(")
	if strings.Contains(body, `"status": "active"`) {
		t.Fatal("absolute renew must not re-enable the account")
	}
}

// 2026-11-04：portal-status 原样转发 reality-status，管理员的 uri / uuid / short_id / 二维码不能出现在响应里。
func TestRealityStatusDoesNotExposeAdminCredentials(t *testing.T) {
	upstream := []byte(`{"installed":true,"active":true,"config":{"uri":"vless://ADMIN-UUID@h:443","uuid":"ADMIN-UUID","pub_key":"PK","short_id":"SID","flow":"xtls-rprx-vision","sni":"www.example.com","port":443,"qr_svg":"<svg>"}}`)
	var info portalRealityInfo
	if err := json.Unmarshal(upstream, &info); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(info)
	for _, secret := range []string{"ADMIN-UUID", "SID", "<svg>", "vless://"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("portal-status leaks %q: %s", secret, out)
		}
	}
	if !strings.Contains(string(out), "www.example.com") || !strings.Contains(string(out), "443") {
		t.Fatalf("channel description fields must remain: %s", out)
	}
}

// 2026-11-04：节点用户页不能直接给订阅账号 / 机主账号续期或停用启用（由订阅流程统一管理）。
// 商城的独立身份账号（hy2_*）不受影响。
func TestPortalRenewAndStatusRefuseManagedAccounts(t *testing.T) {
	s := fixServer(nil)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	for _, uid := range []string{"sub_0f3c2a1b-aaaa-bbbb-cccc-000000000001", "admin_master"} {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/x/users/"+uid+"/renew", strings.NewReader(`{"extend_days":30}`))
		req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req.SetPathValue("uid", uid)
		rec := httptest.NewRecorder()
		s.portalRenewUser(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("renew %s: want 400, got %d", uid, rec.Code)
		}
		req = httptest.NewRequest(http.MethodPost, "/api/nodes/x/users/"+uid+"/status", strings.NewReader(`{"active":true}`))
		req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req.SetPathValue("uid", uid)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		rec = httptest.NewRecorder()
		s.portalSetUserStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %s: want 400, got %d", uid, rec.Code)
		}
	}
	if refuseManagedNodeAccount(httptest.NewRecorder(), "hy2_4972d8596ead", "续期") {
		t.Fatal("the shop's per-order hy2_* accounts must stay renewable")
	}
}

// 2026-11-06：operator 用 manage_awg 的 update 能在节点上以 root 替换二进制，必须 403。
func TestOperatorCannotUseRootAwgActions(t *testing.T) {
	s := fixServer(nil)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	for _, body := range []string{
		`{"verb":"manage_awg","params":{"action":"update"}}`,
		`{"verb":"manage_awg","params":{"action":"set_line","line":"3"}}`,
		`{"verb":"manage_proxy","params":{"action":"create","port":"1080"}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/nodes/x/portal-action", strings.NewReader(body))
		req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: "op", Role: "operator"}))
		rec := httptest.NewRecorder()
		s.portalNodeAction(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("operator %s: want 403, got %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

// 2026-11-06：转发给 root 门户的参数必须在面板侧卡住（endpoint 会进客户端配置的多行字段）。
func TestPortalActionParamsValidated(t *testing.T) {
	awg := portalActions["manage_awg"]
	for _, form := range []map[string]string{
		{"action": "peer_add", "endpoint": "vpn.example.com\nInjected=1"},
		{"action": "peer_add", "endpoint": "1.2.3.4:99999"},
		{"action": "peer_add", "endpoint": "bad host"},
		{"action": "set_line", "line": "4"},
		{"action": "set_line", "line": "2; rm -rf /"},
	} {
		if err := validateActionForm(awg, form); err == nil {
			t.Fatalf("must reject %v", form)
		}
	}
	for _, form := range []map[string]string{
		{"action": "peer_add", "endpoint": "vpn.example.com:51820"},
		{"action": "set_line", "line": "3"},
		{"action": "state"},
	} {
		if err := validateActionForm(awg, form); err != nil {
			t.Fatalf("must accept %v: %v", form, err)
		}
	}
	proxy := portalActions["manage_proxy"]
	for _, form := range []map[string]string{
		{"action": "create", "port": "70000"},
		{"action": "create", "port": "abc"},
		{"action": "delete", "id": "1; rm -rf /"},
		{"action": "create", "note": "line1\nline2"},
	} {
		if err := validateActionForm(proxy, form); err == nil {
			t.Fatalf("must reject proxy %v", form)
		}
	}
	if err := validateActionForm(proxy, map[string]string{"action": "delete", "id": "proxy_0123456789ab"}); err != nil {
		t.Fatalf("must accept a portal proxy id: %v", err)
	}
}
