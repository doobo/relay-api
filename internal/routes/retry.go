package routes

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"relay-api/internal/router"
)

// errCircuitOpen marks a route skipped because its provider's circuit breaker
// is open. When every route is skipped the caller reports 503 rather than 502.
var errCircuitOpen = errors.New("upstream provider circuit open")

// callWithRetry runs fn up to 1+maxRetries times, sleeping with exponentially
// increasing, jittered backoff between attempts. Only failover-eligible errors
// (network/timeout/408/429/5xx) are retried; a non-retryable status or a
// canceled request stops the loop immediately.
//
// The request body is rebuilt by fn on every attempt, so retrying a streaming
// or non-streaming call is safe: a streaming attempt only fails before the
// first byte reaches the client.
func callWithRetry[T any](
	ctx context.Context,
	maxRetries int,
	delay func(attempt int) time.Duration,
	fn func() (T, error),
) (T, error) {
	var zero T
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			wait := delay(attempt)
			if !sleepContext(ctx, wait) {
				return zero, lastErr
			}
			slog.Debug("retrying upstream",
				"attempt", attempt,
				"max_retries", maxRetries,
				"delay_ms", wait.Milliseconds(),
				"error", lastErr,
			)
		}
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !shouldFailover(err) || ctx.Err() != nil {
			return zero, err
		}
	}
	return zero, lastErr
}

// retryDelay returns the jittered backoff before the given attempt (1-based).
// Full jitter over [delay/2, delay] spreads a concurrent retry storm instead of
// releasing every request at the same instant.
func (g *Gateway) retryDelay(attempt int) time.Duration {
	base := time.Duration(g.cfg.UpstreamRetryBaseMs) * time.Millisecond
	maxDelay := time.Duration(g.cfg.UpstreamRetryMaxMs) * time.Millisecond
	if base <= 0 || maxDelay <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for step := 1; step < attempt && delay < maxDelay; step++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

// sleepContext waits for delay unless the request context is canceled first.
func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// breakerKey identifies the upstream a provider row points at. The provider id
// separates providers that share a base URL but are configured independently.
func breakerKey(route router.Route) string {
	if route.Provider.ID > 0 {
		return strconv.FormatInt(route.Provider.ID, 10)
	}
	return route.Provider.BaseURL
}
