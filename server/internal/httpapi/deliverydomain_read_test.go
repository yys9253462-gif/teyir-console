package httpapi

import (
	"strings"
	"testing"
)

func TestDeliveryDomainBootLoadDoesNotOverwriteCacheOnDatabaseFailure(t *testing.T) {
	body := funcBlock(t, "deliverydomain.go", "func (s *Server) loadDeliveryBase(")
	for _, want := range []string{
		"errors.Is(err, pgx.ErrNoRows)",
		"retaining existing cache",
		"deliveryBaseCache.Store(strings.TrimSpace(v))",
		"subscriptionNameCache.Store(strings.TrimSpace(name))",
		"if err := s.db.QueryRow(ctx, `SELECT value FROM system_settings WHERE key=$1`, deliveryBaseKey).Scan(&v); err != nil && !errors.Is(err, pgx.ErrNoRows)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("delivery config load must preserve cache on read failure; missing %q", want)
		}
	}
	if strings.Index(body, "retaining existing cache") > strings.Index(body, "deliveryBaseCache.Store") {
		t.Fatal("cache must be updated only after both setting reads succeed")
	}
}
