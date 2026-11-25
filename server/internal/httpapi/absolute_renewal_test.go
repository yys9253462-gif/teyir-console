package httpapi

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionAbsoluteRenewDeadline(t *testing.T) {
	now := time.Unix(1800000000, 0)
	expiry := now.Add(30*24*time.Hour + 17*time.Second)
	rec := &subscriptionRecord{ExpiresAt: &expiry}
	for _, current := range []int64{now.Unix() - 1, expiry.Unix(), expiry.Unix() + 86400} {
		if got := subscriptionNodeRenewDeadline(rec, current, now); got != expiry.Unix() {
			t.Fatalf("current=%d got=%d want=%d", current, got, expiry.Unix())
		}
		if got := subscriptionNodeRenewDeadline(rec, current, now.Add(time.Minute)); got != expiry.Unix() {
			t.Fatal("retry moved absolute target")
		}
	}
	for _, n := range []time.Time{expiry, expiry.Add(time.Second)} {
		if got := subscriptionNodeRenewDeadline(rec, now.Unix(), n); got != 0 {
			t.Fatal("expired subscription would reactivate")
		}
	}
	rec.ExpiresAt = nil
	current := now.AddDate(20, 0, 0).Unix()
	if got := subscriptionNodeRenewDeadline(rec, current, now); got != current {
		t.Fatal("perpetual subscription shortened")
	}
	if got := subscriptionNodeRenewDeadline(rec, 0, now); got != now.AddDate(0, 0, 3650).Unix() {
		t.Fatal("perpetual default incorrect")
	}
}

func TestRenewUsesAbsoluteNodeUpdateAndReadback(t *testing.T) {
	raw, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	a := strings.Index(s, "func (s *Server) renewSubscriptionAccounts(")
	b := strings.Index(s[a:], "// subscriptionNodeRenewDeadline")
	if a < 0 || b < 0 {
		t.Fatal("renew function not found")
	}
	body := s[a : a+b]
	if !strings.Contains(body, "s.synchronizeSubscription(") {
		t.Fatal("renewal must use locked durable synchronization")
	}
	attrs, err := os.ReadFile("subscription_attributes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(attrs), "verifyAccountExpiry(") || !strings.Contains(string(attrs), `"/users/update"`) {
		t.Fatal("absolute update/readback missing")
	}

	if strings.Contains(body, `"extend_days"`) {
		t.Fatal("relative renewal reintroduced")
	}
}
