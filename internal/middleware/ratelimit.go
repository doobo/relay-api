// Package middleware holds the HTTP middleware shared by the gateway: request
// logging/ids, admin authentication and the in-memory rate limiter.
package middleware

import (
	"sync"
	"time"
)

// rateWindow is the sliding window width (one minute, i.e. RPM).
const rateWindow = time.Minute

// RateLimiter is an in-memory sliding-window limiter, single-process by design
// (port of middleware/rate-limit.ts).
type RateLimiter struct {
	mu          sync.Mutex
	buckets     map[string][]int64 // key -> event timestamps (epoch ms)
	lastCleanup int64
}

// NewRateLimiter returns an empty limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets:     make(map[string][]int64),
		lastCleanup: time.Now().UnixMilli(),
	}
}

// Check records an attempt for key and reports whether it is within the limit.
// A non-positive limit means unlimited. When rejected, retryAfterSeconds is how
// long until a slot frees up.
func (l *RateLimiter) Check(key string, limitPerMinute int) (retryAfterSeconds int, allowed bool) {
	if limitPerMinute <= 0 {
		return 0, true
	}
	now := time.Now().UnixMilli()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeCleanupLocked(now)

	windowStart := now - rateWindow.Milliseconds()
	bucket := l.buckets[key]
	kept := bucket[:0]
	for _, ts := range bucket {
		if ts > windowStart {
			kept = append(kept, ts)
		}
	}

	if len(kept) >= limitPerMinute {
		l.buckets[key] = kept
		retry := 60
		if len(kept) > 0 {
			retry = int((rateWindow.Milliseconds() - (now - kept[0]) + 999) / 1000)
			if retry < 1 {
				retry = 1
			}
		}
		return retry, false
	}

	l.buckets[key] = append(kept, now)
	return 0, true
}

// Reset drops a key's history. Used where a bucket tracks failures: a
// successful login clears the client's failure count.
func (l *RateLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// maybeCleanupLocked drops idle buckets; caller holds the mutex.
func (l *RateLimiter) maybeCleanupLocked(now int64) {
	if now-l.lastCleanup < rateWindow.Milliseconds() {
		return
	}
	l.lastCleanup = now
	for key, bucket := range l.buckets {
		if len(bucket) == 0 || now-bucket[len(bucket)-1] > 2*rateWindow.Milliseconds() {
			delete(l.buckets, key)
		}
	}
}
