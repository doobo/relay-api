package util

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// ErrUpstreamTimeout marks a request that exceeded its configured timeout (as
// opposed to a client-side cancellation).
var ErrUpstreamTimeout = errors.New("upstream timeout")

// Upstream connection-pool sizing. The standard library default transport keeps
// only two idle connections per host (http.DefaultMaxIdleConnsPerHost), so
// concurrent requests to the same upstream force a fresh TCP/TLS handshake and
// a fresh DNS lookup for nearly every one. Xray's XHTTP transport fans a single
// client into many parallel POST/GET streams, which makes that default the
// largest throughput limiter when relaying it, so the pool is widened here.
const (
	maxIdleConns        = 1024
	maxIdleConnsPerHost = 256
	idleConnTimeout     = 90 * time.Second
	dialTimeout         = 10 * time.Second
	dialKeepAlive       = 30 * time.Second
	tlsSessionCacheSize = 256
)

// httpsTransport serves TLS upstreams (and plaintext HTTP/1.1 upstreams while
// h2c is disabled). ForceAttemptHTTP2 lets https endpoints negotiate HTTP/2.
//
// A custom TLSClientConfig (the session cache) and DialContext disable the
// standard library's implicit HTTP/2 setup, so ForceAttemptHTTP2 has to stay
// true. The session cache lets a freshly dialed connection resume instead of
// performing a full TLS handshake, which matters when an upstream pool grows
// during a burst.
var httpsTransport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        maxIdleConns,
	MaxIdleConnsPerHost: maxIdleConnsPerHost,
	IdleConnTimeout:     idleConnTimeout,
	TLSHandshakeTimeout: dialTimeout,
	DialContext: (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}).DialContext,
	TLSClientConfig: &tls.Config{
		ClientSessionCache: tls.NewLRUClientSessionCache(tlsSessionCacheSize),
	},
}

// h2cTransport serves cleartext HTTP/2 upstreams. It is nil unless
// UPSTREAM_H2C is set, because a plain HTTP/1.1 upstream (most APIs, and any
// server without a cleartext HTTP/2 listener) rejects an h2c preface.
var h2cTransport = newH2CTransport()

// newH2CTransport returns an h2c-capable round tripper when UPSTREAM_H2C is
// truthy, otherwise nil so plaintext upstreams keep their HTTP/1.1 behaviour.
func newH2CTransport() http.RoundTripper {
	if !upstreamH2CEnabled() {
		return nil
	}
	return &http2.Transport{
		AllowHTTP: true,
		// AllowHTTP makes the transport use h2c for http:// URLs, so the dialer
		// must hand back a plain connection instead of a TLS one. Note that
		// http2.Transport multiplexes every stream over one connection and does
		// not consult a proxy - both are deliberate for an XHTTP upstream.
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

// upstreamH2CEnabled reports whether UPSTREAM_H2C opts plaintext upstreams into
// cleartext HTTP/2, accepting the usual truthy spellings.
func upstreamH2CEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("UPSTREAM_H2C"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// transportRouter picks a transport per request scheme, so an https upstream
// keeps its TLS + HTTP/2 handling while a plaintext one can opt into h2c.
type transportRouter struct{}

func (transportRouter) RoundTrip(request *http.Request) (*http.Response, error) {
	if h2cTransport != nil && request.URL.Scheme == "http" {
		return h2cTransport.RoundTrip(request)
	}
	return httpsTransport.RoundTrip(request)
}

// httpClient has no global timeout: per-call timeouts are applied via context,
// and streaming calls deliberately run without one.
var httpClient = &http.Client{Transport: transportRouter{}}

// FetchOptions describes one upstream request.
type FetchOptions struct {
	Method    string
	Headers   map[string]string
	Body      io.Reader
	TimeoutMs int
	// URL, when set, is used directly instead of parsing rawURL. Callers that
	// already hold a parsed URL (a cached provider base, or a validated config
	// target) skip url.Parse on every request. The value is copied, so callers
	// may reuse theirs.
	URL *url.URL
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
	var (
		request *http.Request
		err     error
	)
	if opts.URL != nil {
		request = newRequestWithURL(ctx, opts.Method, opts.URL, opts.Body)
	} else {
		request, err = http.NewRequestWithContext(ctx, opts.Method, rawURL, opts.Body)
		if err != nil {
			return nil, err
		}
	}
	for key, value := range opts.Headers {
		request.Header.Set(key, value)
	}
	return httpClient.Do(request)
}

// newRequestWithURL builds a client request around an already-parsed URL. It is
// the fast path for the relay: net/http's NewRequest would parse the URL string
// again, and that parse is otherwise paid on every forwarded request. Body
// length and replay handling mirror NewRequest for the reader types the relay
// uses.
func newRequestWithURL(ctx context.Context, method string, target *url.URL, body io.Reader) *http.Request {
	u := *target
	request := &http.Request{
		Method:     method,
		URL:        &u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       u.Host,
		Body:       http.NoBody,
	}
	if body != nil {
		switch typed := body.(type) {
		case *bytes.Buffer:
			request.ContentLength = int64(typed.Len())
			request.Body = io.NopCloser(typed)
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(typed.Bytes())), nil
			}
		case *bytes.Reader:
			request.ContentLength = int64(typed.Len())
			request.Body = io.NopCloser(typed)
			snapshot := *typed
			request.GetBody = func() (io.ReadCloser, error) {
				clone := snapshot
				return io.NopCloser(&clone), nil
			}
		case *strings.Reader:
			request.ContentLength = int64(typed.Len())
			request.Body = io.NopCloser(typed)
			snapshot := *typed
			request.GetBody = func() (io.ReadCloser, error) {
				clone := snapshot
				return io.NopCloser(&clone), nil
			}
		case io.ReadCloser:
			request.Body = typed
		default:
			request.Body = io.NopCloser(typed)
		}
	}
	return request.WithContext(ctx)
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

// ForwardedRequestHeaders builds the upstream header map directly from a client
// header set, lowercasing each name once and dropping hop-by-hop/auth headers.
// It is the single-pass equivalent of normalizing a header set and then calling
// FilterRequestHeaders.
func ForwardedRequestHeaders(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		name := strings.ToLower(key)
		if requestHeaderBlocklist[name] {
			continue
		}
		out[name] = values[0]
	}
	return out
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
