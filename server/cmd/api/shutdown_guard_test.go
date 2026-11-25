package main

import (
	"os"
	"strings"
	"testing"
)

// 验证 API 入口启动期数据库调用均设有限超时，且优雅停机时会取消全部后台周期任务并等待采集器与执行器排空。
func TestMainStartupTimeoutsAndBackgroundDrain(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		"seedCtx, seedCancel := context.WithTimeout(context.Background(), 15*time.Second)",
		"purityLoadCtx, purityLoadCancel := context.WithTimeout(context.Background(), 15*time.Second)",
		"alertCancel()",
		"subscriptionCancel()",
		"trafficCancel()",
		"monitoringCancel()",
		"collectorCancel()",
		"metricsCollector.Start(collectorCtx)",
		"taskExecutor.Wait()",
		"metricsCollector.Wait()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("server/cmd/api/main.go 缺少预期的超时或停机排空语句: %q", want)
		}
	}
	if strings.Contains(src, "go metricsCollector.Run(") {
		t.Error("server/cmd/api/main.go 不得绕过 Start 直接派生 Run，避免启停瞬间与 Wait 竞态")
	}
}
