package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

func TestTrafficCycleStart(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, trafficCycleZone)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		now  string
		day  int
		want string
	}{
		{"2026-10-14 12:00", 1, "2026-10-01 00:00"},
		{"2026-10-14 12:00", 15, "2026-09-15 00:00"}, // 还没到本月重置日 → 上月
		{"2026-10-15 00:00", 15, "2026-10-15 00:00"}, // 重置日 0 点整属于新周期
		{"2026-03-10 08:00", 31, "2026-02-28 00:00"}, // 2 月没有 31 号 → 取月末
		{"2026-01-05 08:00", 10, "2025-12-10 00:00"}, // 跨年
		{"2026-10-31 23:59", 31, "2026-10-31 00:00"},
	}
	for _, c := range cases {
		got := trafficCycleStart(at(c.now), c.day)
		if !got.Equal(at(c.want)) {
			t.Errorf("cycleStart(%s, day=%d) = %s, want %s", c.now, c.day, got.In(trafficCycleZone).Format("2006-01-02 15:04"), c.want)
		}
	}
	// 北京时间重置：UTC 9-30 16:00 已经是北京 10-01 0 点。
	if got := trafficCycleStart(time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC), 1); !got.Equal(at("2026-10-01 00:00")) {
		t.Errorf("reset must follow Beijing time, got %s", got)
	}
}

func TestTrafficDelta(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	if rx, tx := trafficDelta(nil, nil, "", 500, 700, "b1"); rx != 0 || tx != 0 {
		t.Errorf("first sample must only set a baseline, got %d/%d", rx, tx)
	}
	if rx, tx := trafficDelta(i(100), i(200), "b1", 150, 260, "b1"); rx != 50 || tx != 60 {
		t.Errorf("same boot: want 50/60, got %d/%d", rx, tx)
	}
	if rx, tx := trafficDelta(i(100), i(200), "b1", 30, 40, "b2"); rx != 30 || tx != 40 {
		t.Errorf("reboot: counters since boot are the delta, got %d/%d", rx, tx)
	}
	if rx, tx := trafficDelta(i(100), i(200), "b1", 90, 260, "b1"); rx != 90 || tx != 60 {
		t.Errorf("only rx went backwards: rx resets, tx keeps its delta, got %d/%d", rx, tx)
	}
	// 读不到 boot_id 时不能每分钟把开机以来的全量重复计入。
	if rx, tx := trafficDelta(i(100), i(200), "", 150, 260, ""); rx != 50 || tx != 60 {
		t.Errorf("empty boot id: want 50/60, got %d/%d", rx, tx)
	}
	if rx, tx := trafficDelta(i(100), i(200), "", 30, 40, ""); rx != 30 || tx != 40 {
		t.Errorf("empty boot id + counters dropped: treat as reboot, got %d/%d", rx, tx)
	}
	// 计量网卡变了：只重建基线。
	if rx, tx := trafficDelta(i(100), i(200), trafficBootKey("b1", []string{"eth0"}), 9000, 9000, trafficBootKey("b1", []string{"ens3"})); rx != 0 || tx != 0 {
		t.Errorf("interface change must only reset the baseline, got %d/%d", rx, tx)
	}
	// 旧数据只有 boot_id：升级后第一轮照常取差值。
	if rx, tx := trafficDelta(i(100), i(200), "b1", 150, 260, trafficBootKey("b1", []string{"eth0"})); rx != 50 || tx != 60 {
		t.Errorf("legacy key: want 50/60, got %d/%d", rx, tx)
	}
	if got := trafficBootKey("b1", []string{"ens3", "eth0"}); got != trafficBootKey("b1", []string{"eth0", "ens3"}) {
		t.Errorf("interface order must not matter: %s", got)
	}
}

// 只改重置日不能清掉本周期已用量；即时计量与循环计量要串行。
func TestTrafficResetDayChangeKeepsUsage(t *testing.T) {
	body := funcBlock(t, "nodetraffic.go", "func (s *Server) updateNodeTrafficLimit(")
	if !strings.Contains(body, "traffic_reset_day <> $3") || !strings.Contains(body, "s.meterNodeByID(ctx, id)") {
		t.Error("updateNodeTrafficLimit must move cycle start on reset-day change and meter through the per-node lock")
	}
	loop := funcBlock(t, "nodetraffic.go", "func (s *Server) meterNodeTraffic(")
	if !strings.Contains(loop, "s.meterNodeByID(ctx, id)") {
		t.Error("meter loop must load each node fresh under the per-node lock")
	}
	for _, want := range []string{"rows.Scan(&id); err != nil", "skipping round", "rows.Err()"} {
		if !strings.Contains(loop, want) {
			t.Errorf("traffic meter must not process a partial node inventory; missing %q", want)
		}
	}
}

// 超额时停服、未超额时恢复；节点侧状态与库里不一致也要纠正。
func TestMeterEnforcesLimitBothWays(t *testing.T) {
	body := funcBlock(t, "nodetraffic.go", "func (s *Server) meterOneNode(")
	// 2026-11-06：恢复条件只认**面板自己的**标记（n.suspended）。原来还接受 reading.Suspended：
	// 运维在门户里手工停掉的 hy2 会在 60 秒内被面板拉起，并写一条 node_traffic_resume 审计。
	for _, want := range []string{
		"over := n.limit != nil && cycleBytes >= *n.limit",
		"over && (!n.suspended || !reading.Suspended)",
		"!over && n.suspended:",
		"!over && !n.suspended && reading.Suspended:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("meterOneNode missing %q", want)
		}
	}
}

type hangingTraffic struct{ delay time.Duration }

func (h hangingTraffic) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(h.delay):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return nil, errors.New("unreachable")
}

// 2026-11-04：计量要并发 —— 几台不可达的节点不能把整轮拖到超出一分钟的周期。
func TestMeterNodeTrafficRunsInParallel(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile(filepath.Join("../../migrations", "033_node_traffic_limit.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("migration 033: %v", err)
	}
	box, _ := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cipher, _ := box.Seal([]byte("fixture-node-api-key-at-least-16"))
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES('`+fixHost+`')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		id := "00000000-0000-0000-0000-00000000060" + string(rune('0'+i))
		if _, err := db.Exec(ctx, `INSERT INTO hysteria_nodes(id,name,api_url,api_key,server_id,listen_port) VALUES($1::uuid,$1,'https://8.8.8.8:8443',$2,$3::uuid,443+$4::int)`, id, cipher, fixHost, i); err != nil {
			t.Fatal(err)
		}
	}
	s := fixServer(db)
	s.box = box
	old := portalClient
	portalClient = &http.Client{Transport: hangingTraffic{delay: time.Second}}
	t.Cleanup(func() { portalClient = old })
	start := time.Now()
	s.meterNodeTraffic(ctx)
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("three slow nodes must be metered concurrently, took %v", took)
	}
	var withErr int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM hysteria_nodes WHERE traffic_error <> ''`).Scan(&withErr); err != nil || withErr != 3 {
		t.Fatalf("every node must record its metering failure, got %d err=%v", withErr, err)
	}
}
