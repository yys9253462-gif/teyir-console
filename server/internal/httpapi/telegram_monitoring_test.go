package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"nodeops/server/internal/ippurity"
)

func TestMonitoringTelegramNodesSendChunksAndUnavailableStats(t *testing.T) {
	db := fixDB(t)
	s := fixServer(db)
	ctx := context.Background()
	_, err := db.Exec(ctx, `ALTER TABLE hysteria_nodes ADD COLUMN status text DEFAULT 'online';
 INSERT INTO system_settings(key,value)VALUES('telegram_chat_id','12345'),('auto_hide_unhealthy_nodes','0');
 INSERT INTO hysteria_nodes(id,name)SELECT gen_random_uuid(),repeat('节点名称',60)||i::text FROM generate_series(1,50)i;`)
	if err != nil {
		t.Fatal(err)
	}
	s.purityKeys = ippurity.NewKeyStore(map[string]string{ippurity.KeyTelegram: "fixture-token"})
	oldHTTP := telegramHTTP
	defer func() { telegramHTTP = oldHTTP }()
	var chunks []string
	telegramHTTP = &http.Client{Transport: monitoringTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatal("unexpected Telegram API", r.URL.Path)
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, body.Text)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	s.handleTelegramMessage(ctx, "12345", "/nodes")
	if len(chunks) < 2 {
		t.Fatal("nodes not chunked", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > telegramChunkLimit {
			t.Fatal("oversize reply")
		}
	}
	// Real PostgreSQL query errors, no fake zero counts.
	if _, err = db.Exec(ctx, `ALTER TABLE hysteria_nodes RENAME TO fixture_nodes;ALTER TABLE servers RENAME TO fixture_servers`); err != nil {
		t.Fatal(err)
	}
	status := s.telegramStatus(ctx)
	if strings.Count(status, "暂不可用") != 3 || strings.Contains(status, "0 条") {
		t.Fatal("failed statistics presented as zero", status)
	}
}
