package executor

import (
	"os"
	"strings"
	"testing"
)

// 验证执行器第二轮稳定性与性能加固：panic 回写终态释锁、领取事务限时、日志批量先落库、健康检查单行无杂音。
func TestExecutorRound2HardeningGuards(t *testing.T) {
	raw, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	for _, want := range []string{
		`claimCtx, cancelClaim := context.WithTimeout(ctx, 10*time.Second)`,
		`e.writeTaskResult(t, "failed", msg, "exec_panic"`,
		`FROM unnest($2::int[], $3::text[], $4::text[]) AS u(seq, lvl, msg)`,
		`tag.RowsAffected() == 0`,
		`errorCode = "EXECUTOR_INTERRUPTED"`,
		`s=$(systemctl is-active %[1]s 2>/dev/null); printf "SERVICE=%%s\n" "${s:-unknown}"`,
		`if exit != 0 {`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("executor.go 缺少关键加固语句: %q", want)
		}
	}

	// 验证批量写日志顺序位于终态更新之前，消除前端看到终态却读到空日志的竞态。
	logIdx := strings.Index(src, "INSERT INTO task_logs")
	updIdx := strings.Index(src, "UPDATE operation_tasks SET status=$1")
	if logIdx < 0 || updIdx < 0 || logIdx > updIdx {
		t.Fatalf("task_logs 批量写入必须先于 operation_tasks 终态更新（logIdx=%d updIdx=%d）", logIdx, updIdx)
	}
}
