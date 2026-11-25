package executor

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestExecutorReapsStaleRunningOnStart 钉住「执行器启动时必须回收僵尸任务」。
//
// 背景（2026-10-04 审计发现）：claimAndRun 领取任务时立刻置 status='running'，
// 但全代码库没有任何回收逻辑。进程在任务执行中被重建
// （`docker compose up -d`，日常运维动作）时，那一行永远停在 running：
// claim 只取 queued，于是它再也不会被处理、也永远不会 finished。
// 界面上表现为一条"永远在跑"的任务，没有报错、没有日志。
//
// 这个测试防止回退：只要有人删掉 Run 里的回收调用，它立刻红。
func TestExecutorReapsStaleRunningOnStart(t *testing.T) {
	raw, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("读 executor.go 失败: %v", err)
	}
	src := string(raw)

	// 1) Run 里必须有回收调用
	runIdx := strings.Index(src, "func (e *Executor) Run(ctx context.Context) {")
	if runIdx < 0 {
		t.Fatal("找不到 Executor.Run —— 函数被改名了？本测试需同步更新")
	}
	rest := src[runIdx:]
	nextFunc := strings.Index(rest, "\nfunc ")
	runBody := rest
	if nextFunc >= 0 {
		runBody = rest[:nextFunc]
	}
	if !strings.Contains(runBody, "reapStaleRunning") {
		t.Error("Run() 必须在启动时调用 reapStaleRunning —— " +
			"否则进程重建时卡住的 running 任务会永远留在那里（claim 只取 queued）")
	}

	// 2) 回收实现必须存在，且把 running 置为 failed（不是 requeue）
	if !regexp.MustCompile(`func \(e \*Executor\) reapStaleRunning\(`).MatchString(src) {
		t.Fatal("reapStaleRunning 实现不存在")
	}
	reapIdx := strings.Index(src, "func (e *Executor) reapStaleRunning(")
	reapRest := src[reapIdx:]
	reapBody := reapRest
	if nf := strings.Index(reapRest[1:], "\nfunc "); nf >= 0 {
		reapBody = reapRest[:nf+1]
	}

	if !strings.Contains(reapBody, "status = 'failed'") {
		t.Error("回收应把僵尸任务标成 'failed'，而不是重新排队（requeue）——\n" +
			"这些任务可能已对目标机器产生了**部分副作用**（重启过服务、改过配置），" +
			"自动重跑会让副作用叠加。交回操作者用「重试」按钮处理更安全。")
	}
	if !strings.Contains(reapBody, "status = 'running'") {
		t.Error("回收的 WHERE 条件必须限定 status='running' —— 不能误伤其他状态")
	}
	// 必须带时间条件，否则会把刚落地的 running 任务也一起收掉
	if !strings.Contains(reapBody, "started_at <") {
		t.Error("回收必须带 started_at 超时条件 —— 否则会把正在执行的任务误判为僵尸")
	}
}
