package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type requestTraceKey struct{}
type requestTrace struct {
	id      string
	started time.Time
}

// statusWriter 记录响应状态码，供访问日志使用（2026-10-24 审计 P3-1）。
//
// 为什么必须包一层：原来的 "http request" 日志**只有方法、路径、耗时**，
// 没有状态码。这导致排查时无法回答最基本的问题 —— 这条 500ms 的请求
// 到底是成功还是 5xx？只能靠时间戳去别的日志里对，而错误日志不覆盖
// 所有失败路径（比如 401、404、429 都是静默返回的）。
//
// 接口保留：net/http 的 ResponseWriter 有三个可选接口
// （Flusher / Hijacker / Pusher），面板里的 SSE 推送与连接升级依赖它们。
// 只实现 WriteHeader 而不转发这些接口，会让 flush 失效、流式响应卡住 ——
// 这是包装 ResponseWriter 最经典的坑，所以三个都显式转发。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	// 未显式 WriteHeader 就 Write，按 net/http 语义等价于 200。
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush 转发给底层（SSE 必需）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 转发给底层（连接升级必需）。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap 让 http.ResponseController 能找到底层 writer（Go 1.20+ 推荐做法）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

var fallbackRequestSequence atomic.Uint64

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("fallback-%x-%x", time.Now().UnixNano(), fallbackRequestSequence.Add(1))
}

// statusClientClosedRequest 是 nginx 的 499「client closed request」。
const statusClientClosedRequest = 499

// isClientCanceled 只在请求 context 被客户端取消（不是 deadline）且
// 数据库错误本身也只是取消时成立；独立的数据库故障不能被掩盖。
func isClientCanceled(r *http.Request, err error) bool {
	ctxErr := r.Context().Err()
	return errors.Is(ctxErr, context.Canceled) && !errors.Is(ctxErr, context.DeadlineExceeded) && databaseErrorClass(err) == "canceled"
}

func contextState(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if err != nil {
		return "error"
	}
	return "active"
}

// Classify only pure cancellation chains. Joined real failures retain ERROR.
func databaseErrorClass(err error) string {
	if err == context.Canceled {
		return "canceled"
	}
	if err == context.DeadlineExceeded {
		return "deadline_exceeded"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		kind := ""
		for _, child := range joined.Unwrap() {
			childKind := databaseErrorClass(child)
			if childKind == "error" || kind != "" && childKind != kind {
				return "error"
			}
			kind = childKind
		}
		if kind != "" {
			return kind
		}
		return "error"
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return databaseErrorClass(wrapped.Unwrap())
	}
	return "error"
}

func traceFields(r *http.Request) []any {
	trace, _ := r.Context().Value(requestTraceKey{}).(requestTrace)
	elapsed := int64(0)
	if !trace.started.IsZero() {
		elapsed = time.Since(trace.started).Milliseconds()
	}
	return []any{"request_id", trace.id, "method", r.Method, "path", redactRequestPath(r.URL.Path), "elapsed_ms", elapsed, "duration_ms", elapsed, "context_state", contextState(r.Context().Err())}
}
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) || len(r.Header.Values("X-Request-ID")) != 1 {
			id = newRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, requestTrace{id, time.Now()}))
		w.Header().Set("X-Request-ID", id)
		// 包一层记录状态码（2026-10-24 审计 P3-1）：访问日志没有状态码的话，
		// 排查时无法区分「慢但成功」与「慢且失败」，而后者才是要修的东西。
		rec := &statusWriter{ResponseWriter: w}
		w = rec
		// Keep the original ResponseWriter: optional interfaces retain exact semantics.
		defer func() {
			state := contextState(r.Context().Err())
			if state == "canceled" {
				s.requestCanceled.Add(1)
			}
			if state == "deadline_exceeded" {
				s.requestDeadline.Add(1)
			}
			fields := traceFields(r)
			// 状态码取 0 说明 handler 什么都没写（连接在写之前就断了，
			// 或 handler 里发生了 panic 并被上层恢复）。记 0 而不是猜测成 200 ——
			// 一个「没有响应」的请求被记成 200 比缺字段更误导。
			fields = append(fields, "status", rec.status, "bytes", rec.bytes)
			fields = append(fields, "request_canceled_total", s.requestCanceled.Load(), "request_deadline_total", s.requestDeadline.Load(), "database_canceled_total", s.databaseCanceled.Load(), "database_deadline_total", s.databaseDeadline.Load())
			// 5xx 提到 Error 级：这些是真正的服务端故障，混在几百条 Info 里
			// 只能靠人肉 grep 才发现。
			if rec.status == statusClientClosedRequest {
				s.logger.Info("client canceled request", fields...)
				return
			}
			if rec.status >= 500 {
				s.logger.Error("http request failed", fields...)
				return
			}
			s.logger.Info("http request", fields...)
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) logDatabaseError(r *http.Request, err error) {
	fields := traceFields(r)
	kind := databaseErrorClass(err)
	fields = append(fields, "error_class", kind)
	switch kind {
	case "canceled":
		fields = append(fields, "database_canceled_total", s.databaseCanceled.Add(1))
		s.logger.Info("database request canceled", fields...)
	case "deadline_exceeded":
		fields = append(fields, "database_deadline_total", s.databaseDeadline.Add(1))
		s.logger.Warn("database request deadline exceeded", fields...)
	default:
		// A canceled HTTP context must never conceal an independent DB failure.
		fields = append(fields, "error", err)
		s.logger.Error("database request failed", fields...)
	}
}
