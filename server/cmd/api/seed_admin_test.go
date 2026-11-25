package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"nodeops/server/internal/auth"
)

func TestSeedAdminOnlyValidatesPasswordForNewUser(t *testing.T) {
	dsn := os.Getenv("HY2_REPAIR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HY2_REPAIR_TEST_DATABASE_URL required (disposable PostgreSQL only)")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("hy2_seed_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); db.Close() })
	if _, err = db.Exec(ctx, "CREATE SCHEMA "+schema+`; CREATE TABLE users(email text PRIMARY KEY, password_hash text NOT NULL, role text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const email = "seed@example.invalid"
	if err = seedAdminUser(ctx, db, logger, email, "short"); err == nil {
		t.Fatal("new administrator accepted short password")
	}
	const password = "fixture-password-12"
	if err = seedAdminUser(ctx, db, logger, email, password); err != nil {
		t.Fatal(err)
	}
	var hash, role string
	if err = db.QueryRow(ctx, `SELECT password_hash,role FROM users WHERE email=$1`, email).Scan(&hash, &role); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || !auth.VerifyPassword(hash, password) {
		t.Fatal("new administrator credentials were not persisted")
	}
	if err = seedAdminUser(ctx, db, logger, email, "short"); err != nil {
		t.Fatal("existing administrator prevented startup:", err)
	}
	var after string
	if err = db.QueryRow(ctx, `SELECT password_hash FROM users WHERE email=$1`, email).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != hash {
		t.Fatal("restart changed existing administrator password")
	}
}
