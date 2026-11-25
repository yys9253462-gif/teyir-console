package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/sshclient"
)

func TestResultWritebackContextSurvivesOperationTimeout(t *testing.T) {
	taskCtx, cancelTask := context.WithCancel(context.Background())
	cancelTask()
	if taskCtx.Err() == nil {
		t.Fatal("test precondition: task context must be canceled")
	}

	writebackCtx, cancelWriteback := resultWritebackContext()
	defer cancelWriteback()
	if err := writebackCtx.Err(); err != nil {
		t.Fatalf("result writeback context inherited canceled operation context: %v", err)
	}
	if _, ok := writebackCtx.Deadline(); !ok {
		t.Fatal("result writeback context must have a bounded deadline")
	}
}

// 2026-11-03：一条 8 分钟的预设任务不能让其它主机的排队任务一起等。
func TestRunClaimedDoesNotBlockNextTick(t *testing.T) {
	raw, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	i := strings.Index(src, "func (e *Executor) runClaimed(")
	j := strings.Index(src[i:], "\n}\n")
	body := src[i : i+j]
	if strings.Contains(body, "wg.Wait()") {
		t.Fatal("runClaimed must not wait for the whole batch before the next tick")
	}
	if !strings.Contains(src, "if e.inflight.Load() >= maxInflightTasks {") {
		t.Fatal("claimAndRun must cap in-flight tasks")
	}
}

// 2026-11-04：健康检查的端口探测必须用与采集器相同的命令与判据（缺 ss 不能被当成「未监听」）。
func TestHealthCheckPortProbeUsesSharedParser(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if strings.Contains(text, "| head -n1 | wc -l") {
		t.Fatal("the health check must not use the wc-based probe whose exit code hides a missing ss")
	}
	if !strings.Contains(text, "sshclient.PortProbeCommand(*t.nodePort)") || !strings.Contains(text, "sshclient.ParsePortProbe(portOut, portCode)") {
		t.Fatal("the health check must use sshclient.PortProbeCommand / ParsePortProbe")
	}
	// 缺 ss 时（exit 127）判据必须报「探测失败」而不是「未监听」。
	if _, err := sshclient.ParsePortProbe("", 127); err == nil {
		t.Fatal("a missing ss must be reported as a probe failure")
	}
}

// 2026-11-04：结果写回要重试，并且只改仍是 running 的行；预设预算 + 写回预算必须短于回收阈值。
func TestResultWritebackRetriesAndKeepsReaperVerdict(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, "WHERE id=$4 AND status='running'`, status, summary, errorCode, t.id)") {
		t.Fatal("the result write-back must only touch rows that are still running")
	}
	if !strings.Contains(text, "for attempt := 0; ; attempt++ {") {
		t.Fatal("the result write-back must retry within its budget")
	}
	if 8*time.Minute+resultWritebackTimeout >= 10*time.Minute {
		t.Fatal("preset budget plus write-back must stay under the 10-minute reaper threshold")
	}
}
