package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 2026-11-04：入站 ID 只能是正整数。「..」之类会原样拼进旧版 3X-UI 的 /inbounds/{id}/… 路径。
// 校验必须在读取面板凭据之前就拒绝（这里 Server 没有数据库：走到 loadXUITarget 就会 panic）。
func TestXUIInboundIDMustBeNumeric(t *testing.T) {
	if !xuiInboundIDPattern.MatchString("12") || xuiInboundIDPattern.MatchString("..") || xuiInboundIDPattern.MatchString("0") || xuiInboundIDPattern.MatchString("1.2") {
		t.Fatal("inbound id pattern must accept only positive integers")
	}
	s := &Server{writeLimiter: newSlidingLimiter(time.Minute, 100)}
	req := httptest.NewRequest(http.MethodDelete, "/api/xui/panels/p/inbounds/x/clients/c", nil)
	req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
	req.SetPathValue("inboundID", "..")
	req.SetPathValue("clientId", "c")
	req.Header.Set("X-Confirm-Dangerous", "yes")
	rec := httptest.NewRecorder()
	s.xuiDeleteClient(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a non-numeric inbound id, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/xui/panels/p/inbounds/x/clients/e/reset", nil)
	req.SetPathValue("id", "00000000-0000-0000-0000-000000000001")
	req.SetPathValue("inboundID", "..")
	req.SetPathValue("email", "a@b.c")
	rec = httptest.NewRecorder()
	s.xuiResetClientTraffic(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a non-numeric inbound id, got %d", rec.Code)
	}
}
