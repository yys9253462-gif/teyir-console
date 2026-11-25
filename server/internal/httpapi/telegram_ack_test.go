package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"nodeops/server/internal/ippurity"
)

// fakeTelegramPoll 记录调用顺序：第一次 getUpdates 交付一条回调，之后一律返回空。
type fakeTelegramPoll struct {
	mu     sync.Mutex
	calls  []string
	served bool
}

func (f *fakeTelegramPoll) RoundTrip(r *http.Request) (*http.Response, error) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	body := `{"ok":true,"result":true}`
	switch method {
	case "getMe":
		body = `{"ok":true,"result":{"username":"fixture_bot"}}`
	case "getUpdates":
		off, _ := in["offset"].(float64)
		timeout, _ := in["timeout"].(float64)
		f.calls = append(f.calls, "getUpdates:"+strings.TrimSuffix(strings.TrimSuffix(jsonNumber(off), ".0"), ".00")+map[bool]string{true: ":ack", false: ":poll"}[timeout == 0])
		// 下一次长轮询（timeout>0）挂住，直到回调已经处理完：只有处理前显式确认（timeout=0）才会出现在回调之前。
		if f.served && timeout > 0 {
			f.mu.Unlock()
			time.Sleep(300 * time.Millisecond)
			f.mu.Lock()
		}
		if !f.served {
			f.served = true
			body = `{"ok":true,"result":[{"update_id":41,"callback_query":{"id":"cb1","data":"tg:noop","from":{"id":7},"message":{"chat":{"id":7}}}}]}`
		} else {
			body = `{"ok":true,"result":[]}`
			time.Sleep(20 * time.Millisecond)
		}
	default:
		f.calls = append(f.calls, method)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func jsonNumber(f float64) string { b, _ := json.Marshal(f); return string(b) }

// 2026-11-04：一批更新必须先确认（带新 offset 的 getUpdates）再处理，
// 否则处理途中重启会让 Telegram 把这批重发、写操作执行两次。
func TestTelegramAcksBatchBeforeHandling(t *testing.T) {
	fake := &fakeTelegramPoll{}
	old := telegramHTTP
	telegramHTTP = &http.Client{Transport: fake}
	t.Cleanup(func() { telegramHTTP = old })
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.purityKeys = ippurity.NewKeyStore(map[string]string{ippurity.KeyTelegram: "fixture-token"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartTelegramBot(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		joined := strings.Join(fake.calls, ",")
		fake.mu.Unlock()
		if strings.Contains(joined, "answerCallbackQuery") {
			ack := strings.Index(joined, "getUpdates:42:ack")
			handled := strings.Index(joined, "answerCallbackQuery")
			if ack < 0 || ack > handled {
				t.Fatalf("the batch must be confirmed (offset 42) before the callback is handled: %s", joined)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	t.Fatalf("callback was never handled: %v", fake.calls)
}
