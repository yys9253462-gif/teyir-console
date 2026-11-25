package httpapi

import "testing"

func TestSubscriptionRenewURL(t *testing.T) {
	const shop = "https://shop.example.com/"
	cases := []struct {
		order, token, want string
	}{
		{"OPS-prdord_fixture00000000000001", "fixture_subscription_token_0000000000000001",
			shop + "?renew=prdord_fixture00000000000001&sub=fixture_subscription_token_0000000000000001"},
		{"", "tok", shop},                        // 手工开的订阅：没有商城订单
		{"ORDER-123", "tok", shop},               // 其它来源的订单号
		{"OPS-prdord_x\" onclick=", "tok", shop}, // 不合规的订单号不拼进链接
	}
	for _, c := range cases {
		got := subscriptionRenewURL(&subscriptionRecord{OrderID: c.order, Token: c.token}, shop)
		if got != c.want {
			t.Errorf("order=%q: got %q want %q", c.order, got, c.want)
		}
	}
	if subscriptionRenewURL(nil, shop) != shop {
		t.Error("nil record must fall back to the shop")
	}
}
