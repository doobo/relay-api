package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/providers"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

var (
	anthropicMessageStartMarker = []byte(`"message_start"`)
	anthropicMessageDeltaMarker = []byte(`"message_delta"`)
)

// messages handles POST /v1/messages, the Anthropic Messages API. Routes to
// anthropic providers are passed through: the client body reaches the upstream
// unchanged (tools, tool results, system prompt, ...), and only the model name
// and the credential are swapped. Token usage is read from the response.
func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	auth := middleware.APIAuthFrom(r.Context())
	requestID := middleware.RequestID(r.Context())
	startedAt := time.Now()

	summary, ok := middleware.RequestSummaryFrom(r.Context())
	if !ok || summary.Model == "" {
		invalidRequest(w, "model is required")
		return
	}
	model := summary.Model
	stream := summary.Stream
	body := middleware.RequestBody(r.Context())

	routes, err := g.resolver.Resolve(model)
	if err != nil {
		switch {
		case errors.Is(err, router.ErrModelNotFound):
			httperr.Write(w, http.StatusNotFound, "Model '"+model+"' not found", "not_found_error", "model_not_found")
		case errors.Is(err, router.ErrProviderUnavailable):
			httperr.Write(w, http.StatusNotFound, "Provider for model '"+model+"' is unavailable", "not_found_error", "provider_unavailable")
		default:
			internalError(w, "resolve model", err)
		}
		return
	}

	apiKeyID := int64(0)
	if auth != nil {
		apiKeyID = auth.Key.ID
	}

	var anthropicRoutes []router.Route
	for _, route := range routes {
		if route.Provider.Type == "anthropic" {
			anthropicRoutes = append(anthropicRoutes, route)
		}
	}
	if len(anthropicRoutes) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"model '"+model+"' has no anthropic route; /v1/messages only passes through anthropic providers")
		return
	}

	var (
		lastErr         error
		lastProvider    string
		upstreamHeaders map[string]string
		clientHeaders   map[string]string
	)
	providerRequest := providers.Request{
		Stream:            stream,
		ClientModel:       model,
		ClientStream:      stream,
		ClientStreamKnown: summary.StreamKnown,
		Session:           middleware.AISessionFrom(r.Context()),
		OnUpstreamHeaders: func(headers map[string]string) {
			upstreamHeaders = headers
		},
	}

	for _, route := range anthropicRoutes {
		providerCfg, err := g.resolver.ProviderConfig(route, g.cfg.RequestTimeoutMs, g.cfg.UpstreamHeaderTimeoutMs)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		key := breakerKey(route)
		if !g.breaker.Allow(key) {
			lastErr = errCircuitOpen
			continue
		}
		provider := providers.NewAnthropic(providerCfg)
		request := providerRequest
		request.Model = route.UpstreamModel
		if len(providerCfg.Headers) > 0 {
			if clientHeaders == nil {
				clientHeaders = clientHeaderMap(r.Header)
			}
			request.ClientHeaders = clientHeaders
		}
		lastProvider = route.Provider.Name

		upstream, err := callWithRetry(r.Context(), g.cfg.UpstreamMaxRetries, g.retryDelay, func() (*http.Response, error) {
			return provider.Passthrough(r.Context(), request, providers.PassthroughOptions{
				Body:          body,
				Header:        r.Header,
				Stream:        stream,
				IdleTimeoutMs: g.cfg.StreamIdleTimeoutMs,
			})
		})
		if err != nil {
			if r.Context().Err() != nil {
				g.breaker.Success(key)
				return
			}
			g.breaker.Failure(key)
			lastErr = err
			continue
		}

		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			errBody, readErr := io.ReadAll(io.LimitReader(upstream.Body, 1<<20))
			_ = upstream.Body.Close()
			if readErr != nil {
				errBody = []byte(readErr.Error())
			}
			if util.IsFailoverStatus(upstream.StatusCode) {
				g.breaker.Failure(key)
				lastErr = &util.UpstreamStatusError{Status: upstream.StatusCode, Body: clipText(string(errBody), 500)}
				continue
			}
			// A non-retryable upstream error is relayed verbatim, so the client
			// sees the provider's own error body.
			g.breaker.Success(key)
			writePassthroughHeaders(w, upstream.Header, requestID)
			w.WriteHeader(upstream.StatusCode)
			_, _ = w.Write(errBody)
			message := clipText(string(errBody), 500)
			g.recordAI(requestID, apiKeyID, model, route.Provider.Name, nil, startedAt, upstream.StatusCode, stream, &message, r.Header, upstreamHeaders, util.ClientIP(r))
			return
		}

		g.breaker.Success(key)
		meta := passthroughMeta{
			requestID:       requestID,
			apiKeyID:        apiKeyID,
			model:           model,
			provider:        route.Provider.Name,
			clientIP:        util.ClientIP(r),
			startedAt:       startedAt,
			requestHeaders:  r.Header,
			upstreamHeaders: upstreamHeaders,
		}
		if stream {
			g.writePassthroughStream(w, upstream, meta)
		} else {
			g.writePassthroughJSON(w, upstream, meta)
		}
		return
	}

	message := "no upstream route available"
	if lastErr != nil {
		message = lastErr.Error()
	}
	writeAnthropicError(w, http.StatusBadGateway, "api_error", message)
	g.recordAI(requestID, apiKeyID, model, lastProvider, nil, startedAt, http.StatusBadGateway, stream, &message, r.Header, upstreamHeaders, util.ClientIP(r))
}

// passthroughMeta carries what the passthrough writers need to record usage.
type passthroughMeta struct {
	requestID       string
	apiKeyID        int64
	model           string
	provider        string
	clientIP        string
	startedAt       time.Time
	requestHeaders  http.Header
	upstreamHeaders map[string]string
}

// writePassthroughJSON relays a non-streaming response and records its usage.
func (g *Gateway) writePassthroughJSON(w http.ResponseWriter, upstream *http.Response, meta passthroughMeta) {
	data, err := io.ReadAll(upstream.Body)
	_ = upstream.Body.Close()
	if err != nil {
		message := err.Error()
		writeAnthropicError(w, http.StatusBadGateway, "api_error", message)
		g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, nil, meta.startedAt, http.StatusBadGateway, false, &message, meta.requestHeaders, meta.upstreamHeaders, meta.clientIP)
		return
	}

	var parsed struct {
		Usage *anthropicUsage `json:"usage"`
	}
	var usage *providers.Usage
	if json.Unmarshal(data, &parsed) == nil && parsed.Usage != nil {
		usage = parsed.Usage.toUsage()
	}

	writePassthroughHeaders(w, upstream.Header, meta.requestID)
	w.WriteHeader(upstream.StatusCode)
	_, _ = w.Write(data)
	g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, usage, meta.startedAt, upstream.StatusCode, false, nil, meta.requestHeaders, meta.upstreamHeaders, meta.clientIP)
}

// writePassthroughStream relays an SSE response chunk by chunk while tallying
// its usage events, and records the usage once the stream ends.
func (g *Gateway) writePassthroughStream(w http.ResponseWriter, upstream *http.Response, meta passthroughMeta) {
	tap := newAnthropicUsageTap(upstream.Body)
	writePassthroughHeaders(w, upstream.Header, meta.requestID)
	w.WriteHeader(upstream.StatusCode)
	_, copyErr := copyAndFlush(w, tap)
	_ = upstream.Body.Close()

	usage := tap.finish().toUsage()
	var errMsg *string
	if copyErr != nil {
		message := copyErr.Error()
		errMsg = &message
	}
	g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, usage, meta.startedAt, upstream.StatusCode, true, errMsg, meta.requestHeaders, meta.upstreamHeaders, meta.clientIP)
}

// anthropicUsage is the usage object of a Messages response or stream event.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// merge keeps the latest non-zero value of each counter. Stream events report
// cumulative counts, so later events replace earlier ones.
func (u *anthropicUsage) merge(next anthropicUsage) {
	if next.InputTokens > 0 {
		u.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		u.OutputTokens = next.OutputTokens
	}
	if next.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	if next.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = next.CacheReadInputTokens
	}
}

// toUsage folds cache reads and writes into the prompt count, since Anthropic
// reports them separately from input_tokens.
func (u anthropicUsage) toUsage() *providers.Usage {
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return &providers.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
	}
}

// anthropicUsageTap reads a streamed Messages response, tallying usage from its
// message_start and message_delta events. It never alters the bytes it returns.
type anthropicUsageTap struct {
	src    io.Reader
	parser *util.SSEParser
	tally  anthropicUsage
}

func newAnthropicUsageTap(src io.Reader) *anthropicUsageTap {
	tap := &anthropicUsageTap{src: src}
	tap.parser = util.NewFilteredSSEParser(tap.onEvent, func(raw []byte) bool {
		return bytes.Contains(raw, anthropicMessageStartMarker) || bytes.Contains(raw, anthropicMessageDeltaMarker)
	})
	return tap
}

func (t *anthropicUsageTap) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.parser.PushBytes(p[:n])
	}
	return n, err
}

// finish processes any trailing event without a blank line and returns the tally.
func (t *anthropicUsageTap) finish() anthropicUsage {
	t.parser.Flush()
	return t.tally
}

func (t *anthropicUsageTap) onEvent(event util.SSEEvent) {
	var parsed struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
		return
	}
	switch {
	case parsed.Type == "message_start" && parsed.Message != nil && parsed.Message.Usage != nil:
		t.tally.merge(*parsed.Message.Usage)
	case parsed.Type == "message_delta" && parsed.Usage != nil:
		t.tally.merge(*parsed.Usage)
	}
}

// writePassthroughHeaders copies the upstream response headers that are safe to
// relay and stamps the gateway request id.
func writePassthroughHeaders(w http.ResponseWriter, header http.Header, requestID string) {
	for key, value := range util.FilterResponseHeaders(header) {
		w.Header().Set(key, value)
	}
	w.Header().Set("X-Request-ID", requestID)
}

// clipText shortens text for logs and error details.
func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// writeAnthropicError renders a gateway error in the Anthropic error shape.
func writeAnthropicError(w http.ResponseWriter, status int, kind, message string) {
	httperr.WriteJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	})
}
