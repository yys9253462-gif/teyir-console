package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

const adminA = "00000000-0000-0000-0000-0000000aa001"
const adminB = "00000000-0000-0000-0000-0000000aa002"

func adminGuardServer(t *testing.T) *Server {
	t.Helper()
	db := fixDB(t)
	if _, err := db.Exec(context.Background(), `CREATE TABLE users(id uuid PRIMARY KEY, email text NOT NULL UNIQUE, password_hash text NOT NULL DEFAULT '', role text NOT NULL, token_version bigint NOT NULL DEFAULT 0);
INSERT INTO users(id,email,role) VALUES ('`+adminA+`','a@x','admin'),('`+adminB+`','b@x','admin');
CREATE TABLE portal_update_batches(id uuid PRIMARY KEY DEFAULT gen_random_uuid(), created_by uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE, status text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 1000)
	return s
}

func asUser(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), claimsKey, auth.Claims{Subject: id, Role: "admin"}))
}

func demote(s *Server, actor, target string) int {
	req := asUser(httptest.NewRequest(http.MethodPatch, "/api/users/"+target, strings.NewReader(`{"role":"viewer"}`)), actor)
	req.SetPathValue("id", target)
	rec := httptest.NewRecorder()
	s.updateUser(rec, req)
	return rec.Code
}

func adminCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 2026-11-03：本人检查原来按字符串比较，大写 id 能绕过；也没有「至少留一个管理员」的约束。
func TestAdminGuardsSurviveIDFormsAndRaces(t *testing.T) {
	s := adminGuardServer(t)
	if code := demote(s, adminA, strings.ToUpper(adminA)); code == http.StatusOK {
		t.Fatal("an admin demoted themselves through an uppercase id")
	}
	// 并发互相降级：最多只能成功一次。
	for round := 0; round < 5; round++ {
		if _, err := s.db.Exec(context.Background(), `UPDATE users SET role='admin'`); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); demote(s, adminA, adminB) }()
		go func() { defer wg.Done(); demote(s, adminB, adminA) }()
		wg.Wait()
		if n := adminCount(t, s); n < 1 {
			t.Fatalf("round %d: concurrent demotion left %d admins", round, n)
		}
	}
	// 只剩一个管理员时，别人（这里用一个非管理员的测试身份）也不能把他降级或删除。
	if _, err := s.db.Exec(context.Background(), `UPDATE users SET role='viewer' WHERE id=$1::uuid; `, adminB); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1::uuid`, adminA); err != nil {
		t.Fatal(err)
	}
	if code := demote(s, adminB, adminA); code != http.StatusConflict {
		t.Fatalf("demoting the last admin: want 409, got %d", code)
	}
	req := asUser(httptest.NewRequest(http.MethodDelete, "/api/users/"+adminA, nil), adminB)
	req.SetPathValue("id", adminA)
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.deleteUser(rec, req)
	if rec.Code != http.StatusConflict || adminCount(t, s) != 1 {
		t.Fatalf("deleting the last admin: want 409, got %d %s", rec.Code, rec.Body.String())
	}
}
