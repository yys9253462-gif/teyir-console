package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 客户端取消：499 + INFO「client canceled request」，不出现 ERROR。
func TestClientCanceledRequestIs499Info(t *testing.T) {
	var log bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
	r := httptest.NewRequest("GET", "/api/monitoring/nodes", nil)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.databaseError(w, r, fmt.Errorf("query: %w", context.Canceled))
	})).ServeHTTP(w, r)
	out := log.String()
	if w.Code != 499 {
		t.Fatalf("status = %d, want 499", w.Code)
	}
	if strings.Contains(out, `"level":"ERROR"`) || strings.Contains(out, "http request failed") {
		t.Fatalf("client cancel logged as error: %s", out)
	}
	if !strings.Contains(out, `"msg":"client canceled request"`) || !strings.Contains(out, `"status":499`) {
		t.Fatalf("missing client cancel log: %s", out)
	}
}

// 服务端超时仍是 500 + ERROR。
func TestServerDeadlineStill500Error(t *testing.T) {
	var log bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
	r := httptest.NewRequest("GET", "/api/monitoring/nodes", nil)
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(-time.Second))
	defer cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.databaseError(w, r, fmt.Errorf("query: %w", context.DeadlineExceeded))
	})).ServeHTTP(w, r)
	if w.Code != 500 || !strings.Contains(log.String(), "http request failed") {
		t.Fatalf("deadline behaviour changed: %d %s", w.Code, log.String())
	}
}

// VACUUM 有独立超时：卡住时按超时返回，记 Warn，不挂住调用方。
func TestVacuumTableHasOwnTimeout(t *testing.T) {
	oldTimeout, oldExec := retentionVacuumTimeout, vacuumExec
	defer func() { retentionVacuumTimeout, vacuumExec = oldTimeout, oldExec }()
	retentionVacuumTimeout = 20 * time.Millisecond
	vacuumExec = func(_ *Server, ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	var log bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
	done := make(chan struct{})
	go func() { s.vacuumTable(context.Background(), "node_samples"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("vacuum hung without its own timeout")
	}
	out := log.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, "retention: vacuum timed out") {
		t.Fatalf("missing warn: %s", out)
	}
}
