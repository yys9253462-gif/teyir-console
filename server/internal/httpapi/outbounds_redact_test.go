package httpapi

import (
	"strings"
	"testing"
)

// 2026-11-06：URL 解析失败（密码里有非法的 %）时，代理账号密码不能原样回给只读用户。
func TestRedactProxyURLNeverLeaksCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://user:p%ss@proxy.example:8080",
		"http://user:secret@proxy.example:8080",
		"socks5://u:pa%zzss@1.2.3.4:1080",
	} {
		got := redactProxyURL(raw)
		if strings.Contains(got, "ss@") || strings.Contains(got, "secret") || strings.Contains(got, "pa%zz") {
			t.Fatalf("redactProxyURL(%q) leaked credentials: %q", raw, got)
		}
		if !strings.Contains(got, "proxy.example") && !strings.Contains(got, "1.2.3.4") {
			t.Fatalf("redactProxyURL(%q) lost the host: %q", raw, got)
		}
	}
	if got := redactProxyURL("http://proxy.example:8080"); got != "http://proxy.example:8080" {
		t.Fatalf("a URL without credentials must be unchanged, got %q", got)
	}
}

// 2026-11-06：代理服务列表与节点概览都必须对只读账号脱敏。
func TestProxyServicesRedactedForViewerInBothViews(t *testing.T) {
	services := []portalProxyService{{Password: "p", URL: "socks5://u:p@h:1", Format: "h:1:u:p"}}
	redactProxyServicesForRole(services, "operator")
	if services[0].Password == "" {
		t.Fatal("operators keep the credentials")
	}
	redactProxyServicesForRole(services, "viewer")
	if services[0].Password != "" || services[0].URL != "" || services[0].Format != "" {
		t.Fatalf("viewer must not see proxy credentials: %+v", services[0])
	}
	for _, fn := range []struct{ file, sig string }{
		{"portalproxy.go", "func (s *Server) portalProxyServices("},
		{"portaloverview.go", "func (s *Server) portalOverview("},
	} {
		if body := funcBlock(t, fn.file, fn.sig); !strings.Contains(body, "redactProxyServicesForRole(") {
			t.Fatalf("%s must redact proxy services by role", fn.sig)
		}
	}
}
