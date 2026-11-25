package httpapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests mutate the package cache/query hook and must not run in parallel.
func isolateAutoHideCache(t *testing.T) {
	t.Helper()
	oldQuery := autoHideQuery
	autoHideCache.Lock()
	at, enabled, valid, generation := autoHideCache.at, autoHideCache.enabled, autoHideCache.valid, autoHideCache.generation
	autoHideCache.at, autoHideCache.enabled, autoHideCache.valid, autoHideCache.generation = time.Time{}, false, false, 0
	autoHideCache.Unlock()
	t.Cleanup(func() {
		autoHideQuery = oldQuery
		autoHideCache.Lock()
		autoHideCache.at, autoHideCache.enabled, autoHideCache.valid, autoHideCache.generation = at, enabled, valid, generation
		autoHideCache.Unlock()
	})
}

func expireAutoHideCache() {
	autoHideCache.Lock()
	autoHideCache.at = time.Now().Add(-2 * autoHideCacheTTL)
	autoHideCache.Unlock()
}

func TestAutoHideCacheInvalidationBlocksOldRefresh(t *testing.T) {
	for _, newReadFirst := range []bool{false, true} {
		name := "old_returns_before_new_read"
		if newReadFirst {
			name = "old_returns_after_new_read"
		}
		t.Run(name, func(t *testing.T) {
			isolateAutoHideCache(t)
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var wg sync.WaitGroup
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); wg.Wait() })
			var calls atomic.Int32
			autoHideQuery = func(_ *Server, _ context.Context) (bool, error) {
				if calls.Add(1) == 1 {
					close(started) // The old query has read enabled=true, but has not returned.
					<-release
					return true, nil
				}
				return false, nil // Administrator has now saved enabled=false.
			}
			s := &Server{}
			oldResult := make(chan bool, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				oldResult <- s.autoHideEnabled(context.Background())
			}()
			<-started
			invalidateAutoHideCache()
			if newReadFirst && s.autoHideEnabled(context.Background()) {
				t.Error("new generation did not read the administrator's disabled setting")
			}
			releaseOnce.Do(func() { close(release) })
			<-oldResult // The already-running caller may see its old snapshot.
			autoHideCache.Lock()
			at, enabled := autoHideCache.at, autoHideCache.enabled
			autoHideCache.Unlock()
			if !newReadFirst && !at.IsZero() {
				t.Error("old generation made the invalidated cache fresh")
			}
			if newReadFirst && enabled {
				t.Error("old generation overwrote the newly cached disabled setting")
			}
			if s.autoHideEnabled(context.Background()) || calls.Load() != 2 {
				t.Fatalf("next caller must see disabled with exactly two reads; calls=%d", calls.Load())
			}
		})
	}
}

func TestAutoHideCacheFailurePreservesKnownValue(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		name := "ttl_expired"
		if invalidate {
			name = "administrator_invalidated"
		}
		t.Run(name, func(t *testing.T) {
			isolateAutoHideCache(t)
			calls := 0
			autoHideQuery = func(_ *Server, _ context.Context) (bool, error) {
				calls++
				if calls == 1 {
					return false, nil
				}
				return true, errors.New("database unavailable")
			}
			s := &Server{}
			if s.autoHideEnabled(context.Background()) {
				t.Fatal("initial disabled setting was not read")
			}
			if invalidate {
				invalidateAutoHideCache()
			} else {
				expireAutoHideCache()
			}
			if s.autoHideEnabled(context.Background()) {
				t.Fatal("query failure replaced the known disabled setting with default true")
			}
			if s.autoHideEnabled(context.Background()) || calls != 2 {
				t.Fatalf("failure must retain disabled and throttle retries; calls=%d", calls)
			}
		})
	}
}

func TestAutoHideCacheColdFailureAndRecovery(t *testing.T) {
	isolateAutoHideCache(t)
	calls := 0
	autoHideQuery = func(_ *Server, _ context.Context) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("database unavailable")
		}
		return false, nil
	}
	s := &Server{}
	if !s.autoHideEnabled(context.Background()) || !s.autoHideEnabled(context.Background()) || calls != 1 {
		t.Fatal("cold failure must use default true and throttle retries")
	}
	expireAutoHideCache()
	if s.autoHideEnabled(context.Background()) || calls != 2 {
		t.Fatal("successful refresh did not replace the fallback after expiry")
	}
}

func TestAutoHideCacheFailedRefreshUsesLatestKnownValue(t *testing.T) {
	isolateAutoHideCache(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var wg sync.WaitGroup
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); wg.Wait() })
	var calls atomic.Int32
	autoHideQuery = func(_ *Server, _ context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return true, errors.New("older query failed")
		}
		return false, nil
	}
	s := &Server{}
	result := make(chan bool, 1)
	wg.Add(1)
	go func() { defer wg.Done(); result <- s.autoHideEnabled(context.Background()) }()
	<-started
	if s.autoHideEnabled(context.Background()) {
		t.Error("concurrent successful refresh did not cache disabled")
	}
	releaseOnce.Do(func() { close(release) })
	if <-result || s.autoHideEnabled(context.Background()) || calls.Load() != 2 {
		t.Fatal("late failure overwrote the concurrent success with an older fallback")
	}
}

func TestAutoHideCacheRefreshIgnoresRequestCancellation(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "cancelled_request"
		if expired {
			name = "expired_request_deadline"
		}
		t.Run(name, func(t *testing.T) {
			isolateAutoHideCache(t)
			type contextKey struct{}
			parent := context.WithValue(context.Background(), contextKey{}, "request value")
			var ctx context.Context
			var cancel context.CancelFunc
			if expired {
				ctx, cancel = context.WithDeadline(parent, time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(parent)
				cancel()
			}
			defer cancel()
			calls := 0
			autoHideQuery = func(_ *Server, qctx context.Context) (bool, error) {
				calls++
				if err := qctx.Err(); err != nil {
					t.Errorf("request cancellation leaked into shared refresh: %v", err)
					return true, err
				}
				deadline, ok := qctx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > healthGateQueryTimeout {
					t.Errorf("refresh must have an independent bounded deadline: %v, %v", deadline, ok)
				}
				if qctx.Value(contextKey{}) != "request value" {
					t.Error("request context values were lost")
				}
				if !autoHideCache.TryLock() {
					t.Error("query holds cache mutex")
				} else {
					autoHideCache.Unlock()
				}
				return false, nil
			}
			s := &Server{}
			if s.autoHideEnabled(ctx) || s.autoHideEnabled(context.Background()) || calls != 1 {
				t.Fatal("cancelled caller poisoned the shared cache or bypassed caching")
			}
		})
	}
}

func TestAutoHideCacheQueryTimeoutRetainsDisabled(t *testing.T) {
	isolateAutoHideCache(t)
	autoHideCache.Lock()
	autoHideCache.valid, autoHideCache.enabled = true, false
	autoHideCache.Unlock()
	autoHideQuery = func(_ *Server, ctx context.Context) (bool, error) {
		<-ctx.Done() // Real refresh timeout, no sleep or fabricated timeout error.
		return true, ctx.Err()
	}
	if (&Server{}).autoHideEnabled(context.Background()) {
		t.Fatal("refresh timeout replaced disabled with default true")
	}
}
