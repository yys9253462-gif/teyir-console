package collector

import (
	"os"
	"strings"
	"testing"
)

// 2026-11-03：原来 `ss ... | head | wc -l` 的退出码来自 wc，主机缺 ss 时照样输出 0、退出 0，
// 节点被判 offline（probeOK=true），健康门随即把好节点踢出订阅。
func TestParsePortProbeDistinguishesFailureFromOffline(t *testing.T) {
	for _, c := range []struct {
		out       string
		code      int
		listening bool
		failed    bool
	}{
		{"2\n", 0, true, false},
		{"0\n", 1, false, false}, // grep -c 零行退出 1：确定的「没在听」
		{"", 127, false, true},   // 缺 ss
		{"", 2, false, true},     // ss 自身报错
		{"garbage", 0, false, true},
		{"", 0, false, true},
		{"3\n", 1, false, true},
	} {
		listening, err := parsePortProbe(c.out, c.code)
		if listening != c.listening || (err != nil) != c.failed {
			t.Errorf("parsePortProbe(%q,%d) = %v,%v; want listening=%v failed=%v", c.out, c.code, listening, err, c.listening, c.failed)
		}
	}
	cmd := portProbeCommand(443)
	for _, want := range []string{"command -v ss", "exit 127", `"sport = :443"`, "|| exit $?", "grep -c ."} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command missing %q: %s", want, cmd)
		}
	}
	src, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "| head -n1 | wc -l") || !strings.Contains(string(src), "parsePortProbe(stdout, code)") {
		t.Fatal("probeNodes must use portProbeCommand + parsePortProbe")
	}
}
