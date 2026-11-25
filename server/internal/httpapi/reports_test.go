package httpapi

import (
	"strings"
	"testing"
	"time"
)

func TestNetwork16(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":       "1.2.0.0",
		"1.2.200.9":     "1.2.0.0",
		"8.8.8.8":       "8.8.0.0",
		"2001:db8:1::5": "2001:db8:1::",
		"bad":           "",
	}
	for in, want := range cases {
		if got := network16(in); got != want {
			t.Errorf("network16(%q) = %q, want %q", in, got, want)
		}
	}
	// 手机在同一运营商段内换 IP 不应算成多个网段
	if network16("112.96.1.1") != network16("112.96.250.3") {
		t.Fatal("same /16 must collapse")
	}
}

func TestDetectSharingIncompleteReadsAreNotTreatedAsNoSharing(t *testing.T) {
	body := funcBlock(t, "reports.go", "func (s *Server) detectSharing(")
	for _, want := range []string{
		"rows.Scan(&id, &label, &order, &limit, &raw); err != nil",
		"json.Unmarshal(raw, &seen); err != nil",
		"rows.Err(); err != nil",
		"lrows.Scan(&id, &raw); err != nil",
		"json.Unmarshal(raw, &ips); err != nil",
		"lrows.Err(); err != nil",
		"lrows, err := s.db.Query(ctx",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detectSharing must surface incomplete evidence; missing %q", want)
		}
	}
	liveQuery := strings.Index(body, "lrows, err := s.db.Query(ctx")
	liveError := strings.Index(body[liveQuery:], "if err != nil")
	liveLoop := strings.Index(body[liveQuery:], "for lrows.Next()")
	if liveError < 0 || liveLoop < 0 || liveError > liveLoop {
		t.Fatal("live-snapshot query errors must return before computing a partial result")
	}
}

func TestOrderPeriodDays(t *testing.T) {
	now := time.Now()
	in30 := now.Add(30 * 24 * time.Hour)
	in60 := now.Add(60 * 24 * time.Hour)
	if d := orderPeriodDays(90, nil, &in30); d != 90 {
		t.Fatalf("duration_days wins, got %d", d)
	}
	if d := orderPeriodDays(0, nil, &in30); d != 30 {
		t.Fatalf("new order from expires_at: got %d", d)
	}
	// 续费：旧到期还没到，周期 = 新到期 − 旧到期
	if d := orderPeriodDays(0, &in30, &in60); d != 30 {
		t.Fatalf("renewal extension: got %d", d)
	}
	// 续费：旧的早过期了，从现在算
	past := now.Add(-10 * 24 * time.Hour)
	if d := orderPeriodDays(0, &past, &in30); d != 30 {
		t.Fatalf("lapsed renewal counts from now: got %d", d)
	}
	if d := orderPeriodDays(0, nil, nil); d != 0 {
		t.Fatal("no info must give 0 (revenue not recorded)")
	}
}
