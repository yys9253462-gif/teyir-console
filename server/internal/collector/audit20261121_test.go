package collector

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShortSSHErrPreservesValidUTF8OnMultibyteBoundary(t *testing.T) {
	// 构造恰好在第 120 字节落在三字节汉字中间的报错信息（40 个汉字 = 120 字节，前置 1 个 ASCII 字母使第 120 字节切在汉字内部）
	raw := "X" + strings.Repeat("故", 45)
	got := shortSSHErr(errors.New(raw))
	if !utf8.ValidString(got) {
		t.Fatalf("shortSSHErr 截断产生了非法 UTF-8 字节序列（写入 PostgreSQL 会报 SQLSTATE 22021）: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长报错应以省略号结尾，实际: %q", got)
	}
}

func TestUpdateNodeStatusLogsNonEmptyErrDetail(t *testing.T) {
	src := sourceOf(t, "collector.go")
	body := funcBody(t, src, "func (c *Collector) updateNodeStatus(")
	if !strings.Contains(body, "errDetail") {
		t.Fatal("updateNodeStatus 函数体完全忽略了 errDetail 诊断信息")
	}
}
