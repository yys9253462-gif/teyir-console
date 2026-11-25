package collector

import (
	"os"
	"strings"
	"testing"
)

func TestAudit20261122_CollectorIntervalAndDetachedWrite(t *testing.T) {
	c := NewCollector(nil, nil, 0, nil)
	if c.interval <= 0 {
		t.Fatalf("非正采集间隔必须回退为正默认周期以防 NewTicker panic，实际 %v", c.interval)
	}
	raw, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "clampCollectorPercent") {
		t.Fatal("collector.go 必须对采集的 CPU 百分比进行 [0, 100] 钳位")
	}
	if !strings.Contains(src, "WHERE id=$1 AND deleted_at IS NULL") {
		t.Fatal("collector.go 写 servers 状态更新必须带 AND deleted_at IS NULL")
	}
}
