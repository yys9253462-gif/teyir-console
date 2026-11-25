package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestTraceClassification(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		cancel        bool
		level, marker string
		count         uint64
	}{
		{"cancel", fmt.Errorf("query: %w", context.Canceled), true, "INFO", "database request canceled", 1},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), false, "WARN", "database request deadline exceeded", 0},
		{"real_failure_with_canceled_context", errors.New("connection refused"), true, "ERROR", "database request failed", 0},
		{"joined_real_failure", errors.Join(context.Canceled, errors.New("connection refused")), true, "ERROR", "database request failed", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
			r := httptest.NewRequest("GET", "/api/sub/view/SECRET?token=QUERYSECRET", nil)
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			r = r.WithContext(ctx)
			r.Header.Set("X-Request-ID", "trace-test.1")
			w := httptest.NewRecorder()
			s.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.databaseError(w, r, tc.err) })).ServeHTTP(w, r)
			output := log.String()
			for _, want := range []string{tc.marker, `"level":"` + tc.level + `"`, `"request_id":"trace-test.1"`, `"elapsed_ms":`, `"context_state":`, `"database_canceled_total":`} {
				if !strings.Contains(output, want) {
					t.Fatalf("missing %s: %s", want, output)
				}
			}
			if strings.Contains(output, "SECRET") {
				t.Fatalf("sensitive path/query leaked: %s", output)
			}
			if w.Header().Get("X-Request-ID") != "trace-test.1" || (tc.name == "cancel") != (w.Code == 499) || (tc.name != "cancel" && w.Code != 500) {
				t.Fatalf("response changed: %v %d", w.Header(), w.Code)
			}
			if s.databaseCanceled.Load() != tc.count {
				t.Fatal("wrong cancel count")
			}
			if tc.name == "deadline" && s.databaseDeadline.Load() != 1 {
				t.Fatal("wrong deadline count")
			}
		})
	}
}
func TestRequestIDAndWriterPreserved(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 65), "bad id", "bad\nvalue", "合法"} {
		var log bytes.Buffer
		s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
		r := httptest.NewRequest("OPTIONS", "/api/health", nil)
		r.Header.Set("X-Request-ID", id)
		w := httptest.NewRecorder()
		s.requestLog(http.HandlerFunc(func(got http.ResponseWriter, r *http.Request) {
			// 2026-10-24 审计 P3-1 起中间件会包一层 statusWriter 以记录状态码，
			// 所以这里断言的不再是「同一个对象」，而是**包装后仍然具备
			// 所有可选接口能力** —— 那才是这条测试真正要守的东西：
			// 丢了 Flusher，SSE 推送会卡住；丢了 Hijacker，连接升级会失败。
			//
			// 这一改不是为了让测试变绿而放宽：原来那条 `got != w` 断言的
			// 是「实现细节」而不是「行为」，它阻止的正是加状态码记录的合理改动。
			if got.(http.Flusher) == nil {
				t.Fatal("Flusher lost after wrapping")
			}
			if uc, ok := got.(interface{ Unwrap() http.ResponseWriter }); !ok || uc.Unwrap() != w {
				t.Fatal("Unwrap must expose the original writer")
			}
			got.WriteHeader(http.StatusTeapot)
			got.(http.Flusher).Flush()
		})).ServeHTTP(w, r)
		actual := w.Header().Get("X-Request-ID")
		if !validRequestID(actual) || actual == id {
			t.Fatalf("invalid generated ID %q", actual)
		}
		// 包装不能改变响应：状态码要如实到得了客户端。
		if w.Code != http.StatusTeapot {
			t.Fatalf("status leaked away: %d", w.Code)
		}
		// 同时，访问日志里必须出现这个状态码（P3-1 的验收点）。
		if !strings.Contains(log.String(), `"status":418`) {
			t.Fatalf("access log missing status: %s", log.String())
		}
	}
}

// TestAccessLogRecordsStatus 是 P3-1 的直接验收点：
// 成功与失败必须能从访问日志一眼分开，且 5xx 要升到 ERROR 级。
func TestAccessLogRecordsStatus(t *testing.T) {
	cases := []struct {
		status int
		level  string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusNotFound, "INFO"},
		{http.StatusInternalServerError, "ERROR"},
		{http.StatusBadGateway, "ERROR"},
	}
	for _, tc := range cases {
		var log bytes.Buffer
		s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
		r := httptest.NewRequest("GET", "/api/health", nil)
		s.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		})).ServeHTTP(httptest.NewRecorder(), r)
		out := log.String()
		if !strings.Contains(out, fmt.Sprintf(`"status":%d`, tc.status)) {
			t.Fatalf("%d: missing status in %s", tc.status, out)
		}
		if !strings.Contains(out, `"level":"`+tc.level+`"`) {
			t.Fatalf("%d: expected level %s in %s", tc.status, tc.level, out)
		}
	}
}
func TestRequestIDDuplicateAndValidLimits(t *testing.T) {
	if !validRequestID(strings.Repeat("a", 64)) {
		t.Fatal("64 byte valid ID rejected")
	}
	var log bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.Header.Add("X-Request-ID", "first")
	r.Header.Add("X-Request-ID", "second")
	w := httptest.NewRecorder()
	s.requestLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)
	if id := w.Header().Get("X-Request-ID"); !validRequestID(id) || id == "first" || id == "second" {
		t.Fatalf("duplicate accepted: %q", id)
	}
}

func TestRequestContextCounters(t *testing.T) {
	var log bytes.Buffer
	s := &Server{logger: slog.New(slog.NewJSONHandler(&log, nil))}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := httptest.NewRequest("GET", "/api/health", nil).WithContext(ctx)
	s.requestLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), r)
	if s.requestDeadline.Load() != 1 || !strings.Contains(log.String(), `"request_deadline_total":1`) {
		t.Fatal(log.String())
	}
}
