package collector

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLegacyFieldNeverRecountsMeasuredBytes(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{`{"traffic_used_bytes":3145728,"measured_bytes":3145728,"legacy_auth_value":99}`, 99},
		{`{"traffic_used_bytes":3145728,"measured_bytes":3145728}`, 0},
		{`{"traffic_used_bytes":123}`, 123},
		{`{"legacy_auth_value":-1}`, 0},
	}
	for _, c := range cases {
		var user portalUserUsage
		if err := json.Unmarshal([]byte(c.input), &user); err != nil {
			t.Fatal(err)
		}
		if got := legacyUsageValue(user); got != c.want {
			t.Fatalf("got %d want %d", got, c.want)
		}
	}
}

// 2026-11-04：流量探针失败时整条命令仍要成功退出，并保留 USED_TOTAL、给出 TRAFFIC_ERR。
func TestUsageCommandSurvivesTrafficProbeFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	src := sourceOf(t, "collector.go")
	if !strings.Contains(src, `probe = "{ " + probe + "} || echo TRAFFIC_ERR=$?\n"`) {
		t.Fatal("the traffic probe must be wrapped so its failure cannot fail the whole usage command")
	}
	stubs := t.TempDir()
	if err := os.WriteFile(stubs+"/python3", []byte("#!/bin/sh\ncat >/dev/null\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := "python3 - 'x' 'y' <<'METERPY'\nprint(1)\nMETERPY\n"
	script := "echo USED_TOTAL=42\n" + "{ " + probe + "} || echo TRAFFIC_ERR=$?\n" + "\n"
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the usage command must exit 0 when only the traffic probe fails: %v\n%s", err, out)
	}
	values := parseKeyValues(string(out))
	if values["USED_TOTAL"] != "42" || values["TRAFFIC_ERR"] != "3" {
		t.Fatalf("want USED_TOTAL kept and TRAFFIC_ERR=3, got %q", out)
	}
}
