// Command migrate applies SQL migrations from server/migrations to the configured
// database, exactly once per file, in filename order.
//
// Why this exists: docker-entrypoint-initdb.d only runs on a *fresh* data volume.
// Once a deployment is live, adding 008_*.sql does nothing on restart — the
// container starts, the API serves the old schema, and queries fail at runtime
// with confusing "column does not exist" errors. This runner closes that gap.
//
// Design notes:
//   - Ledger table `schema_migrations` records which files were applied.
//   - Files already recorded are skipped, so re-running is a no-op (idempotent).
//   - The special "legacy baseline" case: a database created before this runner
//     existed has all migrations applied but no ledger rows. Blindly replaying
//     001..007 would fail on the first CREATE TABLE. We detect that case and
//     mark the existing files as already applied instead of re-executing them.
//   - Each migration runs in its own transaction together with its ledger insert,
//     so a failed migration leaves no partial record.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultMigrationsDir = "server/migrations"
	ledgerTable          = "schema_migrations"
	// Baseline version marks the schema state of a pre-runner deployment.
	baselineVersion = "000_baseline"
	connectTimeout  = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		logger.Error("DATABASE_URL is not set")
		os.Exit(1)
	}
	dir := strings.TrimSpace(os.Getenv("MIGRATIONS_DIR"))
	if dir == "" {
		dir = defaultMigrationsDir
	}
	// Only the baseline behaviour needs an explicit switch. Applying files is
	// always the job; this flag exists so operators can decide how to treat a
	// database that predates the ledger.
	assumeBaseline := strings.TrimSpace(os.Getenv("MIGRATE_ASSUME_BASELINE")) == "true"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Migration files are multi-statement SQL (001_init.sql alone creates a dozen
	// tables). pgx's default exec mode is QueryExecModeCacheStatement, which uses
	// the extended protocol and only accepts a single statement per Exec — a
	// multi-statement file would fail on the first semicolon-separated chunk.
	// Simple protocol sends the whole file text to the server, which parses and
	// runs each statement itself. That is exactly what a migration runner wants.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logger.Error("database configuration failed", "error", err)
		os.Exit(1)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Error("database pool failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, connectTimeout)
	if err := pool.Ping(pingCtx); err != nil {
		pingCancel()
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	pingCancel()

	// 2026-11-04：整个迁移过程持有一把会话级 advisory 锁。原来没有任何互斥：
	// compose up 起的 migrate 与手工 `compose run migrate` 同时跑时，两边都会执行同一个待办文件，
	// 第二个的台账写入被 ON CONFLICT 静默吞掉 —— 非幂等的数据修正就执行了两次。
	unlock, err := acquireMigrationLock(ctx, pool)
	if err != nil {
		logger.Error("migration lock failed", "error", err)
		os.Exit(1)
	}
	err = run(ctx, logger, pool, dir, assumeBaseline)
	unlock()
	if err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

// migrationLockKey 是迁移器专用的 advisory 锁键（任意固定值，只要不与业务锁冲突）。
const migrationLockKey int64 = 0x6879326f70736d67 // "hy2opsmg"

// acquireMigrationLock 在一条专用连接上拿会话级 advisory 锁，返回释放函数。
// 锁随连接走：进程崩溃、连接断开时数据库自动释放，不会留下死锁。
func acquireMigrationLock(ctx context.Context, pool *pgxpool.Pool) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lock connection: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		conn.Release()
		return nil, fmt.Errorf("pg_advisory_lock: %w", err)
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(releaseCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey)
		conn.Release()
	}, nil
}

func run(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, dir string, assumeBaseline bool) error {
	files, err := migrationFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migration files found in %s", dir)
	}

	if err := ensureLedger(ctx, pool); err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	// Legacy database: has user tables but no ledger history. Replaying the
	// existing migrations would fail immediately, so record them as done.
	if len(applied) == 0 && !assumeBaseline {
		populated, err := schemaPopulated(ctx, pool)
		if err != nil {
			return err
		}
		if populated {
			// 2026-11-04：提示写反了。默认（未设置 / false）就是记基线；要强制重放得设成 true。
			logger.Warn("existing schema found without migration ledger; recording current files as baseline",
				"hint", "set MIGRATE_ASSUME_BASELINE=true to replay every file instead")
			for _, f := range files {
				if err := recordVersion(ctx, pool, f.version, f.name); err != nil {
					return err
				}
			}
			return nil
		}
	}

	pending := make([]migrationFile, 0, len(files))
	for _, f := range files {
		if _, ok := applied[f.version]; !ok {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		logger.Info("database schema is up to date", "applied", len(applied))
		return nil
	}

	for _, f := range pending {
		if err := applyWithRetry(ctx, logger, pool, f); err != nil {
			return fmt.Errorf("apply %s: %w", f.name, err)
		}
	}
	logger.Info("migrations applied", "count", len(pending))
	return nil
}

type migrationFile struct {
	version string // numeric prefix, used for ordering and ledger keys
	name    string // file name, for logs
	path    string // absolute path on disk
}

// migrationFiles reads dir and returns the SQL files sorted by version. Files
// without a leading numeric prefix are ignored rather than guessed at, so a
// stray README or .gitkeep does not become a migration.
func migrationFiles(dir string) ([]migrationFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".sql") {
			continue
		}
		version, ok := splitVersion(name)
		if !ok {
			continue
		}
		files = append(files, migrationFile{version: version, name: name, path: filepath.Join(dir, name)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	// 2026-11-04：台账按版本号（数字前缀）记账。两个分支各加一个 053_*.sql 时，
	// 后来的那个会被当成「已执行」永远跳过、且没有任何报错 —— 运行时才撞上缺列。
	// 撞号直接失败，逼着改号。
	for i := 1; i < len(files); i++ {
		if files[i].version == files[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %s: %s and %s (rename one of them)", files[i].version, files[i-1].name, files[i].name)
		}
	}
	return files, nil
}

// splitVersion takes "001_init.sql" -> "001". Leading digits only.
func splitVersion(name string) (string, bool) {
	idx := strings.Index(name, "_")
	if idx <= 0 {
		// Also accept "001.sql" (no description part)
		idx = strings.Index(name, ".")
	}
	if idx <= 0 {
		return "", false
	}
	head := name[:idx]
	for _, r := range head {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return head, true
}

func ensureLedger(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version    TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, ledgerTable))
	if err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]struct{}, error) {
	rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT version FROM %s`, ledgerTable))
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan ledger: %w", err)
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

// schemaPopulated reports whether the database already contains application
// tables. Used to distinguish "brand new database" from "pre-existing database
// that simply has no ledger yet". `users` is the anchor because 001_init.sql
// creates it unconditionally.
func schemaPopulated(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'users'
		)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	return exists, nil
}

func recordVersion(ctx context.Context, pool *pgxpool.Pool, version, name string) error {
	_, err := pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (version, name) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING`, ledgerTable),
		version, name)
	if err != nil {
		return fmt.Errorf("record version %s: %w", version, err)
	}
	return nil
}

// applyFile runs one migration and records it in the same transaction.
//
// The pool is configured for the simple protocol (see main), so a file holding
// several statements is sent to the server as one text and executed as a unit.
// Note this means errors report a line position inside the file rather than a
// statement name — acceptable for a one-shot migration runner.
func applyFile(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, f migrationFile) error {
	body, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.name, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		logger.Warn("skipping empty migration", "file", f.name)
		return recordVersion(ctx, pool, f.version, f.name)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Rollback after a successful Commit is a no-op returning ErrTxClosed,
		// which is why the error is deliberately discarded here.
		_ = tx.Rollback(ctx)
	}()

	// Without a lock timeout a migration queued behind a long transaction (the
	// retention VACUUM, a slow report) blocks every later reader and writer of
	// the table until it gets the lock. Fail fast instead and let the caller retry.
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%s'; SET LOCAL statement_timeout = '%s'",
		migrationLockTimeout, migrationStatementTimeout)); err != nil {
		return fmt.Errorf("set timeouts: %w", err)
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (version, name) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING`, ledgerTable),
		f.version, f.name); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	logger.Info("migration applied", "version", f.version, "file", f.name)
	return nil
}

const (
	migrationLockTimeout      = "5s"
	migrationStatementTimeout = "10min"
	migrationLockAttempts     = 4
)

// isLockTimeout reports PostgreSQL's lock_not_available (55P03), raised when
// lock_timeout expires. Only that error is safe to retry: the transaction was
// rolled back before anything was applied.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

func applyWithRetry(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, f migrationFile) error {
	var err error
	for attempt := 1; attempt <= migrationLockAttempts; attempt++ {
		err = applyFile(ctx, logger, pool, f)
		if err == nil || !isLockTimeout(err) || attempt == migrationLockAttempts {
			return err
		}
		wait := time.Duration(attempt) * 3 * time.Second
		logger.Warn("migration lock busy, retrying", "file", f.name, "attempt", attempt, "wait", wait.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}
