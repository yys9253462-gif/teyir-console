package collector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"nodeops/server/internal/secrets"
)

// TestPrefetchNodeUsersRunsConcurrently 钉住 2026-11-06 的修复：一轮用量采集里各节点的 users/list
// 并发请求。原来串行 —— 每个慢 / 不可达节点吃满超时，几台就把整轮预算耗光，后面的健康节点被标成陈旧。
func TestPrefetchNodeUsersRunsConcurrently(t *testing.T) {
	orig := usageClient.Transport
	usageClient.Transport = &http.Transport{DialContext: testDial(), DisableKeepAlives: true}
	defer func() { usageClient.Transport = orig }()

	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	key, _ := box.Seal([]byte("k"))

	const delay = 1500 * time.Millisecond
	var targets []usageNodeTarget
	for i := 0; i < 6; i++ {
		uid := "sub_" + strconv.Itoa(i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			_, _ = w.Write([]byte(`{"ok":true,"users":[{"user_id":"` + uid + `","traffic_used_bytes":1}]}`))
		}))
		defer srv.Close()
		targets = append(targets, usageNodeTarget{nodeID: "n" + strconv.Itoa(i), apiURL: srv.URL, apiKey: key})
	}

	c := &Collector{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), box: box}
	perNode := map[string]map[string]portalUserUsage{}
	failures := map[string]string{}
	start := time.Now()
	c.prefetchNodeUsers(context.Background(), targets, perNode, failures)
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Fatalf("6 个节点各慢 %v，预取耗时 %v —— 仍是串行", delay, elapsed)
	}
	if len(failures) != 0 {
		t.Fatalf("不应失败：%v", failures)
	}
	for i := 0; i < 6; i++ {
		users := perNode["n"+strconv.Itoa(i)]
		if _, ok := users["sub_"+strconv.Itoa(i)]; !ok {
			t.Fatalf("节点 n%d 的用户没取回来：%v", i, users)
		}
	}
}
