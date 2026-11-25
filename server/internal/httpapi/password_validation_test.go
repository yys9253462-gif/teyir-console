package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

func TestPasswordHandlersRejectInvalidLengthBeforeDatabase(t *testing.T) {
	for _, endpoint := range []string{"create", "reset", "change"} {
		for _, password := range []string{strings.Repeat("密", 4), strings.Repeat("a", 73)} {
			t.Run(endpoint+"/"+strconv.Itoa(len(password)), func(t *testing.T) {
				s := &Server{writeLimiter: newSlidingLimiter(time.Second, 100)}
				body, err := json.Marshal(map[string]string{"email": "fixture@example.invalid", "role": "viewer", "password": password, "new_password": password, "current_password": "fixture-current"})
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, "http://fixture.invalid/", strings.NewReader(string(body)))
				r = r.WithContext(context.WithValue(r.Context(), claimsKey, auth.Claims{Subject: repairFirst, Role: "admin"}))
				r.SetPathValue("id", repairSecond)
				w := httptest.NewRecorder()
				switch endpoint {
				case "create":
					s.createUser(w, r)
				case "reset":
					s.resetUserPassword(w, r)
				case "change":
					s.changePassword(w, r)
				}
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), auth.ValidatePassword(password).Error()) {
					t.Fatal("missing validation reason:", w.Body.String())
				}
			})
		}
	}
}
