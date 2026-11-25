package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokyoTrustedProxyBoundary(t *testing.T) {
	s := &Server{}
	for _, raw := range []string{"10.10.0.4/32", "10.10.0.7/32", "::1/128"} {
		_, block, _ := net.ParseCIDR(raw)
		s.trustedProxyCIDRs = append(s.trustedProxyCIDRs, block)
	}
	cases := []struct{ peer, xff, want string }{
		{"198.51.100.1:123", "1.1.1.1", "198.51.100.1"},
		{"10.10.0.4:123", "203.0.113.20, 10.10.0.7", "203.0.113.20"},
		{"10.10.0.4:123", "1.1.1.1, 203.0.113.20", "203.0.113.20"},
		{"10.10.0.4:123", ", 203.0.113.20", "203.0.113.20"},
		{"10.10.0.4:123", "forged, invalid", "10.10.0.4"},
		{"10.10.0.4:123", "", "10.10.0.4"},
		{"[::1]:123", "2001:db8::42", "2001:db8::42"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://fixture.invalid/", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-For", c.xff)
		r.Header.Set("X-Real-IP", "8.8.8.8")
		if got := s.requestClientIP(r); got != c.want {
			t.Errorf("peer=%s xff=%s got=%s want=%s", c.peer, c.xff, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "http://fixture.invalid/", nil)
	r.RemoteAddr = "10.10.0.4:123"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := (&Server{}).requestClientIP(r); got != "10.10.0.4" {
		t.Fatalf("unconfigured trust accepted header: %s", got)
	}
}

func TestTokyoViewerSubscriptionOptIn(t *testing.T) {
	for _, path := range []string{"/api/subscription", "/api/subscription/preview", "/api/subscriptions", "/api/subscriptions/abc/preview"} {
		if !subscriptionCredentialPath(path) {
			t.Errorf("missing credential gate: %s", path)
		}
		if !viewerMayUseRequest(http.MethodGet, path) {
			t.Errorf("legacy viewer compatibility lost: %s", path)
		}
	}
	for _, path := range []string{"/api/nodes", "/api/sub/view/public-token", "/api/subscriptions-other"} {
		if subscriptionCredentialPath(path) {
			t.Errorf("overbroad gate: %s", path)
		}
	}
}
