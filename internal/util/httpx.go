package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrUpstreamTimeout marks a request that exceeded its configured timeout (as
// opposed to a client-side cancellation).
var ErrUpstreamTimeout = errors.New("upstream timeout")

// httpClient has no global timeout: per-call timeouts are applied via context,
// and streaming calls deliberately run without one.
var httpClient = &http.Client{}

// FetchOptions describes one upstream request.
type FetchOptions struct {
	Method    string
	Headers   map[string]string
	Body      io.Reader
	TimeoutMs int
}

// FetchUpstream performs an upstream request. timeoutMs <= 0 disables the
// timeout (used for streaming). A timeout maps to ErrUpstreamTimeout; other
// failures (network, client cancellation) pass through unchanged.
//
// The timeout context must outlive FetchUpstream itself: callers stream
// response.Body afterwards, and cancelling on return would truncate every
// streamed body. So the cancel function is deferred to the body's Close.
func FetchUpstream(ctx context.Context, rawURL string, opts FetchOptions) (*http.Response, error) {
	if opts.TimeoutMs <= 0 {
		return do(ctx, rawURL, opts)
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
	response, err := do(requestCtx, rawURL, opts)
	if err != nil {
		cancel()
		if requestCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, fmt.Errorf("%w after %dms", ErrUpstreamTimeout, opts.TimeoutMs)
		}
		return nil, err
	}
	if response.Body == nil {
		cancel()
		return response, nil
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// cancelOnClose releases the timeout context when the body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

func do(ctx context.Context, rawURL string, opts FetchOptions) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, opts.Method, rawURL, opts.Body)
	if err != nil {
		return nil, err
	}
	for key, value := range opts.Headers {
		request.Header.Set(key, value)
	}
	return httpClient.Do(request)
}

// requestHeaderBlocklist are hop-by-hop / auth / browser-state headers that are
// never forwarded upstream. `cookie` is blocked because it belongs to this
// gateway's own origin.
var requestHeaderBlocklist = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"host":                true,
	"content-length":      true,
	"connection":          true,
	"keep-alive":          true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"expect":              true,
	"accept-encoding":     true,
	"x-request-id":        true,
}

// responseHeaderBlocklist are headers never copied back to clients.
var responseHeaderBlocklist = map[string]bool{
	"content-length":    true,
	"connection":        true,
	"keep-alive":        true,
	"transfer-encoding": true,
	"content-encoding":  true,
	"set-cookie":        true,
}

// FilterRequestHeaders drops blocked headers from a client header set.
func FilterRequestHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		if !requestHeaderBlocklist[strings.ToLower(key)] {
			out[key] = value
		}
	}
	return out
}

// FilterResponseHeaders drops blocked headers from an upstream response.
func FilterResponseHeaders(headers http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range headers {
		if responseHeaderBlocklist[strings.ToLower(key)] || len(values) == 0 {
			continue
		}
		out[key] = values[0]
	}
	return out
}

// ResolveUpstreamMethod decides the method for a forwarded config request:
// blank keeps the historical POST default, "NONE" forwards the client's verb.
func ResolveUpstreamMethod(configured, clientMethod string) string {
	pinned := strings.ToUpper(strings.TrimSpace(configured))
	if pinned == "" {
		return "POST"
	}
	if pinned == "NONE" {
		return strings.ToUpper(clientMethod)
	}
	return pinned
}

// IsFailoverStatus reports whether an upstream status triggers failover.
func IsFailoverStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500
}

// UpstreamStatusError carries a non-2xx upstream status.
type UpstreamStatusError struct {
	Status int
	Body   string
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("Upstream %d: %s", e.Status, e.Body)
}

// ReadBodyLimited reads at most limit bytes and closes the response body.
func ReadBodyLimited(response *http.Response, limit int64) string {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return ""
	}
	return string(data)
}
