package collector

import "testing"

func TestResourceStatusHysteresis(t *testing.T) {
	cases := []struct {
		prev      string
		cpu, disk float64
		want      string
	}{
		{"online", 86, 10, "warning"},
		{"online", 80, 10, "online"},   // 没到进入门槛
		{"warning", 80, 10, "warning"}, // 已告警，未低于退出门槛
		{"warning", 74, 10, "online"},
		{"warning", 10, 86, "warning"},
		{"online", 10, 91, "warning"},
		{"offline", 80, 10, "online"},
	}
	for _, c := range cases {
		if got := resourceStatus(c.prev, c.cpu, c.disk); got != c.want {
			t.Errorf("resourceStatus(%q,%v,%v)=%q want %q", c.prev, c.cpu, c.disk, got, c.want)
		}
	}
}
