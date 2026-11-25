package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIsLockTimeout(t *testing.T) {
	if !isLockTimeout(fmt.Errorf("exec: %w", &pgconn.PgError{Code: "55P03"})) {
		t.Fatal("wrapped 55P03 not detected")
	}
	if isLockTimeout(&pgconn.PgError{Code: "57014"}) {
		t.Fatal("statement timeout must not be retried")
	}
	if isLockTimeout(errors.New("other")) {
		t.Fatal("plain error treated as lock timeout")
	}
}

// 2026-11-08：迁移遇到持锁的长事务时要在 lock_timeout 后放弃并重试，
// 不能无限排队挡住表上的其它读写；锁释放后重试应当成功。
func TestApplyWithRetryWaitsOutLockHolder(t *testing.T) {
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
	table := fmt.Sprintf("m11_lock_probe_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, "DROP TABLE IF EXISTS "+table)
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (version text PRIMARY KEY, name text, applied_at timestamptz DEFAULT now())", ledgerTable)); err != nil {
		t.Fatal(err)
	}
	version := fmt.Sprintf("9%d", time.Now().UnixNano()%1e9)
	defer pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE version=$1", ledgerTable), version)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()

	path := filepath.Join(t.TempDir(), version+"_probe.sql")
	if err := os.WriteFile(path, []byte("ALTER TABLE "+table+" ADD COLUMN c int;"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := migrationFile{version: version, name: filepath.Base(path), path: path}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 第一次尝试必须在 lock_timeout 内以 55P03 失败，而不是一直等待。
	start := time.Now()
	err = applyFile(ctx, logger, pool, f)
	if !isLockTimeout(err) {
		t.Fatalf("expected lock timeout, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("lock wait not bounded: %s", time.Since(start))
	}
	// 等待期间其它会话仍能读这张表（迁移已放弃排队）。
	if _, err := other.Exec(ctx, "SELECT count(*) FROM "+table); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(2 * time.Second); _ = holder.Rollback(ctx) }()
	if err := applyWithRetry(ctx, logger, pool, f); err != nil {
		t.Fatalf("retry after release failed: %v", err)
	}
}
