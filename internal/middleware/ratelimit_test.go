package middleware

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRateLimiterWindowAndReset(t *testing.T) {
	limiter := NewRateLimiter()

	if _, allowed := limiter.Check("key", 2); !allowed {
		t.Fatal("first request should be allowed")
	}
	if _, allowed := limiter.Check("key", 2); !allowed {
		t.Fatal("second request should be allowed")
	}
	if retry, allowed := limiter.Check("key", 2); allowed || retry <= 0 {
		t.Fatalf("third request: retry=%d allowed=%v", retry, allowed)
	}
	if _, allowed := limiter.Check("other", 2); !allowed {
		t.Fatal("a separate key must have its own bucket")
	}
	if _, allowed := limiter.Check("unlimited", 0); !allowed {
		t.Fatal("non-positive limit should be unlimited")
	}

	limiter.Reset("key")
	if _, allowed := limiter.Check("key", 2); !allowed {
		t.Fatal("reset should clear the bucket")
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	limiter := NewRateLimiter()
	const (
		limit   = 1000
		workers = 50
		per     = 100
	)
	var allowed atomic.Int64
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < per; attempt++ {
				if _, ok := limiter.Check("shared", limit); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wait.Wait()

	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed = %d, want %d", got, limit)
	}
}
