package httpapi

import (
	"os"
	"strings"
	"testing"
)

// 2026-11-06：商城下发的是「付款前算好的绝对到期时间」，控制台只能往后推。
// 原来直接赋值，运营手动延长过的天数会被冲回商城算出的值，永久订阅也会变成有限期。
func TestStoreRenewNeverShortens(t *testing.T) {
	raw, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	a := strings.Index(s, "const storeRenewQuery")
	if a < 0 {
		t.Fatal("storeRenewQuery not found")
	}
	b := strings.Index(s[a:], "WHERE id = $4::uuid")
	if b < 0 {
		t.Fatal("storeRenewQuery tail not found")
	}
	q := s[a : a+b]
	if strings.Contains(q, "THEN $1::timestamptz") {
		t.Fatal("absolute renewal still assigns $1 directly and can shorten the subscription")
	}
	if !strings.Contains(q, "GREATEST(expires_at, $1::timestamptz)") {
		t.Fatal("absolute renewal must keep the later of current and target")
	}
	if !strings.Contains(q, "WHEN $1::timestamptz IS NOT NULL AND expires_at IS NULL THEN NULL") {
		t.Fatal("perpetual subscription must stay perpetual when an absolute target arrives")
	}
	// 按天续费（$1 为空）的语义保持不变。
	if !strings.Contains(q, "GREATEST(expires_at, now()) + make_interval(days => $2::int)") {
		t.Fatal("relative renewal semantics changed")
	}
}

// 商城要能区分「续费成功但订阅是永久的」与「续费没生效」。
func TestStoreOrderResponseFlagsPerpetual(t *testing.T) {
	raw, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, "func (s *Server) writeStoreOrder(")
	if start < 0 {
		t.Fatal("writeStoreOrder not found")
	}
	end := strings.Index(s[start:], "\n}\n")
	if end < 0 {
		t.Fatal("writeStoreOrder end not found")
	}
	body := s[start : start+end]
	if !strings.Contains(body, `"perpetual":`) {
		t.Fatal("store order response must expose perpetual so the shop does not treat expires_at=0 as failure")
	}
}
