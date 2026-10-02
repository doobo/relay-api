package routes

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
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
}

// NewTunnelRoutes builds the tunnel handler.
func NewTunnelRoutes(secrets *util.SecretBox) *TunnelRoutes {
	return &TunnelRoutes{secrets: secrets}
}

// Register mounts the catch-all handler under prefix.
func (h *TunnelRoutes) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/{path...}", h.handle)
}

func (h *TunnelRoutes) handle(w http.ResponseWriter, r *http.Request) {
	requestID := middleware.RequestID(r.Context())
	startedAt := time.Now()

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
		configID := apiConfig.ID
		latency := time.Since(startedAt).Milliseconds()
		_ = db.RecordUsage(db.UsageRecord{
			RequestID:   requestID,
			APIKeyID:    nil,
			Kind:        "api",
			APIConfigID: &configID,
			TargetURL:   targetURL,
			LatencyMs:   &latency,
			Status:      &status,
			Error:       errMsg,
		})
	}

	// The client verb is always forwarded as-is: a tunnel pins no method, or the
	// two halves of a split protocol (a long-polling GET down, a streaming POST
	// up) could never both be served.
	clientMethod := strings.ToUpper(r.Method)

	clientHeaders := map[string]string{}
	for key, values := range r.Header {
		if len(values) > 0 {
			clientHeaders[strings.ToLower(key)] = values[0]
		}
	}

	// Config headers still work, but only headers/query can be templated: a
	// tunnel never parses the body, so {{body.*}} has nothing to read.
	headersContext := map[string]any{}
	for key, value := range clientHeaders {
		headersContext[key] = value
	}
	queryContext := map[string]any{}
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			queryContext[key] = values[0]
		}
	}
	templateContext := map[string]any{"headers": headersContext, "query": queryContext, "body": nil}

	renderedConfigHeaders := map[string]string{}
	for key, value := range parseStringMap(apiConfig.Headers) {
		renderedConfigHeaders[key] = asString(util.RenderTemplate(value, templateContext))
	}
	upstreamHeaders := util.FilterRequestHeaders(clientHeaders)
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
	// for a long time. A non-zero timeout is enforced via the request context,
	// and a client disconnect cancels that context too (r.Context()), so a dead
	// tunnel does not keep an upstream request alive.
	response, err := util.FetchUpstream(r.Context(), parsedURL.String(), util.FetchOptions{
		Method:    clientMethod,
		Headers:   upstreamHeaders,
		Body:      body,
		TimeoutMs: apiConfig.StreamTimeoutMs,
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
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(response.StatusCode)
	record(response.StatusCode, nil)
	copyAndFlush(w, response.Body)
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

// copyAndFlush streams src into w, flushing after every chunk so a long-lived
// response is delivered as it is produced rather than sitting in the server's
// write buffer.
func copyAndFlush(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
