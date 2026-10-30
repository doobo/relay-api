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
	"strings"
	"sync"
	"sync/atomic"
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
// largest throughput limiter when relaying it. The pool is therefore sized for
// hundreds of concurrent AI requests to one host, so a 300-way burst reuses
// warm connections instead of churning through handshakes (and file
// descriptors) it cannot get back.
const (
	maxIdleConns        = 2048
	maxIdleConnsPerHost = 1024
	idleConnTimeout     = 90 * time.Second
	dialTimeout         = 10 * time.Second
	dialKeepAlive       = 30 * time.Second
	tlsSessionCacheSize = 512
	// A 1 MiB HTTP/2 frame cuts frame-header overhead on bulk relay traffic
	// while staying well below the 16 MiB maximum (each connection may have to
	// buffer one frame per direction).
	upstreamMaxReadFrameSize = 1 << 20
	// Ping an HTTP/2 connection after this much silence. A long-lived stream
	// can otherwise sit on a connection whose peer disappeared without a FIN;
	// the ping turns that into a prompt connection failure.
	upstreamHTTP2SendPing = 30 * time.Second
	upstreamHTTP2PingWait = 15 * time.Second
)

// httpsTransport serves https upstreams and plaintext http upstreams over
// HTTP/1.1. ForceAttemptHTTP2 lets https endpoints negotiate HTTP/2.
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
	// Tune HTTP/2 itself: bigger frames reduce framing overhead, and the ping
	// timeout detects an upstream connection that died silently during a long
	// stream. Go's HTTP/2 defaults otherwise apply (notably a 1 MiB receive
	// window per stream), so a high-BDP upstream is not throttled to
	// window/RTT.
	HTTP2: &http.HTTP2Config{
		MaxReadFrameSize: upstreamMaxReadFrameSize,
		SendPingTimeout:  upstreamHTTP2SendPing,
		PingTimeout:      upstreamHTTP2PingWait,
	},
}

// h2cTransport serves cleartext HTTP/2 upstreams. It is used only by requests
// whose config sets FetchOptions.H2C, because a plain HTTP/1.1 upstream (most
// APIs, and any server without a cleartext HTTP/2 listener) rejects an h2c
// preface.
var h2cTransport = &http2.Transport{
	AllowHTTP: true,
	// Match the TLS transport's frame size and silence detection so an h2c
	// upstream gets the same throughput and dead-connection handling.
	MaxReadFrameSize: upstreamMaxReadFrameSize,
	ReadIdleTimeout:  upstreamHTTP2SendPing,
	PingTimeout:      upstreamHTTP2PingWait,
	// AllowHTTP makes the transport use h2c for http:// URLs, so the dialer
	// must hand back a plain connection instead of a TLS one. Note that
	// http2.Transport multiplexes every stream over one connection and does
	// not consult a proxy - both are deliberate for an XHTTP upstream.
	DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	},
}

// httpClient has no global timeout: per-call timeouts are applied via context,
// and streaming calls deliberately run without one.
var httpClient = &http.Client{Transport: httpsTransport}

// h2cClient sends cleartext HTTP/2 for http:// upstreams that opt in.
var h2cClient = &http.Client{Transport: h2cTransport}

// FetchOptions describes one upstream request.
type FetchOptions struct {
	Method  string
	Headers map[string]string
	Body    io.Reader
	// TimeoutMs bounds the whole request, including reading the body. Use it
	// for non-streaming calls.
	TimeoutMs int
	// HeaderTimeoutMs bounds only the wait for response headers. It is for
	// streaming calls, where TimeoutMs is 0 and a stalled upstream must not
	// hold the request open forever: once headers arrive the timer stops and
	// the body may stream for as long as it needs.
	HeaderTimeoutMs int
	// IdleTimeoutMs bounds inactivity while reading a streaming response
	// body. The timer restarts on every byte received, so a long-lived stream
	// that keeps producing data may run past it, while one that stops
	// producing data is cancelled (ErrUpstreamTimeout) instead of pinning the
	// handler forever. Ignored when TimeoutMs > 0, because that already bounds
	// the whole call.
	IdleTimeoutMs int
	// URL, when set, is used directly instead of parsing rawURL. Callers that
	// already hold a parsed URL (a cached provider base, or a validated config
	// target) skip url.Parse on every request. The value is copied, so callers
	// may reuse theirs.
	URL *url.URL
	// H2C sends an http:// upstream over cleartext HTTP/2 instead of HTTP/1.1.
	// Only set it for upstreams known to speak h2c; https:// URLs ignore it.
	H2C bool
}

// FetchUpstream performs an upstream request. timeoutMs <= 0 disables the
// overall timeout (used for streaming). A timeout maps to ErrUpstreamTimeout;
// other failures (network, client cancellation) pass through unchanged.
//
// HeaderTimeoutMs, when set on a streaming call, bounds only the wait for
// response headers. Without it a stalled upstream that never sends headers
// would hold the stream (and its goroutine/socket) open indefinitely.
//
// The timeout context must outlive FetchUpstream itself: callers stream
// response.Body afterwards, and cancelling on return would truncate every
// streamed body. So the cancel function is deferred to the body's Close.
func FetchUpstream(ctx context.Context, rawURL string, opts FetchOptions) (*http.Response, error) {
	if opts.TimeoutMs <= 0 && opts.HeaderTimeoutMs <= 0 && opts.IdleTimeoutMs <= 0 {
		return do(ctx, rawURL, opts)
	}

	var (
		requestCtx context.Context
		cancel     context.CancelFunc
	)
	if opts.TimeoutMs > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, time.Duration(opts.TimeoutMs)*time.Millisecond)
	} else {
		requestCtx, cancel = context.WithCancel(ctx)
	}

	// The header deadline is a plain timer, not a context deadline: it must be
	// stopped the moment headers arrive so a long stream is not cut off.
	var headerTimer *time.Timer
	if opts.TimeoutMs <= 0 && opts.HeaderTimeoutMs > 0 {
		headerTimer = time.AfterFunc(
			time.Duration(opts.HeaderTimeoutMs)*time.Millisecond,
			cancel,
		)
	}

	response, err := do(requestCtx, rawURL, opts)
	if headerTimer != nil && !headerTimer.Stop() && ctx.Err() == nil {
		// The header timer fired (or was firing) while the request was still in
		// flight. Treat it as an upstream timeout rather than handing back a
		// body whose context is already cancelled.
		cancel()
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("%w after %dms", ErrUpstreamTimeout, opts.HeaderTimeoutMs)
	}
	if err != nil {
		cancel()
		if opts.TimeoutMs > 0 && requestCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			return nil, fmt.Errorf("%w after %dms", ErrUpstreamTimeout, opts.TimeoutMs)
		}
		return nil, err
	}
	if response.Body == nil {
		cancel()
		return response, nil
	}
	body := response.Body
	if opts.TimeoutMs <= 0 && opts.IdleTimeoutMs > 0 {
		body = newIdleTimeoutBody(body, cancel, time.Duration(opts.IdleTimeoutMs)*time.Millisecond)
	}
	response.Body = &cancelOnClose{ReadCloser: body, cancel: cancel}
	return response, nil
}

// idleTimeoutBody cancels the request when the upstream stops producing body
// bytes for the configured interval. Resetting on every read (rather than using
// a single deadline) lets an active long-lived stream outlive the timeout while
// still reaping one that has silently stalled.
type idleTimeoutBody struct {
	io.ReadCloser
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
}

func newIdleTimeoutBody(body io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) *idleTimeoutBody {
	idle := &idleTimeoutBody{ReadCloser: body, cancel: cancel, timeout: timeout}
	idle.timer = time.AfterFunc(timeout, func() {
		idle.fired.Store(true)
		cancel()
	})
	return idle
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.fired.Load() {
		return n, fmt.Errorf("%w after %dms", ErrUpstreamTimeout, b.timeout.Milliseconds())
	}
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	if err != nil {
		b.timer.Stop()
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
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
	if opts.H2C && request.URL.Scheme == "http" {
		return h2cClient.Do(request)
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
