package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nodeops/server/internal/ippurity"
)

func monitoringServer() *Server {
	return &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), alertNotifier: newAlertNotifier()}
}

func TestMonitoringResolvedBypassesEveryCooldown(t *testing.T) {
	n := newAlertNotifier()
	now := time.Now()
	ev := alertNotification{Event: "resolved", AlertType: "node_offline", Target: "fixture"}
	for range 3 {
		if !n.admit(ev, now) {
			t.Fatal("recovery suppressed")
		}
	}
	ev.Event = "triggered"
	if !n.admit(ev, now) || n.admit(ev, now.Add(time.Minute)) {
		t.Fatal("trigger cooldown broken")
	}
}

func TestMonitoringAnnouncedOnlyAfterAdmission(t *testing.T) {
	s := monitoringServer()
	persisted := map[string]bool{}
	s.alertNotifier.persist = func(_ context.Context, id string) error { persisted[id] = true; return nil }
	ev := alertNotification{Event: "triggered", AlertType: "node_offline", Target: "fixture"}
	if !s.admitTrigger(context.Background(), "first", ev) {
		t.Fatal("first rejected")
	}
	// 2026-11-03：admitTrigger 只做节流与内存标记；notified_at 要等至少一个渠道送达后才写。
	if persisted["first"] {
		t.Fatal("notified_at must not be written before delivery succeeds")
	}
	s.markDelivered(context.Background(), "first")
	if s.admitTrigger(context.Background(), "second", ev) || s.alertNotifier.takeSent("second", false) || persisted["second"] {
		t.Fatal("suppressed trigger marked sent")
	}
	s.alertNotifier = newAlertNotifier()
	s.alertNotifier.lookup = func(_ context.Context, id string) (bool, error) { return persisted[id], nil }
	if !s.wasAnnounced(context.Background(), "first", true) || s.wasAnnounced(context.Background(), "second", true) {
		t.Fatal("restart lookup broken")
	}
	s.alertNotifier.lookup = func(context.Context, string) (bool, error) { return false, errors.New("database down") }
	if s.wasAnnounced(context.Background(), "first", true) {
		t.Fatal("lookup failure treated as sent")
	}
}

func TestMonitoringHealthRefreshOutsideLockAndPreservesCache(t *testing.T) {
	oldQuery := healthGateQuery
	healthGateCache.Lock()
	oldAt, oldNodes := healthGateCache.at, healthGateCache.nodes
	healthGateCache.at = time.Time{}
	healthGateCache.nodes = map[string]hiddenNode{"old": {NodeID: "old"}}
	healthGateCache.Unlock()
	defer func() {
		healthGateQuery = oldQuery
		healthGateCache.Lock()
		healthGateCache.at, healthGateCache.nodes = oldAt, oldNodes
		healthGateCache.Unlock()
	}()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	healthGateQuery = func(_ *Server, ctx context.Context) (map[string]hiddenNode, error) {
		calls.Add(1)
		if ctx.Err() != nil {
			t.Error("request cancellation leaked")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > healthGateQueryTimeout {
			t.Error("missing query deadline")
		}
		if !healthGateCache.TryLock() {
			t.Error("query holds cache mutex")
		} else {
			healthGateCache.Unlock()
		}
		close(started)
		<-release
		return nil, errors.New("database down")
	}
	s := monitoringServer()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	go func() { defer close(done); s.unhealthyNodes(cancelled) }()
	<-started
	if got := s.unhealthyNodes(context.Background()); got["old"].NodeID != "old" {
		t.Error("refresh discarded old cache")
	}
	close(release)
	<-done
	if got := s.unhealthyNodes(context.Background()); got["old"].NodeID != "old" || calls.Load() != 1 {
		t.Fatal("failure cache not retained", got, calls.Load())
	}
}

func TestMonitoringTelegramChunking(t *testing.T) {
	for _, text := range []string{strings.Repeat("节点一\n", 3000), strings.Repeat("长", 9001), strings.Repeat("🟢 名称\n", 1000)} {
		chunks := splitTelegramMessage(text, telegramChunkLimit)
		for _, chunk := range chunks {
			if len([]rune(chunk)) > telegramChunkLimit || chunk == "" {
				t.Fatalf("bad chunk length %d", len([]rune(chunk)))
			}
		}
		if strings.ReplaceAll(strings.Join(chunks, ""), "\n", "") != strings.ReplaceAll(text, "\n", "") {
			t.Fatal("chunk content lost")
		}
	}
	if len(splitTelegramMessage("abc", 0)) != 1 {
		t.Fatal("invalid limit did not terminate")
	}
}

type monitoringTransport func(*http.Request) (*http.Response, error)

func (f monitoringTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMonitoringTelegramRetriesExactlyOnce(t *testing.T) {
	oldHTTP, oldBackoff := telegramHTTP, telegramRetryBackoff
	defer func() { telegramHTTP = oldHTTP; telegramRetryBackoff = oldBackoff }()
	telegramRetryBackoff = time.Millisecond
	for _, status := range []int{0, 429, 500, 403, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			telegramHTTP = &http.Client{Transport: monitoringTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.telegram.org" {
					t.Fatal("unexpected host")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("missing per-attempt timeout")
				}
				if status == 0 {
					return nil, errors.New("fixture network failure")
				}
				body := `{"ok":false,"description":"fixture failure"}`
				if status == 200 {
					body = `{"ok":true,"result":{}}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			s := monitoringServer()
			s.purityKeys = ippurity.NewKeyStore(map[string]string{ippurity.KeyTelegram: "fixture-token"})
			err := s.sendTelegram(context.Background(), "12345", "fixture")
			want := 2
			if status == 403 || status == 200 {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls %d want %d", calls, want)
			}
			if (err == nil) != (status == 200) {
				t.Fatal("wrong error", err)
			}
		})
	}
}

func TestMonitoringTelegramRetryRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_ = sendWithOneRetry(ctx, func(context.Context) error { calls++; cancel(); return &telegramRetryableError{msg: "fixture"} })
	if calls != 1 {
		t.Fatal("retried cancelled request")
	}
	if _, ok := telegramRetryWait(&telegramRetryableError{after: telegramRetryMaxWait + time.Second}); ok {
		t.Fatal("excessive retry_after accepted")
	}
}

func TestMonitoringXUIRedirectPolicy(t *testing.T) {
	var targetHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/one", 302)
		case "/one":
			http.Redirect(w, r, "/two", 302)
		case "/two":
			http.Redirect(w, r, "/done", 302)
		case "/cross":
			http.Redirect(w, r, other.URL, 302)
		case "/loop":
			http.Redirect(w, r, "/loop", 302)
		default:
			fmt.Fprint(w, `{"success":true}`)
		}
	}))
	defer server.Close()
	base := server.Client()
	base.Timeout = time.Nanosecond
	client := xuiHTTPClient(base)
	if base.Timeout != time.Nanosecond || client.Timeout != 0 || client.Transport != base.Transport {
		t.Fatal("base client mutated or transport replaced")
	}
	for _, tc := range []struct {
		path string
		fail bool
	}{{"/start", false}, {"/cross", true}, {"/loop", true}} {
		_, _, _, err := xuiCall(context.Background(), client, xuiRequest{method: "GET", url: server.URL + tc.path})
		if (err != nil) != tc.fail {
			t.Fatalf("%s: %v", tc.path, err)
		}
	}
	if targetHits.Load() != 0 {
		t.Fatal("cross-host request leaked")
	}
	origin := httptest.NewRequest("GET", "https://panel.test/start", nil)
	for _, raw := range []string{"http://panel.test/end", "https://panel.test:8443/end"} {
		if xuiCheckRedirect(httptest.NewRequest("GET", raw, nil), []*http.Request{origin}) == nil {
			t.Fatal("unsafe redirect accepted", raw)
		}
	}
}

func TestMonitoringSchedulerRecoversWorkPanic(t *testing.T) {
	s := monitoringServer()
	state := &scheduleRunState{}
	ctx := context.WithValue(context.Background(), scheduleRunKey{}, state)
	s.runScheduleWork(ctx, scheduleJob{Key: "fixture", run: func(*Server, context.Context) string { panic("fixture") }})
	if state.Failed != 1 {
		t.Fatal("panic not recorded as failure")
	}
}

func TestMonitoringPersistenceAndHealthSQL(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	s.alertNotifier = newAlertNotifier()
	ctx := context.Background()
	_, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN status text DEFAULT 'online';
 CREATE TABLE alerts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,resolved_at timestamptz);
 CREATE TABLE node_block_checks(node_id uuid PRIMARY KEY,blocked boolean,checked_at timestamptz);
 INSERT INTO hysteria_nodes(id,name,server_id) VALUES('00000000-0000-0000-0000-000000000102','fixture','00000000-0000-0000-0000-000000000103');`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../migrations/049_alert_notified_at.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = db.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	var id string
	err = db.QueryRow(ctx, `INSERT INTO alerts(node_id,server_id,type,status,first_seen_at,resolved_at)VALUES($1::uuid,$2::uuid,'node_offline','resolved',now()-interval '1 hour',now()-interval '1 minute') RETURNING id::text`, fixNode, fixHost).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if !s.admitTrigger(ctx, id, alertNotification{Event: "triggered", AlertType: "node_offline", Target: "fixture"}) {
		t.Fatal("trigger denied")
	}
	s.markDelivered(ctx, id)
	s.alertNotifier = newAlertNotifier()
	if !s.wasAnnounced(ctx, id, true) {
		t.Fatal("persisted mark lost after restart")
	}
	for _, kind := range []string{"node_offline", "node_blocked"} {
		if _, err = db.Exec(ctx, `UPDATE alerts SET type=$1,resolved_at=now()-interval '1 minute'`, kind); err != nil {
			t.Fatal(err)
		}
		bad, err := s.queryUnhealthyNodes(ctx)
		if err != nil || !strings.Contains(bad[fixNode].Reason, "观察") {
			t.Fatal("recently resolved not held", bad, err)
		}
		if _, err = db.Exec(ctx, `UPDATE alerts SET resolved_at=now()-interval '11 minutes'`); err != nil {
			t.Fatal(err)
		}
		bad, err = s.queryUnhealthyNodes(ctx)
		if err != nil || len(bad) != 0 {
			t.Fatal("hold never released", bad, err)
		}
	}
	if _, err = db.Exec(ctx, `UPDATE alerts SET type='node_blocked',status='open'`); err != nil {
		t.Fatal(err)
	}
	// 「数据过期维持原判」只在探测点仍在时成立（2026-11-06）：没配探测点时旧告警不再维持。
	_, err = db.Exec(ctx, `INSERT INTO node_block_checks VALUES($1::uuid,true,now()-interval '2 days')`, fixNode)
	if err != nil {
		t.Fatal(err)
	}
	if cond, err := s.blockedNodeConditions(ctx); err != nil || len(cond) != 0 {
		t.Fatal("no probe configured: stale verdict must be released", cond, err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO servers(id,name,host) VALUES('00000000-0000-0000-0000-0000000000c1','cn-probe','203.0.113.5');
INSERT INTO system_settings(key,value) VALUES('cn_probe_server_id','00000000-0000-0000-0000-0000000000c1');
DELETE FROM node_block_checks`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO node_block_checks VALUES($1::uuid,true,now()-interval '2 days')`, fixNode)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT 1", `UPDATE node_block_checks SET blocked=NULL,checked_at=now()`, `DELETE FROM node_block_checks`, `UPDATE hysteria_nodes SET status='offline'`} {
		if _, err = db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		cond, err := s.blockedNodeConditions(ctx)
		if err != nil || len(cond) != 1 {
			t.Fatal("stale check cleared prior state", cond, err)
		}
	}
	if _, err = db.Exec(ctx, `UPDATE hysteria_nodes SET status='online'`); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO node_block_checks VALUES($1::uuid,false,now())`, fixNode)
	if err != nil {
		t.Fatal(err)
	}
	cond, err := s.blockedNodeConditions(ctx)
	if err != nil || len(cond) != 0 {
		t.Fatal("fresh success did not clear state", cond, err)
	}
	// 探测点主机被删：同样不再维持旧判定。
	if _, err = db.Exec(ctx, `UPDATE node_block_checks SET blocked=true, checked_at=now()-interval '2 days';
UPDATE servers SET deleted_at=now() WHERE id='00000000-0000-0000-0000-0000000000c1'`); err != nil {
		t.Fatal(err)
	}
	if cond, err := s.blockedNodeConditions(ctx); err != nil || len(cond) != 0 {
		t.Fatal("deleted probe host: stale verdict must be released", cond, err)
	}
}

func TestMonitoringSchedulerWriteFailureAndAtomicState(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()
	_, err := db.Exec(ctx, `ALTER TABLE system_settings ADD CONSTRAINT fixture_reject CHECK(key <> 'schedule_retry_fixture')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.putScheduleSettings(ctx, []scheduleSetting{{"schedule_last_fixture", "done", ""}, {"schedule_retry_fixture", "", ""}}); err == nil {
		t.Fatal("failed write ignored")
	}
	var count int
	db.QueryRow(ctx, `SELECT count(*) FROM system_settings WHERE key='schedule_last_fixture'`).Scan(&count)
	if count != 0 {
		t.Fatal("partial state committed")
	}
	_, err = db.Exec(ctx, `ALTER TABLE system_settings DROP CONSTRAINT fixture_reject`)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.runScheduleJob(ctx, scheduleJob{Key: "fixture", run: func(*Server, context.Context) string { calls++; panic("fixture") }})
	var retry string
	if err = db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_retry_count_fixture'`).Scan(&retry); err != nil || retry != "1" {
		t.Fatal("panic not scheduled for retry", retry, err)
	}
	if _, busy := scheduleRunning.Load("fixture"); busy {
		t.Fatal("panic leaked job lock")
	}
	_, err = db.Exec(ctx, `ALTER TABLE system_settings ADD CONSTRAINT fixture_reject CHECK(key <> 'schedule_attempt_blocked')`)
	if err != nil {
		t.Fatal(err)
	}
	s.runScheduleJob(ctx, scheduleJob{Key: "blocked", run: func(*Server, context.Context) string { calls++; return "" }})
	if calls != 1 {
		t.Fatal("job ran despite failed attempt write")
	}
	// 2026-11-06：另一进程刚占下（租期内）的任务不能再跑；过了租期的残留占用可以接手；跑完要放掉占用。
	if _, err = db.Exec(ctx, `INSERT INTO system_settings(key,value) VALUES('schedule_attempt_held',$1)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	s.runScheduleJob(ctx, scheduleJob{Key: "held", run: func(*Server, context.Context) string { calls++; return "" }})
	if calls != 1 {
		t.Fatal("a job claimed by another process within its lease must not run again")
	}
	if _, err = db.Exec(ctx, `UPDATE system_settings SET value=$1 WHERE key='schedule_attempt_held'`, time.Now().Add(-2*scheduleAttemptLease).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	s.runScheduleJob(ctx, scheduleJob{Key: "held", run: func(*Server, context.Context) string { calls++; return "" }})
	if calls != 2 {
		t.Fatal("a stale claim (crashed process) must be taken over")
	}
	var held string
	if err = db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_attempt_held'`).Scan(&held); err != nil || held != "" {
		t.Fatalf("a finished run must release its claim, got %q %v", held, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.putScheduleSetting(cancelled, "fixture_final", "ok", ""); err != nil {
		t.Fatal("final write used cancelled job context", err)
	}
}

// 2026-11-04：只有真正送达才算「推过」。原来 admitTrigger 就 markSent，投递失败 / 类型被静音时
// 内存仍记着推过，告警恢复时会单独推一条「已恢复」，而故障本身从没通知过。
func TestAnnouncedOnlyAfterDelivery(t *testing.T) {
	s := monitoringServer()
	s.alertNotifier.persist = func(context.Context, string) error { return nil }
	s.alertNotifier.lookup = func(context.Context, string) (bool, error) { return false, nil }
	ev := alertNotification{Event: "triggered", AlertType: "node_offline", Target: "fixture-undelivered"}
	if !s.admitTrigger(context.Background(), "undelivered", ev) {
		t.Fatal("first trigger must be admitted")
	}
	if s.wasAnnounced(context.Background(), "undelivered", false) {
		t.Fatal("an admitted but undelivered alert must not count as announced")
	}
	s.markDelivered(context.Background(), "undelivered")
	if !s.wasAnnounced(context.Background(), "undelivered", true) {
		t.Fatal("a delivered alert must count as announced")
	}
}

// 2026-11-04：复活告警时必须清 notified_at；升级重推只针对未确认的告警，并限制记事件频率。
func TestAlertReopenAndEscalationGuards(t *testing.T) {
	body := funcBlock(t, "manage.go", "func (s *Server) ReconcileAlerts(")
	for _, want := range []string{
		"SET status='open', resolved_at=NULL, last_seen_at=now(), severity=$4, notified_at=NULL",
		"s.alertShouldEscalate(ctx, existingID)",
		`s.alertNotifier.allow("escalate|"+existingID, time.Now())`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reconcileAlerts missing %q", want)
		}
	}
	// 2026-11-06：升级判定搬到 alertShouldEscalate（行为见 TestAlertShouldEscalate）。
	if esc := funcBlock(t, "manage.go", "func (s *Server) alertShouldEscalate("); !strings.Contains(esc, "SELECT notified_at IS NULL AND status = 'open' AND first_seen_at") {
		t.Error("alertShouldEscalate must still require an undelivered, open alert")
	}
	notify := funcBlock(t, "manage.go", "func (s *Server) notifyAlertFor(")
	if !strings.Contains(notify, `(eventType == "repeated" && status != "open")`) {
		t.Fatal("repeated pushes must re-check status (acknowledged alerts are not escalated)")
	}
}

// 2026-11-04：节点监控页一行一个节点，24h 流量必须按节点取，不能按主机汇总后复制到每个节点。
func TestNodeMonitoringTrafficIsPerNode(t *testing.T) {
	body := funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring(")
	if strings.Contains(body, "WHERE hn.server_id = s.id AND ts.sampled_at") || strings.Contains(body, "WHERE pn.server_id = s.id AND p.source = 'portal_used'") {
		t.Fatal("per-node rows must not carry the whole host's traffic")
	}
	for _, want := range []string{"WHERE ts.node_id = n.id AND ts.sampled_at", "WHERE p.node_id = n.id AND p.source = 'portal_used'"} {
		if !strings.Contains(body, want) {
			t.Errorf("nodeMonitoring missing %q", want)
		}
	}
}

// 2026-11-04：测速结果读取失败必须告诉前端，不能让每张卡片都显示成「未测得」。
func TestNodeMonitoringReportsSpeedtestReadFailure(t *testing.T) {
	body := funcBlock(t, "monitoring.go", "func (s *Server) nodeMonitoring(")
	if !strings.Contains(body, `"speedtest_error": speedtestError`) || !strings.Contains(body, `speedtestError = "`) {
		t.Fatal("a failed speedtest read must be reported as speedtest_error")
	}
}

// 2026-11-04：traffic_samples 的保留期清理必须带 node_id 走唯一键前缀（生产全表扫每批 150ms）。
func TestTrafficSampleRetentionUsesNodeIndex(t *testing.T) {
	body := funcBlock(t, "retention.go", "func (s *Server) pruneTrafficSamples(")
	if !strings.Contains(body, "WHERE node_id = $3::uuid AND sampled_at < $1 LIMIT $2") || !strings.Contains(body, "SELECT id::text FROM hysteria_nodes`") {
		t.Fatal("traffic_samples must be pruned per node (soft-deleted nodes included)")
	}
	if !strings.Contains(funcBlock(t, "retention.go", "func (s *Server) pruneSamples("), `t.table == "traffic_samples"`) {
		t.Fatal("pruneSamples must route traffic_samples to the per-node path")
	}
}
