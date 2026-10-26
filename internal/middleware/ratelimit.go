// Package middleware holds the HTTP middleware shared by the gateway: request
// logging/ids, admin authentication and the in-memory rate limiter.
package middleware

import (
	"sort"
	"sync"
	"time"
)

const (
	// rateWindow is the sliding window width (one minute, i.e. RPM).
	rateWindow = time.Minute
	// rateShardCount spreads keys across independent locks. A single global
	// mutex made every authenticated request contend on the same cache line.
	rateShardCount = 64
)

// rateShard is one independently locked slice of the limiter's key space.
type rateShard struct {
	mu          sync.Mutex
	buckets     map[string][]int64 // key -> event timestamps (epoch ms)
	lastCleanup int64
}

// RateLimiter is an in-memory sharded sliding-window limiter, single-process by
// design (port of middleware/rate-limit.ts).
type RateLimiter struct {
	shards [rateShardCount]rateShard
}

// NewRateLimiter returns an empty limiter.
func NewRateLimiter() *RateLimiter {
	limiter := &RateLimiter{}
	now := time.Now().UnixMilli()
	for index := range limiter.shards {
		limiter.shards[index].buckets = make(map[string][]int64)
		limiter.shards[index].lastCleanup = now
	}
	return limiter
}

// shardFor maps a key to a shard with FNV-1a. The key is short (an API-key id
// or a client IP), so hashing in place avoids an extra allocation.
func (l *RateLimiter) shardFor(key string) *rateShard {
	hash := uint32(2166136261)
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= 16777619
	}
	return &l.shards[hash%rateShardCount]
}

// Check records an attempt for key and reports whether it is within the limit.
// A non-positive limit means unlimited. When rejected, retryAfterSeconds is how
// long until a slot frees up.
func (l *RateLimiter) Check(key string, limitPerMinute int) (retryAfterSeconds int, allowed bool) {
	if limitPerMinute <= 0 {
		return 0, true
	}
	now := time.Now().UnixMilli()

	shard := l.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.buckets == nil {
		shard.buckets = make(map[string][]int64)
	}
	shard.maybeCleanupLocked(now)

	windowStart := now - rateWindow.Milliseconds()
	bucket := shard.buckets[key]
	// Timestamps are appended in order, so binary search finds the first one
	// still inside the window without scanning the whole bucket.
	if start := sort.Search(len(bucket), func(index int) bool {
		return bucket[index] > windowStart
	}); start > 0 {
		bucket = bucket[start:]
	}

	if len(bucket) >= limitPerMinute {
		shard.buckets[key] = bucket
		retry := 60
		if len(bucket) > 0 {
			retry = int((rateWindow.Milliseconds() - (now - bucket[0]) + 999) / 1000)
			if retry < 1 {
				retry = 1
			}
		}
		return retry, false
	}

	shard.buckets[key] = append(bucket, now)
	return 0, true
}

// Reset drops a key's history. Used where a bucket tracks failures: a
// successful login clears the client's failure count.
func (l *RateLimiter) Reset(key string) {
	shard := l.shardFor(key)
	shard.mu.Lock()
	delete(shard.buckets, key)
	shard.mu.Unlock()
}

// maybeCleanupLocked drops idle buckets; caller holds the shard mutex.
func (s *rateShard) maybeCleanupLocked(now int64) {
	if now-s.lastCleanup < rateWindow.Milliseconds() {
		return
	}
	s.lastCleanup = now
	for key, bucket := range s.buckets {
		if len(bucket) == 0 || now-bucket[len(bucket)-1] > 2*rateWindow.Milliseconds() {
			delete(s.buckets, key)
		}
	}
}
