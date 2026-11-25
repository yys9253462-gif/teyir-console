package collector

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCollectorCycleBudgetAndRecovery(t *testing.T) {
	for _, interval := range []time.Duration{30 * time.Second, 2 * time.Minute} {
		c := &Collector{interval: interval, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := c.cycleTimeout(); got < 60*time.Second || got < interval {
			t.Fatalf("insufficient cycle budget: %v", got)
		}
		// nil db forces a panic before any network access. Both rounds must recover,
		// demonstrating that a failed cycle cannot terminate the collector loop.
		c.collectOnce(context.Background())
		c.collectOnce(context.Background())
	}
}
