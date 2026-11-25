package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScheduledAuditDoesNotQueuePartialServerLists(t *testing.T) {
	body := funcBlock(t, "scheduler.go", "func (s *Server) scheduledAudit(")
	for _, want := range []string{
		"if err := rows.Scan(&id); err != nil",
		"if err := rows.Err(); err != nil",
		"scheduleFailed(ctx)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scheduledAudit must abort an incomplete server list; missing %q", want)
		}
	}
	if strings.Index(body, "if err := rows.Err()") > strings.Index(body, "var taskIDs []string") {
		t.Fatal("must validate the entire server list before creating audit tasks")
	}
}

// 2026-11-06：已有排队 / 执行中安全检查的主机不再派发；待汇总列表追加而不是覆盖。
func TestScheduledAuditSkipsPendingHostsAndMergesDigest(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const hostA, hostB = "00000000-0000-0000-0000-0000000005c1", "00000000-0000-0000-0000-0000000005c2"
	const queued = "00000000-0000-0000-0000-0000000005c3"
	for _, q := range []string{
		`ALTER TABLE operation_tasks ADD COLUMN operation_type text, ADD COLUMN parameters jsonb NOT NULL DEFAULT '{}'`,
		`CREATE TABLE server_credentials(server_id uuid)`,
		`INSERT INTO servers(id) VALUES ('` + hostA + `'),('` + hostB + `')`,
		`INSERT INTO server_credentials VALUES ('` + hostA + `'),('` + hostB + `')`,
		`INSERT INTO operation_tasks(id,server_id,status,operation_type,parameters) VALUES ('` + queued + `','` + hostA + `','queued','system_preset','{"preset":"security_audit"}')`,
		`INSERT INTO system_settings(key,value) VALUES ('schedule_audit_tasks','["` + queued + `"]')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	s := fixServer(db)
	s.scheduledAudit(ctx)
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM operation_tasks WHERE server_id=$1::uuid`, hostA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a host with a pending audit must not get another one: n=%d err=%v", n, err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM operation_tasks WHERE server_id=$1::uuid`, hostB).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the other host gets its audit: n=%d err=%v", n, err)
	}
	var raw string
	if err := db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_audit_tasks'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) != 2 || ids[0] != queued {
		t.Fatalf("the digest list must keep the earlier task and add the new one, got %s", raw)
	}
}

// 2026-11-06：切换开关只改这一项（原子合并），两次切换不同任务的开关都要留下。
func TestSetScheduleMergesInsteadOfOverwriting(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	set := func(key string, enabled bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/schedules", strings.NewReader(fmt.Sprintf(`{"key":%q,"enabled":%t}`, key, enabled)))
		rec := httptest.NewRecorder()
		s.setSchedule(rec, asUser(req, adminA))
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s: %d %s", key, rec.Code, rec.Body.String())
		}
	}
	set(scheduleJobs[0].Key, false)
	// 模拟另一个管理员在此期间改了库里另一项（读出 → 改 → 写回的旧写法会把它冲掉）。
	if _, err := db.Exec(context.Background(), `UPDATE system_settings SET value = (value::jsonb || jsonb_build_object($1::text, false))::text WHERE key=$2`, scheduleJobs[1].Key, scheduleConfigKey); err != nil {
		t.Fatal(err)
	}
	set(scheduleJobs[2].Key, false)
	cfg, err := s.scheduleConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range scheduleJobs {
		if cfg[j.Key] {
			t.Fatalf("all three switches were turned off, got %v", cfg)
		}
	}
	// 并发丢更新发生在一次请求的「读出」与「写回」之间，行为测试模拟不出来：钉住写法本身 ——
	// 必须在库里按键合并（jsonb ||），不能把读出的整份配置写回去。
	body := funcBlock(t, "scheduler.go", "func (s *Server) setSchedule(")
	if !strings.Contains(body, "|| jsonb_build_object($3::text, $4::boolean)") || strings.Contains(body, "s.putSetting(r.Context(), scheduleConfigKey") {
		t.Fatal("setSchedule must merge the single key in the database, not write back the whole config it read")
	}
}
