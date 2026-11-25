package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 验证 telegram-forms 工具的 -check 校验与冲突参数拦截。
func TestTelegramFormsCLI(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// 1. 正常执行 -check 必须通过。
	cmd := exec.Command("go", "run", "./tools/telegram-forms", "-check")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./tools/telegram-forms -check 失败：%v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "Telegram forms:") {
		t.Fatalf("输出缺少预期路由统计：%s", string(out))
	}

	// 2. 同时传 -check 与 -write 必须返回非零退出码并给出中文提示。
	conflict := exec.Command("go", "run", "./tools/telegram-forms", "-check", "-write")
	conflict.Dir = repoRoot
	conflictOut, conflictErr := conflict.CombinedOutput()
	if conflictErr == nil {
		t.Fatalf("-check 与 -write 同时传入应拒绝执行，实际成功：%s", string(conflictOut))
	}
	if !strings.Contains(string(conflictOut), "不能同时使用") {
		t.Fatalf("参数冲突提示不明确：%s", string(conflictOut))
	}
}
