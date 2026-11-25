package speedtest

import (
	"strings"
	"testing"
)

func TestProbeRequiresTargetStatistics(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"marker only", "PROBE_OK=1", false},
		{"missing target", "PROBE_OK=1\nTARGET=other\nLOSS=0\nAVG=10", false},
		{"empty statistics", "PROBE_OK=1\nTARGET=x\nLOSS=\nAVG=", false},
		{"missing average", "PROBE_OK=1\nTARGET=x\nLOSS=0", false},
		{"total loss", "PROBE_OK=1\nTARGET=x\nLOSS=100\nAVG=", true},
		{"zero loss and latency", "PROBE_OK=1\nTARGET=x\nLOSS=0\nAVG=0", true},
		{"invalid loss", "PROBE_OK=1\nTARGET=x\nLOSS=-1\nAVG=10", false},
		{"nonfinite average", "PROBE_OK=1\nTARGET=x\nLOSS=0\nAVG=NaN", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := ParseProbeOutput(tc.raw, []Target{{Key: "x"}})
			if rs[0].ProbeOK != tc.ok || (!tc.ok && rs[0].ProbeError == "") {
				t.Fatalf("unexpected result: %+v", rs[0])
			}
			if (usableProbeResults(rs) == nil) != tc.ok {
				t.Fatal("round usability must agree with target statistics")
			}
			if tc.name == "total loss" && rs[0].ICMPAvgMs != nil {
				t.Fatal("100% loss must retain unknown latency")
			}
		})
	}
	rs := ParseProbeOutput("PROBE_OK=1\nTARGET=x\nLOSS=100", []Target{{Key: "x"}, {Key: "missing"}})
	if !rs[0].ProbeOK || rs[1].ProbeOK || usableProbeResults(rs) != nil {
		t.Fatalf("per-target isolation: %+v", rs)
	}
}

func TestProbeCommandRejectsSurroundingWhitespace(t *testing.T) {
	for _, whitespace := range []string{" ", "\t", "\n", "\r"} {
		for _, before := range []bool{true, false} {
			wrap := func(s string) string {
				if before {
					return whitespace + s
				}
				return s + whitespace
			}
			for _, target := range []Target{{Key: wrap("x"), Host: "1.1.1.1"}, {Key: "x", Host: wrap("1.1.1.1")}} {
				if _, err := BuildProbeCommand([]Target{target}); err == nil {
					t.Fatalf("accepted whitespace: %+v", target)
				}
			}
		}
	}
	cmd, err := BuildProbeCommand([]Target{{Key: "x", Host: "2001:db8::1"}})
	if err != nil || !strings.Contains(cmd, "x:2001:db8::1") {
		t.Fatalf("valid IPv6: %v", err)
	}
}
