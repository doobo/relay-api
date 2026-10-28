package routes

import (
	"context"
	"testing"
	"time"

	"relay-api/internal/util"
)

func noDelay(int) time.Duration { return 0 }

func TestCallWithRetrySucceedsAfterRetryableFailures(t *testing.T) {
	attempts := 0
	result, err := callWithRetry(context.Background(), 2, noDelay, func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", &util.UpstreamStatusError{Status: 500, Body: "boom"}
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q", result)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestCallWithRetryStopsOnNonRetryableStatus(t *testing.T) {
	attempts := 0
	_, err := callWithRetry(context.Background(), 5, noDelay, func() (string, error) {
		attempts++
		return "", &util.UpstreamStatusError{Status: 400, Body: "bad request"}
	})
	if err == nil {
		t.Fatal("expected the non-retryable error to be returned")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (400 must not be retried)", attempts)
	}
}

func TestCallWithRetryExhaustsAttempts(t *testing.T) {
	attempts := 0
	_, err := callWithRetry(context.Background(), 2, noDelay, func() (string, error) {
		attempts++
		return "", &util.UpstreamStatusError{Status: 503, Body: "unavailable"}
	})
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestCallWithRetryStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := callWithRetry(ctx, 3, func(int) time.Duration { return time.Hour }, func() (string, error) {
		attempts++
		cancel()
		return "", &util.UpstreamStatusError{Status: 503, Body: "unavailable"}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after cancellation", attempts)
	}
}

func TestRetryDelayGrowsAndIsCapped(t *testing.T) {
	gateway := &Gateway{}
	gateway.cfg.UpstreamRetryBaseMs = 100
	gateway.cfg.UpstreamRetryMaxMs = 400

	// The jittered delay stays within [half, full] of the exponential step.
	for attempt, want := range map[int]time.Duration{
		1: 100 * time.Millisecond,
		2: 200 * time.Millisecond,
		3: 400 * time.Millisecond,
		4: 400 * time.Millisecond,
	} {
		delay := gateway.retryDelay(attempt)
		if delay < want/2 || delay > want {
			t.Fatalf("attempt %d delay = %v, want within [%v, %v]", attempt, delay, want/2, want)
		}
	}
}
