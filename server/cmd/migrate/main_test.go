package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 2026-11-04：同号迁移文件必须报错，不能让第二个静默变成「已执行」。
func TestMigrationFilesRejectsDuplicateVersions(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"001_init.sql", "053_a.sql", "053_b.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrationFiles(dir); err == nil || !strings.Contains(err.Error(), "duplicate migration version 053") {
		t.Fatalf("duplicate versions must be rejected, got %v", err)
	}
	if files, err := migrationFiles(filepath.Join("..", "..", "migrations")); err != nil || len(files) == 0 {
		t.Fatalf("the real migrations directory must stay valid: %v (%d files)", err, len(files))
	}
}

// 2026-11-04：迁移器整体持 advisory 锁，第二个进程要等第一个跑完。
func TestMigrationLockSerializesRunners(t *testing.T) {
	dsn := os.Getenv("NODEOPS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NODEOPS_TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	unlock, err := acquireMigrationLock(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Time, 1)
	lockCtx, cancelLock := context.WithCancel(ctx)
	defer cancelLock()
	go func() {
		second, err := acquireMigrationLock(lockCtx, pool)
		if err != nil {
			got <- time.Time{}
			return
		}
		got <- time.Now()
		second()
	}()
	select {
	case <-got:
		unlock()
		t.Fatal("a second runner acquired the migration lock while the first still held it")
	case <-time.After(500 * time.Millisecond):
	}
	released := time.Now()
	unlock()
	select {
	case at := <-got:
		if at.Before(released) {
			t.Fatal("second runner did not wait for the release")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second runner never acquired the lock after release")
	}
}
