package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/util"
)

// tunnelHarness wires the /free tunnel exactly as main.go does: no body parser
// and no API-key auth in front of it.
type tunnelHarness struct {
	handler http.Handler
	secrets *util.SecretBox
}

func newTunnelHarness(t *testing.T) tunnelHarness {
	t.Helper()
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "gateway.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	// The httptest mock listens on 127.0.0.1; allow private upstreams.
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "1")

	if _, err := db.Open(filepath.Join(t.TempDir(), "tunnel.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(db.Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	secrets, err := util.NewSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}

	mux := http.NewServeMux()
	NewTunnelRoutes(secrets).Register(mux, "/free")
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	return tunnelHarness{handler: middleware.RequestLog(mux), secrets: secrets}
}

func TestTunnelPassthrough(t *testing.T) {
	harness := newTunnelHarness(t)

	var (
		mu      sync.Mutex
		lastReq capturedRequest
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastReq = capturedRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Headers: r.Header.Clone(), Body: string(body)}
		mu.Unlock()
		w.Header().Set("content-type", "application/octet-stream")
		_, _ = w.Write([]byte("echo:" + string(body)))
	}))
	defer mock.Close()

	capture := func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return lastReq
	}

	encrypted, err := harness.secrets.Encrypt("config-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	headers := `{"X-Cfg":"{{headers.xcustom}}"}`
	mustConfig(t, db.NewAPIConfig{
		Name: "raw", Method: "NONE", URL: mock.URL + "/echo",
		Headers: &headers, APIKey: &encrypted,
		StreamTimeoutMs: 15000, StreamMaxBodyMb: 0, Route: "free", Enabled: true,
	})
	// A wildcard config: the tail and the query string must survive.
	mustConfig(t, db.NewAPIConfig{
		Name: "api/**", Method: "POST", URL: mock.URL + "/base",
		StreamTimeoutMs: 15000, Route: "free", Enabled: true,
	})
	// Same name but on /open: the tunnel must not match it.
	mustConfig(t, db.NewAPIConfig{
		Name: "onlyopen", Method: "POST", URL: mock.URL + "/open-only",
		StreamTimeoutMs: 15000, Route: "open", Enabled: true,
	})

	call := func(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		harness.handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("raw body and config headers pass through", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/free/raw?x=1", "hello raw", map[string]string{"Xcustom": "abc"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if recorder.Body.String() != "echo:hello raw" {
			t.Fatalf("body = %q", recorder.Body.String())
		}
		if recorder.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing X-Request-ID")
		}
		seen := capture()
		if seen.Method != http.MethodPost || seen.Path != "/echo" {
			t.Fatalf("upstream saw %s %s", seen.Method, seen.Path)
		}
		if seen.RawQuery != "x=1" {
			t.Fatalf("query = %q", seen.RawQuery)
		}
		if seen.Body != "hello raw" {
			t.Fatalf("body passthrough = %q", seen.Body)
		}
		if seen.Headers.Get("X-Cfg") != "abc" {
			t.Fatalf("config header not templated: %q", seen.Headers.Get("X-Cfg"))
		}
		if seen.Headers.Get("Accept-Encoding") != "identity" {
			t.Fatalf("accept-encoding = %q", seen.Headers.Get("Accept-Encoding"))
		}
		if seen.Headers.Get("Authorization") != "Bearer config-secret" {
			t.Fatalf("config api_key not applied: %q", seen.Headers.Get("Authorization"))
		}
	})

	t.Run("wildcard forwards tail and query", func(t *testing.T) {
		recorder := call(t, http.MethodGet, "/free/api/users/1?fields=name", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		seen := capture()
		if seen.Path != "/base/users/1" || seen.RawQuery != "fields=name" {
			t.Fatalf("upstream saw path=%q query=%q", seen.Path, seen.RawQuery)
		}
	})

	t.Run("config on the open route is not served by the tunnel", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/free/onlyopen", "{}", nil)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "config_not_found") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown config records a 404", func(t *testing.T) {
		recorder := call(t, http.MethodGet, "/free/nope", "", nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
}

func TestTunnelBodyLimit(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "small", Method: "POST", URL: mock.URL,
		StreamTimeoutMs: 15000, StreamMaxBodyMb: 1, Route: "free", Enabled: true,
	})

	t.Run("declared content-length over limit is rejected fast", func(t *testing.T) {
		big := strings.Repeat("a", 2*1024*1024)
		request := httptest.NewRequest(http.MethodPost, "/free/small", strings.NewReader(big))
		recorder := httptest.NewRecorder()
		harness.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "request_too_large") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown content-length is cut off while streaming", func(t *testing.T) {
		// A plain io.Reader hides its length, so the request is sent chunked and
		// the handler cannot fast-reject: the limit must trip mid-stream.
		big := strings.Repeat("a", 2*1024*1024)
		request := httptest.NewRequest(http.MethodPost, "/free/small", io.NopCloser(strings.NewReader(big)))
		request.ContentLength = -1
		recorder := httptest.NewRecorder()
		harness.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "request_too_large") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("body under the limit is forwarded", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/free/small", strings.NewReader("small body"))
		recorder := httptest.NewRecorder()
		harness.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

// TestTunnelClientDisconnect verifies that cancelling the client request (as a
// disconnect would) cancels the in-flight upstream call and records 499.
func TestTunnelClientDisconnect(t *testing.T) {
	harness := newTunnelHarness(t)

	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server's background read can detect the client
		// closing the connection and cancel r.Context().
		go func() { _, _ = io.Copy(io.Discard, r.Body) }()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "block", Method: "POST", URL: mock.URL,
		StreamTimeoutMs: 0, Route: "free", Enabled: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/free/block", strings.NewReader("x")).WithContext(ctx)
	recorder := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		harness.handler.ServeHTTP(recorder, request)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received the request")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was not cancelled after the client disconnected")
	}
	if recorder.Code != 499 {
		t.Fatalf("status = %d, want 499", recorder.Code)
	}
}

// firstWriteRecorder signals the first body write so a test can cancel the
// client only after the relay has already streamed something back.
type firstWriteRecorder struct {
	*httptest.ResponseRecorder
	wrote chan struct{}
	once  sync.Once
}

func (r *firstWriteRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if n > 0 {
		r.once.Do(func() { close(r.wrote) })
	}
	return n, err
}

// TestTunnelClientDisconnectMidStreamKeepsStatus verifies that a client which
// closes after receiving body bytes is not logged as a 504: the upstream read
// fails with context.Canceled, but that is a client-initiated end to the
// stream, not an upstream gateway timeout. The row keeps the upstream status
// (so a successful long stream stays a 200) and explains the early close.
func TestTunnelClientDisconnectMidStreamKeepsStatus(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream has no flusher")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		flusher.Flush()
		// Stay open like a long-lived stream; the client will end it.
		<-r.Context().Done()
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "midstream", Method: "GET", URL: mock.URL,
		StreamTimeoutMs: 0, Route: "free", Enabled: true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/free/midstream", nil).WithContext(ctx)
	recorder := &firstWriteRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		wrote:            make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		harness.handler.ServeHTTP(recorder, request)
		close(done)
	}()

	select {
	case <-recorder.wrote:
	case <-time.After(3 * time.Second):
		t.Fatal("relay never streamed a body chunk to the client")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}
	if err := db.FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	var status *int
	var message *string
	row := db.Get().QueryRow(`SELECT status, error FROM usage_logs ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&status, &message); err != nil {
		t.Fatalf("scan usage: %v", err)
	}
	if status == nil || *status != http.StatusOK {
		t.Fatalf("recorded status = %v, want 200", status)
	}
	if message == nil || !strings.Contains(*message, "client disconnected after") {
		t.Fatalf("recorded error = %v, want a client-disconnect note", message)
	}
}

// TestTunnelStreamTimeout verifies that a stream which stops producing data is
// cut by streamTimeoutMs and recorded as 504 instead of lingering forever (and
// being logged as a clean 200).
func TestTunnelStreamTimeout(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("partial"))
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "stall", Method: "GET", URL: mock.URL,
		StreamTimeoutMs: 150, Route: "free", Enabled: true,
	})

	started := time.Now()
	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/free/stall", nil))
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("handler ran for %v; the stream timeout did not cut it short", elapsed)
	}
	if err := db.FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	var status *int
	var message *string
	row := db.Get().QueryRow(`SELECT status, error FROM usage_logs ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&status, &message); err != nil {
		t.Fatalf("scan usage: %v", err)
	}
	if status == nil || *status != http.StatusGatewayTimeout {
		t.Fatalf("recorded status = %v, want 504", status)
	}
	if message == nil || !strings.Contains(*message, "timeout") {
		t.Fatalf("recorded error = %v, want a timeout", message)
	}
}

// TestTunnelUsage verifies the usage row shape: kind=api, no API key, and a
// target_url without the query string.
func TestTunnelUsage(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "logged", Method: "POST", URL: mock.URL + "/path",
		StreamTimeoutMs: 15000, Route: "free", Enabled: true,
	})

	request := httptest.NewRequest(http.MethodPost, "/free/logged?secret=1", strings.NewReader("{}"))
	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	// The tunnel records usage asynchronously; flush the batch writer so the
	// row is visible before it is asserted on.
	if err := db.FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	var (
		kind      string
		apiKeyID  *int64
		targetURL *string
		status    *int
	)
	row := db.Get().QueryRow(
		`SELECT kind, api_key_id, target_url, status FROM usage_logs ORDER BY id DESC LIMIT 1`,
	)
	if err := row.Scan(&kind, &apiKeyID, &targetURL, &status); err != nil {
		t.Fatalf("scan usage: %v", err)
	}
	if kind != "api" || apiKeyID != nil {
		t.Fatalf("kind = %q, api_key_id = %v", kind, apiKeyID)
	}
	if targetURL == nil || *targetURL != mock.URL+"/path" {
		t.Fatalf("target_url = %v", targetURL)
	}
	if status == nil || *status != http.StatusOK {
		t.Fatalf("status = %v", status)
	}
}

// TestTunnelActiveStreamOutlivesTimeout verifies StreamTimeoutMs is an
// inactivity limit: a stream that keeps producing data is not cut just because
// it has run longer than the configured timeout.
func TestTunnelActiveStreamOutlivesTimeout(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream has no flusher")
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 8; i++ {
			_, _ = w.Write([]byte("chunk\n"))
			flusher.Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "active", Method: "GET", URL: mock.URL,
		StreamTimeoutMs: 80, Route: "free", Enabled: true,
	})

	started := time.Now()
	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/free/active", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("stream finished in %v; the upstream did not stream long enough", elapsed)
	}
	if got := strings.Count(recorder.Body.String(), "chunk"); got != 8 {
		t.Fatalf("received %d chunks, want 8", got)
	}
}

// TestTunnelClientIP verifies the usage row records the caller address,
// preferring Cloudflare's CF-Connecting-IP over the socket address.
func TestTunnelClientIP(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "ip", Method: "POST", URL: mock.URL,
		StreamTimeoutMs: 15000, Route: "free", Enabled: true,
	})

	request := httptest.NewRequest(http.MethodPost, "/free/ip", strings.NewReader("{}"))
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	recorder := httptest.NewRecorder()
	harness.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if err := db.FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	var clientIP *string
	if err := db.Get().QueryRow(`SELECT client_ip FROM usage_logs ORDER BY id DESC LIMIT 1`).Scan(&clientIP); err != nil {
		t.Fatalf("scan usage: %v", err)
	}
	if clientIP == nil || *clientIP != "203.0.113.9" {
		t.Fatalf("client_ip = %v, want 203.0.113.9", clientIP)
	}
}

// TestTunnelForwardsTrailers verifies an upstream's HTTP trailers survive the
// relay (gRPC relies on them for grpc-status).
func TestTunnelForwardsTrailers(t *testing.T) {
	harness := newTunnelHarness(t)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
		w.Header().Set("X-Trailer", "done")
	}))
	defer mock.Close()

	mustConfig(t, db.NewAPIConfig{
		Name: "trailers", Method: "GET", URL: mock.URL,
		StreamTimeoutMs: 15000, Route: "free", Enabled: true,
	})

	proxy := httptest.NewServer(harness.handler)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/free/trailers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "body" {
		t.Fatalf("body = %q", body)
	}
	if got := response.Trailer.Get("X-Trailer"); got != "done" {
		t.Fatalf("trailer = %q, want done", got)
	}
}
