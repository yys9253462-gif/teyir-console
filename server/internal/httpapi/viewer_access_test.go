package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

func TestRedactRequestPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/sub/secret-token", want: "/sub/[redacted]"},
		{path: "/api/sub/secret-token", want: "/api/sub/[redacted]"},
		{path: "/api/sub/view/secret-token", want: "/api/sub/view/[redacted]"},
		{path: "/api/servers/123", want: "/api/servers/123"},
		{path: "/sub/", want: "/sub/"},
	}
	for _, tt := range tests {
		if got := redactRequestPath(tt.path); got != tt.want {
			t.Errorf("redactRequestPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestAuthenticatedRejectsViewerWritesBeforeHandler(t *testing.T) {
	signer := auth.NewSigner("test-secret-that-is-long-enough", time.Hour)
	viewerToken, err := signer.Issue("viewer-id", "viewer@example.invalid", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := signer.Issue("admin-id", "admin@example.invalid", "admin")
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{signer: signer, sessionUser: func(_ context.Context, id string) (string, string, int64, error) {
		if id == "viewer-id" {
			return "viewer", "viewer@example.invalid", 0, nil
		}
		return "admin", "admin@example.invalid", 0, nil
	}}
	called := 0
	wrapped := server.authenticated(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	})
	request := func(method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		return rec
	}

	denied := request(http.MethodPost, "/api/servers", viewerToken)
	if denied.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("viewer write: status=%d handler_calls=%d, want 403 and no side effect", denied.Code, called)
	}
	allowed := request(http.MethodGet, "/api/servers", viewerToken)
	if allowed.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("viewer read: status=%d handler_calls=%d, want 204 and one call", allowed.Code, called)
	}
	adminWrite := request(http.MethodPost, "/api/servers", adminToken)
	if adminWrite.Code != http.StatusNoContent || called != 2 {
		t.Fatalf("admin write should remain allowed: status=%d handler_calls=%d", adminWrite.Code, called)
	}
}

func TestViewerAccessPolicy(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		allow  bool
	}{
		{name: "read endpoint", method: http.MethodGet, path: "/api/servers", allow: true},
		{name: "head endpoint", method: http.MethodHead, path: "/api/servers", allow: true},
		{name: "preflight", method: http.MethodOptions, path: "/api/servers", allow: true},
		{name: "create server", method: http.MethodPost, path: "/api/servers", allow: false},
		{name: "delete node", method: http.MethodDelete, path: "/api/nodes/123", allow: false},
		{name: "change own password", method: http.MethodPost, path: "/api/auth/change-password", allow: true},
		{name: "refresh node metadata has side effect", method: http.MethodGet, path: "/api/nodes/123/api", allow: false},
		// 2026-11-03：GET 路由同样匹配 HEAD，HEAD 也会执行这次写库。
		{name: "refresh node metadata via HEAD", method: http.MethodHead, path: "/api/nodes/123/api", allow: false},
		{name: "unrecognized method", method: "TRACE", path: "/api/servers", allow: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := viewerMayUseRequest(tt.method, tt.path); got != tt.allow {
				t.Fatalf("viewerMayUseRequest(%q, %q) = %t, want %t", tt.method, tt.path, got, tt.allow)
			}
		})
	}
}
