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

// 验证四位版本号按数值序排在三位之后，且前导零等价版本号（053 与 53）被识别为同号冲突。
func TestMigrationFilesNumericOrderingAndZeroPaddedDuplicate(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"1000_future.sql", "099_old.sql", "999_prev.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := migrationFiles(dir)
	if err != nil {
		t.Fatalf("数值排序加载失败: %v", err)
	}
	got := []string{files[0].version, files[1].version, files[2].version}
	want := []string{"099", "999", "1000"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("迁移顺序错误：得到 %v，期望 %v", got, want)
		}
	}

	dupDir := t.TempDir()
	for _, name := range []string{"053_a.sql", "53_b.sql"} {
		if err := os.WriteFile(filepath.Join(dupDir, name), []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := migrationFiles(dupDir); err == nil || !strings.Contains(err.Error(), "duplicate migration version") {
		t.Fatalf("前导零等价版本号必须报同号冲突，实际: %v", err)
	}
}

// 验证旧库基线补记走单事务函数且主函数使用 defer unlock() 防止异常漏释放锁。
func TestBaselineUsesAtomicTransactionAndDeferredUnlock(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "defer unlock()") {
		t.Fatal("main.go 必须用 defer unlock() 保障异常路径释放迁移锁")
	}
	if !strings.Contains(src, "recordBaselineVersions(ctx, pool, files)") {
		t.Fatal("旧库基线录入必须使用单事务函数 recordBaselineVersions")
	}
	if !strings.Contains(src, "runMain(logger)") {
		t.Fatal("main.go 必须将主流程抽离至 runMain 返回错误，确保失败路径退出前执行 defer unlock()")
	}
}

// 验证数值等价版本匹配（如 053 与 53）、UTF-8 BOM 剥离，避免历史台账等价版本被重放或 BOM 导致语法错。
func TestSelectPendingMigrationsCanonicalVersionAndBOMStrip(t *testing.T) {
	files := []migrationFile{
		{version: "001", name: "001_init.sql"},
		{version: "53", name: "53_index.sql"},
		{version: "060", name: "060_cursors.sql"},
	}
	applied := map[string]struct{}{
		"000_baseline": {},
		"1":            {},
		"053":          {},
	}
	pending := selectPendingMigrations(files, applied)
	if len(pending) != 1 || pending[0].version != "060" {
		t.Fatalf("前导零等价版本应识别为已应用，仅剩 060，实际: %+v", pending)
	}
	withBOM := append([]byte{0xef, 0xbb, 0xbf}, []byte("SELECT 1;")...)
	if got := string(stripUTF8BOM(withBOM)); got != "SELECT 1;" {
		t.Fatalf("stripUTF8BOM 未剥离 UTF-8 BOM，实际: %q", got)
	}
	if got := string(stripUTF8BOM([]byte("SELECT 2;"))); got != "SELECT 2;" {
		t.Fatalf("无 BOM 内容不应被修改，实际: %q", got)
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
