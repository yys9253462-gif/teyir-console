package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

func TestNodeEnrollmentRequiresAdmin(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	// Guard the actual registration as well as the middleware behavior below.
	if !strings.Contains(string(source), `s.panelRoute("POST /api/nodes", s.authenticated(s.requireAdmin(s.createNode)))`) {
		t.Fatal("node enrollment must use the same admin gate as node credential binding")
	}
	for _, role := range []string{"admin", "operator", "viewer"} {
		t.Run(role, func(t *testing.T) {
			signer := auth.NewSigner("isolated-node-enrollment-test-secret", time.Hour)
			token, err := signer.Issue("isolated-user", "test@example.invalid", role)
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{signer: signer, sessionUser: func(context.Context, string) (string, string, int64, error) {
				return role, "test@example.invalid", 0, nil
			}}
			calls := 0
			wrapped := server.authenticated(server.requireAdmin(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodPost, "/api/nodes", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			wrapped(response, request)
			if role == "admin" {
				if response.Code != http.StatusNoContent || calls != 1 {
					t.Fatalf("admin: status=%d calls=%d", response.Code, calls)
				}
			} else if response.Code != http.StatusForbidden || calls != 0 {
				t.Fatalf("%s: status=%d calls=%d; want rejection before any enrollment", role, response.Code, calls)
			}
		})
	}
}
