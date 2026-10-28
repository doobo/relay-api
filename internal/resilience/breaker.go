// Package resilience holds the failure-handling primitives used on the relay
// path: the per-provider circuit breaker that keeps a failing upstream from
// absorbing every request, and the backoff policy applied on top of retries.
package resilience

import (
	"sync"
	"time"
)

// Breaker is a per-key circuit breaker with a single half-open probe.
//
// It is deliberately small: the relay only needs to stop hammering an upstream
// whose database or network is failing, not to model every failure mode. A key
// is expected to derive from the upstream's identity (the provider row), so one
// broken provider does not open the breaker for the others.
//
// State machine:
//
//	closed    -> every request is allowed; consecutive failures are counted.
//	open      -> requests are rejected until the cooldown expires.
//	half-open -> exactly one probe is allowed; its result closes or reopens.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]*breakerEntry
}

// breakerEntry is the per-key state. A zero openUntil means the breaker is
// closed. probing is set while a half-open probe is in flight.
type breakerEntry struct {
	failures  int
	openUntil time.Time
	probing   bool
}

// NewBreaker builds a breaker that opens after threshold consecutive failures
// and stays open for cooldown. threshold <= 0 disables the breaker; cooldown
// <= 0 falls back to one second so a re-opened breaker cannot spin.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	if cooldown <= 0 {
		cooldown = time.Second
	}
	return &Breaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		entries:   make(map[string]*breakerEntry),
	}
}

// Enabled reports whether the breaker performs any gating.
func (b *Breaker) Enabled() bool {
	return b != nil && b.threshold > 0
}

// Allow reports whether one request for key may be sent upstream. When it
// returns true for a half-open key, exactly one probe is admitted; concurrent
// callers keep being rejected until Success or Failure resolves it.
func (b *Breaker) Allow(key string) bool {
	if !b.Enabled() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	entry := b.entries[key]
	if entry == nil || entry.openUntil.IsZero() {
		return true
	}
	if b.now().Before(entry.openUntil) {
		return false
	}
	if entry.probing {
		return false
	}
	entry.probing = true
	return true
}

// Success records a healthy response and closes the key's breaker.
func (b *Breaker) Success(key string) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	delete(b.entries, key)
	b.mu.Unlock()
}

// Failure records one failed attempt. The breaker opens once the failure
// threshold is reached, and a failed half-open probe reopens it immediately.
func (b *Breaker) Failure(key string) {
	if !b.Enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	entry := b.entries[key]
	if entry == nil {
		entry = &breakerEntry{}
		b.entries[key] = entry
	}
	// A half-open probe that failed has a non-zero (expired) openUntil and must
	// reopen regardless of the failure count.
	reopen := !entry.openUntil.IsZero()
	entry.probing = false
	entry.failures++
	if reopen || entry.failures >= b.threshold {
		entry.openUntil = b.now().Add(b.cooldown)
	}
}
