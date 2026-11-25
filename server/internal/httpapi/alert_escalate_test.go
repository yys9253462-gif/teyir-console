package httpapi

import (
	"context"
	"testing"
)

// TestAlertShouldEscalate 钉住 2026-11-06 的两处修复：刚复活（reopened）的告警不立刻升级；
// 新接入静默期内的目标不升级（否则冷却被白白消耗，首条真实通知推迟约 75 分钟）。
func TestAlertShouldEscalate(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `ALTER TABLE servers ADD COLUMN created_at timestamptz DEFAULT now()-interval '30 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE alerts(id uuid PRIMARY KEY DEFAULT gen_random_uuid(),node_id uuid,server_id uuid,type text,status text,first_seen_at timestamptz,notified_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE alert_events(alert_id uuid,event_type text,created_at timestamptz DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO servers(id) VALUES($1::uuid)`, fixHost); err != nil {
		t.Fatal(err)
	}
	s := fixServer(db)
	newAlert := func(firstSeen string, notified bool) string {
		t.Helper()
		var id string
		n := "NULL"
		if notified {
			n = "now()"
		}
		if err := db.QueryRow(ctx, `INSERT INTO alerts(server_id,type,status,first_seen_at,notified_at) VALUES($1::uuid,'server_offline','open',now()-$2::interval,`+n+`) RETURNING id::text`, fixHost, firstSeen).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	event := func(id, kind, ago string) {
		t.Helper()
		if _, err := db.Exec(ctx, `INSERT INTO alert_events(alert_id,event_type,created_at) VALUES($1::uuid,$2,now()-$3::interval)`, id, kind, ago); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name, id string, want bool) {
		t.Helper()
		got, err := s.alertShouldEscalate(ctx, id)
		if err != nil || got != want {
			t.Fatalf("%s: want escalate=%v, got %v err=%v", name, want, got, err)
		}
	}

	plain := newAlert("20 minutes", false)
	event(plain, "triggered", "20 minutes")
	check("持续 20 分钟未送达", plain, true)

	revived := newAlert("20 minutes", false)
	event(revived, "triggered", "20 minutes")
	event(revived, "reopened", "30 seconds")
	check("刚复活的告警", revived, false)

	delivered := newAlert("20 minutes", true)
	check("已送达", delivered, false)

	if _, err := db.Exec(ctx, `UPDATE servers SET created_at=now()-interval '5 minutes'`); err != nil {
		t.Fatal(err)
	}
	check("新接入静默期内的主机", plain, false)
}
