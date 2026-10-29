package routes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/util"
)

// bodyLimitMarker mirrors the reference tunnel's error text: it lets the error
// path recognise an oversized upload even when the HTTP stack wraps the reader
// error before it reaches us (the `exceeded` flag is the primary signal).
const bodyLimitMarker = "tunnel body exceeds configured limit"

// errBodyTooLarge is returned by limitedBody once a body crosses its cap.
var errBodyTooLarge = errors.New(bodyLimitMarker)

// TunnelRoutes serves the public /free/* raw streaming tunnel. Unlike the JSON
// forwarder on /open/*, it never buffers or parses the body: the client stream
// is piped upstream untouched and the upstream response is piped back the same
// way, so long-lived streaming/long-polling protocols can ride it.
//
// There is deliberately no auth, scope, quota or rate-limit middleware in front
// of it: /free configs are registered as public. SSRF checks, header filtering
// and per-config size/time limits still apply.
type TunnelRoutes struct {
	secrets *util.SecretBox

	// usageSample records one in N successful tunnel requests. XHTTP packet
	// mode turns a single client into thousands of tiny requests, so writing a
	// usage row for every one of them is mostly noise; errors are always
	// recorded. 1 (the default) records every request.
	usageSample int
	usageSeq    atomic.Uint64
}

// NewTunnelRoutes builds the tunnel handler.
func NewTunnelRoutes(secrets *util.SecretBox) *TunnelRoutes {
	return &TunnelRoutes{secrets: secrets, usageSample: 1}
}

// SetUsageSample records one in every n successful tunnel requests. n <= 1
// keeps every request.
func (h *TunnelRoutes) SetUsageSample(n int) {
	if n < 1 {
		n = 1
	}
	h.usageSample = n
}

// shouldRecord decides whether a tunnel response is written to usage_logs under
// the configured sampling rate. Only successful responses are sampled.
func (h *TunnelRoutes) shouldRecord(status int, errMsg *string) bool {
	if h.usageSample <= 1 || errMsg != nil || status < 200 || status >= 300 {
		return true
	}
	seq := h.usageSeq.Add(1)
	return (seq-1)%uint64(h.usageSample) == 0
}

// Register mounts the catch-all handler under prefix.
func (h *TunnelRoutes) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/{path...}", h.handle)
}

func (h *TunnelRoutes) handle(w http.ResponseWriter, r *http.Request) {
	requestID := middleware.RequestID(r.Context())
	startedAt := time.Now()

	clientIP := util.ClientIP(r)
	configPath := strings.Trim(r.PathValue("path"), "/")
	apiConfig, rest, err := db.ResolveAPIConfig(configPath, "free")
	if err != nil {
		internalError(w, "resolve api config", err)
		return
	}
	if apiConfig == nil {
		message := "API config '" + configPath + "' not found"
		if wildcard, _ := db.FindWildcardConfigBelow(configPath, "free"); wildcard != "" {
			message = "API config '" + configPath + "' not found - '" + wildcard +
				"' only matches paths below it, e.g. /free/" + configPath + "/<id>"
		}
		httperr.Write(w, http.StatusNotFound, message, "not_found_error", "config_not_found")
		return
	}

	var targetURL *string
	record := func(status int, errMsg *string) {
		if !h.shouldRecord(status, errMsg) {
			return
		}
		configID := apiConfig.ID
		latency := time.Since(startedAt).Milliseconds()
		ip := clientIP
		db.RecordUsageAsync(db.UsageRecord{
			RequestID:   requestID,
			APIKeyID:    nil,
			Kind:        "api",
			APIConfigID: &configID,
			TargetURL:   targetURL,
			LatencyMs:   &latency,
			Status:      &status,
			Stream:      true,
			Error:       errMsg,
			ClientIP:    &ip,
		})
	}

	// The client verb is always forwarded as-is: a tunnel pins no method, or the
	// two halves of a split protocol (a long-polling GET down, a streaming POST
	// up) could never both be served.
	clientMethod := strings.ToUpper(r.Method)

	headerTemplates := configHeaderTemplates(apiConfig)

	// Config headers still work, but only headers/query can be templated: a
	// tunnel never parses the body, so {{body.*}} has nothing to read. Building
	// the template context is skipped entirely for the common static-header
	// case, and the upstream map is built in a single header pass.
	var templateContext map[string]any
	if headerTemplatesDynamic(headerTemplates) {
		headersContext := map[string]any{}
		for key, values := range r.Header {
			if len(values) > 0 {
				headersContext[strings.ToLower(key)] = values[0]
			}
		}
		queryContext := map[string]any{}
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				queryContext[key] = values[0]
			}
		}
		templateContext = map[string]any{"headers": headersContext, "query": queryContext, "body": nil}
	}

	var renderedConfigHeaders map[string]string
	if len(headerTemplates) > 0 {
		renderedConfigHeaders = make(map[string]string, len(headerTemplates))
		for key, template := range headerTemplates {
			renderedConfigHeaders[key] = asString(template.Render(templateContext))
		}
	}
	upstreamHeaders := util.ForwardedRequestHeaders(r.Header)
	for key, value := range renderedConfigHeaders {
		upstreamHeaders[key] = value
	}
	// Never negotiate compression on a tunnel: a compressed upstream stream
	// would have to be decompressed (and thus buffered) on the way back, which
	// breaks long-lived protocols and can re-encode bytes the client expects
	// verbatim.
	upstreamHeaders["accept-encoding"] = "identity"
	upstreamHeaders["x-request-id"] = requestID

	// Stored config keys are encrypted at rest; upstream needs plaintext.
	if apiConfig.APIKey != nil && *apiConfig.APIKey != "" {
		plaintext, err := h.secrets.Decrypt(*apiConfig.APIKey)
		if err != nil {
			internalError(w, "decrypt api config key", err)
			return
		}
		if plaintext != "" && !hasHeader(renderedConfigHeaders, "authorization") {
			upstreamHeaders["authorization"] = "Bearer " + plaintext
		}
	}

	parsedURL, err := util.ValidateUpstreamURL(apiConfig.URL)
	if err != nil {
		invalidRequest(w, err.Error())
		return
	}
	if rest != "" {
		parsedURL.Path = strings.TrimRight(parsedURL.Path, "/") + "/" + rest
	}
	parsedURL.RawQuery = r.URL.RawQuery
	// Recorded without the query string: it can carry client secrets.
	target := parsedURL.Scheme + "://" + parsedURL.Host + parsedURL.Path
	targetURL = &target

	// Raw body passthrough: the client stream is handed upstream untouched, so
	// nothing is buffered, decoded as text, or JSON-parsed. The size cap is
	// opt-in per config (0 = unlimited) and enforced while streaming.
	maxBodyBytes := int64(0)
	if apiConfig.StreamMaxBodyMb > 0 {
		maxBodyBytes = int64(apiConfig.StreamMaxBodyMb) * 1024 * 1024
	}
	hasBody := clientMethod != http.MethodGet && clientMethod != http.MethodHead
	var (
		body    io.Reader
		limited *limitedBody
	)
	if hasBody && r.Body != nil {
		// Fast reject when the declared size already exceeds the cap.
		if maxBodyBytes > 0 && r.ContentLength > maxBodyBytes {
			message := "request body too large"
			record(http.StatusBadRequest, &message)
			writeTooLarge(w, apiConfig.StreamMaxBodyMb)
			return
		}
		if maxBodyBytes > 0 {
			limited = &limitedBody{reader: r.Body, limit: maxBodyBytes}
			body = limited
		} else {
			body = r.Body
		}
	}

	// A zero stream timeout means "no timeout": a tunnel may legitimately idle
	// for a long time. A non-zero timeout is an inactivity limit, not a total
	// duration: HeaderTimeoutMs bounds the wait for response headers and
	// IdleTimeoutMs restarts on every body byte, so an active multi-hour stream
	// is never cut just for being old. A client disconnect cancels the request
	// context too (r.Context()), so a dead tunnel does not keep an upstream
	// request alive.
	response, err := util.FetchUpstream(r.Context(), "", util.FetchOptions{
		Method:          clientMethod,
		Headers:         upstreamHeaders,
		Body:            body,
		HeaderTimeoutMs: apiConfig.StreamTimeoutMs,
		IdleTimeoutMs:   apiConfig.StreamTimeoutMs,
		URL:             parsedURL,
	})
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone, so nobody can read a response - just stop. The
			// aborted call is still recorded (499), so a dead tunnel leaves a
			// trace instead of silently vanishing from the usage log.
			message := "client disconnected"
			record(499, &message)
			w.WriteHeader(499)
			return
		}
		message := err.Error()
		tooLargeError := (limited != nil && limited.exceeded) || strings.Contains(message, bodyLimitMarker)
		if tooLargeError {
			record(http.StatusBadRequest, &message)
			writeTooLarge(w, apiConfig.StreamMaxBodyMb)
			return
		}
		if errors.Is(err, util.ErrUpstreamTimeout) {
			record(http.StatusGatewayTimeout, &message)
			httperr.Write(w, http.StatusGatewayTimeout, "Upstream request timed out", "upstream_error", "upstream_timeout")
			return
		}
		record(http.StatusBadGateway, &message)
		httperr.Write(w, http.StatusBadGateway, message, "upstream_error", "upstream_error")
		return
	}
	defer response.Body.Close()

	// Stream the upstream response straight back - the body is copied in chunks
	// and flushed as it arrives, so chunked/push responses keep their timing.
	for key, value := range util.FilterResponseHeaders(response.Header) {
		w.Header().Set(key, value)
	}
	// HTTP/2 trailers carry gRPC's grpc-status and similar end-of-stream
	// metadata. They only survive the relay when the keys are declared before
	// WriteHeader; forwardTrailers below fills in the values after the copy.
	declaredTrailers := declareTrailers(w, response.Trailer)
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(response.StatusCode)
	// Recorded after the response is copied: the usage write no longer sits
	// between the client and its first byte (and, being async, does not block
	// the handler on the single SQLite connection either). A stream reaped by
	// the configured idle timeout is recorded as 504; a client that leaves
	// after receiving data keeps the upstream status and gets a note instead.
	written, copyErr := copyAndFlush(w, response.Body)
	forwardTrailers(w, response.Trailer, declaredTrailers)
	switch {
	case copyErr == nil:
		record(response.StatusCode, nil)
	case r.Context().Err() != nil, errors.Is(copyErr, errClientDisconnected):
		// The client closed the stream (or its write failed) while the response
		// was being relayed. Cancelling the request also cancels the upstream
		// body read, so this must be classified before the read-error branch:
		// otherwise a client-initiated close would be logged as a 504 upstream
		// timeout, which is the false-504 this tunnel is meant to avoid.
		//
		// Data was already flowing, so keep the upstream's status (a successful
		// long stream stays a 200) and only annotate the row with the early
		// close. A disconnect before any body byte is still worth flagging as
		// 499, mirroring the pre-header disconnect path above.
		message := fmt.Sprintf("client disconnected after %d bytes", written)
		if written > 0 {
			record(response.StatusCode, &message)
		} else {
			record(499, &message)
		}
	case errors.Is(copyErr, util.ErrUpstreamTimeout), errors.Is(copyErr, context.DeadlineExceeded):
		// A timeout that fires mid-stream arrives here as a body read error;
		// name it explicitly so the usage row is diagnosable.
		message := copyErr.Error()
		if !strings.Contains(message, "timeout") {
			message = fmt.Sprintf("%v after %dms", util.ErrUpstreamTimeout, apiConfig.StreamTimeoutMs)
		}
		record(http.StatusGatewayTimeout, &message)
	default:
		// A read/reset failure that is not a timeout is an upstream failure,
		// not a gateway timeout: 502 identifies the side that broke.
		message := copyErr.Error()
		record(http.StatusBadGateway, &message)
	}
}

// declareTrailers announces the upstream's trailer keys before the response
// headers are committed. It returns the lowercased key set so forwardTrailers
// knows which names were predeclared and which need the TrailerPrefix form.
func declareTrailers(w http.ResponseWriter, trailers http.Header) map[string]bool {
	if len(trailers) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(trailers))
	for key := range trailers {
		w.Header().Add("Trailer", key)
		declared[strings.ToLower(key)] = true
	}
	return declared
}

// forwardTrailers writes the trailer values collected while copying the body.
// Keys that could not be predeclared (an upstream that only reveals trailers at
// EOF) use http.TrailerPrefix, which net/http recognises for both HTTP/1.1
// chunked trailers and HTTP/2 trailing HEADERS.
func forwardTrailers(w http.ResponseWriter, trailers http.Header, declared map[string]bool) {
	if len(trailers) == 0 {
		return
	}
	wrote := false
	for key, values := range trailers {
		if len(values) == 0 {
			continue
		}
		name := key
		if !declared[strings.ToLower(key)] {
			name = http.TrailerPrefix + key
		}
		w.Header()[name] = values
		wrote = true
	}
	if wrote {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

// writeTooLarge renders the shared 400 for an over-limit tunnel body.
func writeTooLarge(w http.ResponseWriter, limitMb int) {
	message := "Request body too large (limit " + strconv.Itoa(limitMb) + " MB)"
	httperr.Write(w, http.StatusBadRequest, message, "invalid_request_error", "request_too_large")
}

// limitedBody wraps the client's raw body so an oversized upload is cut off
// while it streams, instead of being buffered first. `exceeded` is recorded
// when the cap trips, so the caller can report a size error even if the HTTP
// stack wraps the reader error before it surfaces.
type limitedBody struct {
	reader   io.Reader
	limit    int64
	seen     int64
	exceeded bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.seen += int64(n)
	if b.seen > b.limit {
		b.exceeded = true
		return n, errBodyTooLarge
	}
	return n, err
}

// Tunnel copy tuning. The previous loop flushed every 32 KiB, turning a bulk
// relay into one syscall/DATA frame per chunk; a larger buffer plus a throttle
// keeps the response prompt while cutting the flush count by an order of
// magnitude.
//
// The buffer is held for the whole response, including while blocked on a read
// with nothing to relay, so its size multiplies by the number of concurrent
// tunnels: 3000 idle XHTTP streams at 256 KiB alone would be 750 MiB. 64 KiB
// still saturates a bulk relay while keeping that worst case near 190 MiB, and
// GOMEMLIMIT (see README) makes the runtime return pooled buffers under
// pressure.
const (
	tunnelCopyBuffer    = 64 * 1024
	tunnelFlushBytes    = 32 * 1024
	tunnelFlushInterval = 200 * time.Microsecond
)

// tunnelBufferPool recycles the per-response copy buffer. A high-frequency
// tunnel would otherwise allocate 256 KiB for every short request, which is
// pure GC pressure at packet-mode rates.
var tunnelBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, tunnelCopyBuffer)
		return &buffer
	},
}

// errClientDisconnected marks a tunnel copy that failed writing to the client
// rather than reading from upstream, so the usage row can record 499.
var errClientDisconnected = errors.New("client disconnected")

// copyAndFlush streams src into w so a long-lived response is delivered as it
// is produced. It flushes once enough has accumulated, once a short read shows
// nothing more is buffered upstream (so the client sees it immediately), or
// once the last flush is getting stale. Without a flusher it degrades to
// io.Copy.
//
// It returns the number of bytes relayed to the client along with the outcome:
// nil on a clean end of stream, errClientDisconnected when the client went away,
// and the read error otherwise (an upstream reset, or the configured stream
// timeout expiring mid-response). The byte count lets the caller tell a client
// that left after receiving data from one that vanished before the body began.
func copyAndFlush(w http.ResponseWriter, src io.Reader) (int64, error) {
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		written, err := io.Copy(w, src)
		if err != nil {
			return written, err
		}
		return written, nil
	}
	bufferPtr := tunnelBufferPool.Get().(*[]byte)
	defer tunnelBufferPool.Put(bufferPtr)
	buffer := *bufferPtr
	var (
		pending int
		written int64
	)
	lastFlush := time.Now()
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return written, fmt.Errorf("%w: %v", errClientDisconnected, writeErr)
			}
			written += int64(n)
			pending += n
		}
		if pending > 0 && (pending >= tunnelFlushBytes ||
			n < len(buffer) ||
			time.Since(lastFlush) >= tunnelFlushInterval) {
			flusher.Flush()
			pending = 0
			lastFlush = time.Now()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return written, nil
			}
			return written, err
		}
	}
}
