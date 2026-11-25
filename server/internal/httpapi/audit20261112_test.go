package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nodeops/server/internal/auth"
)

// 2026-11-12：非 UUID 的资源标识（系统设置的配置键）不能再让整条审计写入失败。
func TestAuditResourceSplitsNonUUIDKeys(t *testing.T) {
	const id = "00000000-0000-0000-0000-0000000aa001"
	details := map[string]any{"required": true}

	gotID, gotDetails := auditResource(id, details)
	if gotID != id || len(gotDetails) != 1 {
		t.Fatalf("a UUID must pass through untouched, got id=%q details=%v", gotID, gotDetails)
	}
	if gotID, gotDetails = auditResource("", details); gotID != "" || len(gotDetails) != 1 {
		t.Fatalf("an empty id must pass through untouched, got id=%q details=%v", gotID, gotDetails)
	}

	gotID, gotDetails = auditResource("store_require_signature", details)
	if gotID != "" {
		t.Fatalf("a settings key must not reach the uuid column, got %q", gotID)
	}
	if gotDetails["resource_key"] != "store_require_signature" || gotDetails["required"] != true {
		t.Fatalf("the key must be kept in details next to the caller's fields, got %v", gotDetails)
	}
	if _, leaked := details["resource_key"]; leaked {
		t.Fatal("the caller's map must not be modified")
	}

	// 调用方自己已经用了 resource_key 这个键时，不覆盖它的值。
	_, gotDetails = auditResource("schedule_config", map[string]any{"resource_key": "caller"})
	if gotDetails["resource_key"] != "caller" {
		t.Fatalf("an existing resource_key must be kept, got %v", gotDetails)
	}
}

// 2026-11-12：在真实 PostgreSQL 上验证——resource_id 是 uuid 列，系统设置类操作的审计原来整条写不进去（22P02）。
func TestAuditPersistsSettingsChanges(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)
	req := asUser(httptest.NewRequest(http.MethodPut, "/api/settings/store-signature", nil), adminA)

	s.audit(req.Context(), req, "set_store_signature", "system_setting", storeRequireSignatureKey, map[string]any{"required": true})
	s.audit(req.Context(), req, "rotate_store_key", "system_setting", storeSecretKey, nil)
	s.audit(req.Context(), req, "create_task", "operation_task", fixHost, nil)

	var key string
	var hasID, required bool
	if err := db.QueryRow(ctx, `SELECT resource_id IS NOT NULL, details->>'resource_key', COALESCE((details->>'required')::boolean, false)
FROM audit_logs WHERE action='set_store_signature'`).Scan(&hasID, &key, &required); err != nil {
		t.Fatalf("the settings change left no audit row: %v", err)
	}
	if hasID || key != storeRequireSignatureKey || !required {
		t.Fatalf("settings audit row: resource_id set=%v key=%q required=%v", hasID, key, required)
	}
	if err := db.QueryRow(ctx, `SELECT details->>'resource_key' FROM audit_logs WHERE action='rotate_store_key'`).Scan(&key); err != nil || key != storeSecretKey {
		t.Fatalf("store key rotation audit row: key=%q err=%v", key, err)
	}
	var resourceID string
	var extraKey bool
	if err := db.QueryRow(ctx, `SELECT resource_id::text, details ? 'resource_key' FROM audit_logs WHERE action='create_task'`).Scan(&resourceID, &extraKey); err != nil {
		t.Fatal(err)
	}
	if resourceID != fixHost || extraKey {
		t.Fatalf("a UUID resource must keep using the resource_id column: id=%q resource_key present=%v", resourceID, extraKey)
	}
}

// 2026-11-12：重试与新建同一门槛。createTask 已经把写类任务限制为管理员，
// 原来 operator 仍能对着管理员跑过的写类任务点「重试」，按原参数再跑一遍。
func TestOperatorCannotRetryWriteTasks(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	const restartTask = "00000000-0000-0000-0000-0000000b0001"
	const presetTask = "00000000-0000-0000-0000-0000000b0002"
	const healthTask = "00000000-0000-0000-0000-0000000b0003"
	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN node_id uuid, ADD COLUMN operation_type text, ADD COLUMN parameters jsonb NOT NULL DEFAULT '{}';
INSERT INTO servers(id) VALUES('`+fixHost+`');
INSERT INTO operation_tasks(id,server_id,status,operation_type,parameters) VALUES
 ('`+restartTask+`','`+fixHost+`','success','restart','{"unit":"hysteria-server"}'),
 ('`+presetTask+`','`+fixHost+`','success','system_preset','{"preset":"kernel_reboot"}'),
 ('`+healthTask+`','`+fixHost+`','failed','health_check','{}')`); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	s.writeLimiter = newSlidingLimiter(time.Minute, 100)
	retry := func(role, taskID string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/retry", nil)
		req.SetPathValue("id", taskID)
		req.Header.Set("X-Confirm-Dangerous", "yes")
		req = req.WithContext(context.WithValue(req.Context(), claimsKey, auth.Claims{Subject: adminA, Role: role}))
		rec := httptest.NewRecorder()
		s.retryTask(rec, req)
		return rec.Code
	}
	tasks := func() int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM operation_tasks`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, id := range []string{restartTask, presetTask} {
		if code := retry("operator", id); code != http.StatusForbidden {
			t.Fatalf("operator retried write task %s: got %d, want 403", id, code)
		}
	}
	if n := tasks(); n != 3 {
		t.Fatalf("a refused retry must not queue anything, have %d tasks", n)
	}
	// 只读任务的重试对 operator 照常可用；管理员可以重试写类任务。
	if code := retry("operator", healthTask); code != http.StatusCreated {
		t.Fatalf("operator must still be able to retry a read-only task, got %d", code)
	}
	if code := retry("admin", presetTask); code != http.StatusCreated {
		t.Fatalf("admin must still be able to retry a write task, got %d", code)
	}
	if n := tasks(); n != 5 {
		t.Fatalf("want the two allowed retries queued, have %d tasks", n)
	}
}
