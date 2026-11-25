package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"nodeops/server/internal/ippurity"
)

// fakeTelegram 记录 sendMessage 的文本；超过 4096 字符时像真 Telegram 一样回 400。
type fakeTelegram struct {
	mu    sync.Mutex
	texts []string
	down  bool
}

func (f *fakeTelegram) RoundTrip(r *http.Request) (*http.Response, error) {
	var in struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down || utf8.RuneCountInString(in.Text) > 4096 {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"Bad Request: message is too long"}`)), Header: make(http.Header)}, nil
	}
	f.texts = append(f.texts, in.Text)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
}

// 2026-11-03：长通知原来整段一次发送，被 Telegram 400 拒收；每周安全检查的待汇总列表
// 又在发送之前就清空，发送失败后这周的结果永远丢失。
func TestTelegramLongDigestIsSplitAndKeptUntilDelivered(t *testing.T) {
	db := fixDB(t)
	ctx := context.Background()
	s := fixServer(db)
	s.purityKeys = ippurity.NewKeyStore(map[string]string{ippurity.KeyTelegram: "fixture-token"})
	old, oldBackoff := telegramHTTP, telegramRetryBackoff
	fake := &fakeTelegram{down: true}
	telegramHTTP = &http.Client{Transport: fake}
	telegramRetryBackoff = 0
	t.Cleanup(func() { telegramHTTP, telegramRetryBackoff = old, oldBackoff })

	if _, err := db.Exec(ctx, `ALTER TABLE operation_tasks ADD COLUMN result_summary text;
INSERT INTO system_settings(key,value) VALUES ('telegram_chat_id','42')`); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 120; i++ {
		var id, host string
		if err := db.QueryRow(ctx, `INSERT INTO servers(id,name) VALUES(gen_random_uuid(), 'fixture-host-with-a-long-name-'||$1::int) RETURNING id::text`, i).Scan(&host); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(ctx, `INSERT INTO operation_tasks(server_id,status,result_summary) VALUES($1::uuid,'success','安全评分 40/100') RETURNING id::text`, host).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	raw, _ := json.Marshal(ids)
	if err := s.putSetting(ctx, "schedule_audit_tasks", string(raw), ""); err != nil {
		t.Fatal(err)
	}

	// Telegram 不可用：待汇总列表必须保留，下次再发。
	s.collectAuditDigest(ctx)
	var pending string
	_ = db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_audit_tasks'`).Scan(&pending)
	if pending == "[]" {
		t.Fatal("digest list cleared although delivery failed")
	}

	// 恢复后：长汇总分段送达，列表清空。
	fake.mu.Lock()
	fake.down = false
	fake.mu.Unlock()
	s.collectAuditDigest(ctx)
	_ = db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key='schedule_audit_tasks'`).Scan(&pending)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.texts) < 2 {
		t.Fatalf("long digest must be split into several messages, got %d", len(fake.texts))
	}
	if pending != "[]" {
		t.Fatalf("digest list must be cleared after delivery, got %s", pending)
	}
}
