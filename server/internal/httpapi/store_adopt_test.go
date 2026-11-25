package httpapi

import (
	"strings"
	"testing"
)

// 接入只能挂到空订单号上（或同一订单号的重试），绝不改写已有关联；永久订阅拒绝。
func TestStoreAdoptGuards(t *testing.T) {
	body := funcBlock(t, "store_adopt.go", "func (s *Server) storeAdopt(")
	for _, want := range []string{
		"COALESCE(order_id,'')='' OR order_id=$2",
		"rec.ExpiresAt == nil",
		"shopOrderIDPattern.MatchString(orderID)",
		"s.lockSubscription(",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("storeAdopt 缺少保护：%s", want)
		}
	}
	routes := funcBlock(t, "server.go", "func New(")
	for _, want := range []string{`"GET /api/store/adopt-info", s.requireStoreKey(`, `"POST /api/store/adopt", s.requireStoreKey(`} {
		if !strings.Contains(routes, want) {
			t.Fatalf("接入接口必须走商城密钥：%s", want)
		}
	}
}
