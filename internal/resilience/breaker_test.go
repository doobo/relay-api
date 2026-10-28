package resilience

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	breaker := NewBreaker(2, time.Minute)
	now := time.Unix(1000, 0)
	breaker.now = func() time.Time { return now }

	if !breaker.Allow("provider") {
		t.Fatal("a closed breaker must allow requests")
	}
	breaker.Failure("provider")
	if !breaker.Allow("provider") {
		t.Fatal("a single failure below the threshold must still allow")
	}
	breaker.Failure("provider")
	if breaker.Allow("provider") {
		t.Fatal("the breaker must open once the threshold is reached")
	}

	now = now.Add(59 * time.Second)
	if breaker.Allow("provider") {
		t.Fatal("the breaker must stay open for the whole cooldown")
	}
}

func TestBreakerHalfOpenAdmitsOneProbe(t *testing.T) {
	breaker := NewBreaker(1, time.Minute)
	now := time.Unix(2000, 0)
	breaker.now = func() time.Time { return now }

	breaker.Failure("provider")
	if breaker.Allow("provider") {
		t.Fatal("breaker should be open")
	}

	now = now.Add(time.Minute)
	if !breaker.Allow("provider") {
		t.Fatal("an expired cooldown must admit a probe")
	}
	if breaker.Allow("provider") {
		t.Fatal("only one half-open probe may be in flight")
	}

	// A failed probe reopens the breaker immediately.
	breaker.Failure("provider")
	if breaker.Allow("provider") {
		t.Fatal("a failed probe must reopen the breaker")
	}

	now = now.Add(time.Minute)
	if !breaker.Allow("provider") {
		t.Fatal("the next cooldown must admit another probe")
	}
	breaker.Success("provider")
	if !breaker.Allow("provider") {
		t.Fatal("a successful probe must close the breaker")
	}
}

func TestBreakerIsolatesKeysAndCanBeDisabled(t *testing.T) {
	disabled := NewBreaker(0, time.Minute)
	for index := 0; index < 10; index++ {
		disabled.Failure("provider")
	}
	if !disabled.Allow("provider") {
		t.Fatal("threshold 0 must disable the breaker")
	}

	breaker := NewBreaker(1, time.Minute)
	breaker.Failure("a")
	if breaker.Allow("a") {
		t.Fatal("key a should be open")
	}
	if !breaker.Allow("b") {
		t.Fatal("a failure on one key must not open another")
	}
}
