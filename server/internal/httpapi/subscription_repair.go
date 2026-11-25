package httpapi

import (
	"context"
	"time"
)

const subscriptionCompensationTimeout = 5 * time.Second

func subscriptionCompensationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), subscriptionCompensationTimeout)
}

func (s *Server) queueSubscriptionSyncBounded(ctx context.Context, id string) error {
	ctx, cancel := subscriptionCompensationContext(ctx)
	defer cancel()
	return s.queueSubscriptionSync(ctx, id)
}

type subscriptionFlightKey struct {
	key string
	gen uint64
}

type subscriptionFlight struct {
	done  chan struct{}
	build *subscriptionBuild
}

// load coalesces cold and background builds in the same generation. Each
// waiter owns only its wait; the shared builder has a separate bounded lifetime.
func (c *subscriptionCache) load(ctx context.Context, key string, resolve func(context.Context) *subscriptionBuild) (*subscriptionBuild, error) {
	c.mu.Lock()
	var stale *subscriptionBuild
	if c.build != nil && c.key == key {
		age := time.Since(c.storedAt)
		if age < subscriptionCacheTTL {
			build := c.build
			c.mu.Unlock()
			return build, nil
		}
		if age < subscriptionStaleMax {
			stale = c.build
		}
	}
	fk := subscriptionFlightKey{key: key, gen: c.gen}
	if c.inflight == nil {
		c.inflight = make(map[subscriptionFlightKey]*subscriptionFlight)
	}
	flight := c.inflight[fk]
	if flight == nil {
		flight = &subscriptionFlight{done: make(chan struct{})}
		c.inflight[fk] = flight
		go func() {
			bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subscriptionRebuildTimeout)
			defer cancel()
			build := resolve(bctx)
			c.mu.Lock()
			flight.build = build
			if c.gen == fk.gen {
				c.key, c.build, c.storedAt = key, build, time.Now()
			}
			delete(c.inflight, fk)
			close(flight.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	if stale != nil {
		return stale, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
		return flight.build, nil
	}
}
