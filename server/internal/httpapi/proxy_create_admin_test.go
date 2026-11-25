package httpapi

import (
	"os"
	"strings"
	"testing"
)

// 2026-11-08：在节点上开代理出口与 manage_proxy 同级，创建、删除都只能管理员。
func TestProxyServiceCreateRequiresAdmin(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, want := range []string{
		`s.panelRoute("POST /api/nodes/{id}/proxy-services", s.authenticated(s.requireAdmin(s.portalCreateProxyService)))`,
		`s.panelRoute("DELETE /api/nodes/{id}/proxy-services/{pid}", s.authenticated(s.requireAdmin(s.portalDeleteProxyService)))`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("route guard missing: %s", want)
		}
	}
	if !portalActionAdminOnly("manage_proxy", portalActionSpec{}, nil) {
		t.Fatal("manage_proxy must stay admin-only")
	}
}
