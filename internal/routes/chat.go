package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/providers"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

var chatUsageMarker = []byte(`"usage"`)

// chat handles POST /v1/chat/completions: it resolves the alias, tries each
// route in order (failover on timeout/network/408/429/5xx) and forwards the
// result, streaming or not.
func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	auth := middleware.APIAuthFrom(r.Context())
	requestID := middleware.RequestID(r.Context())
	startedAt := time.Now()

	summary, ok := middleware.RequestSummaryFrom(r.Context())
	if !ok {
		invalidRequest(w, "Invalid JSON body")
		return
	}
	model := summary.Model
	if model == "" {
		invalidRequest(w, "model is required")
		return
	}
	if !summary.HasMessages {
		invalidRequest(w, "messages is required and must be a non-empty array")
		return
	}
	stream := summary.Stream
	temperature := summary.Temperature
	maxTokens := summary.MaxTokens

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
	if len(routes) == 0 {
		httperr.Write(w, http.StatusNotFound, "Model '"+model+"' has no available routes", "not_found_error", "model_not_found")
		return
	}

	apiKeyID := int64(0)
	if auth != nil {
		apiKeyID = auth.Key.ID
	}

	var clientHeaders map[string]string
	var upstreamHeaders map[string]string

	providerRequest := providers.Request{
		Model:             routes[0].UpstreamModel,
		Stream:            stream,
		Temperature:       temperature,
		MaxTokens:         maxTokens,
		RawBytes:          middleware.RequestBody(r.Context()),
		RawResponse:       !stream,
		Session:           middleware.AISessionFrom(r.Context()),
		ClientModel:       model,
		ClientStream:      stream,
		ClientStreamKnown: summary.StreamKnown,
		OnUpstreamHeaders: func(headers map[string]string) {
			upstreamHeaders = headers
		},
	}

	var lastErr error
	for _, route := range routes {
		providerCfg, err := g.resolver.ProviderConfig(route, g.cfg.RequestTimeoutMs)
		if err != nil {
			// A key that cannot be decrypted is a config problem, not an
			// upstream failure: do not fail over.
			httperr.Write(w, http.StatusInternalServerError, err.Error(), "internal_error", "provider_key_undecryptable")
			return
		}
		provider := providers.New(providerCfg)
		request := providerRequest
		request.Model = route.UpstreamModel
		if providerNeedsFullBody(providerCfg.Type) {
			request.RawBody = middleware.ParsedJSON(r.Context())
			if request.RawBody == nil {
				invalidRequest(w, "Invalid JSON body")
				return
			}
		}
		if len(providerCfg.Headers) > 0 {
			if clientHeaders == nil {
				clientHeaders = clientHeaderMap(r.Header)
			}
			request.ClientHeaders = clientHeaders
		}

		if stream {
			// The stream gets its own cancelable context so the idle-timeout
			// timer can abort a stalled upstream read.
			streamCtx, cancelStream := context.WithCancel(r.Context())
			upstream, err := provider.ChatStream(streamCtx, request)
			if err != nil {
				cancelStream()
				if shouldFailover(err) {
					lastErr = err
					continue
				}
				g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, true, r.Header, upstreamHeaders)
				return
			}
			g.writeStream(w, r, upstream, cancelStream, streamMeta{
				requestID:       requestID,
				apiKeyID:        apiKeyID,
				model:           model,
				provider:        route.Provider.Name,
				startedAt:       startedAt,
				requestHeaders:  r.Header,
				upstreamHeaders: upstreamHeaders,
			})
			return
		}

		result, err := provider.Chat(r.Context(), request)
		if err != nil {
			if shouldFailover(err) {
				lastErr = err
				continue
			}
			g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, false, r.Header, upstreamHeaders)
			return
		}
		g.writeCompletion(w, result, requestID)
		g.recordAI(requestID, apiKeyID, model, route.Provider.Name, result.Usage, startedAt, http.StatusOK, false, nil, r.Header, upstreamHeaders)
		return
	}

	g.writeFinalError(w, r, lastErr, apiKeyID, model, startedAt, stream, r.Header, upstreamHeaders)
}

// shouldFailover reports whether an error is worth trying the next route:
// timeouts/network errors and 408/429/5xx, but not 400/401/403/404.
func shouldFailover(err error) bool {
	var statusErr *util.UpstreamStatusError
	if errors.As(err, &statusErr) {
		return util.IsFailoverStatus(statusErr.Status)
	}
	return true
}

func (g *Gateway) writeCompletion(w http.ResponseWriter, result *providers.Result, requestID string) {
	for key, value := range result.Headers {
		w.Header().Set(key, value)
	}
	if len(result.RawBody) > 0 {
		if w.Header().Get("content-type") == "" {
			w.Header().Set("content-type", "application/json; charset=utf-8")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.RawBody)
		return
	}

	usage := result.Usage
	if usage == nil {
		usage = &providers.Usage{}
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{
		"id":      result.ID,
		"object":  "chat.completion",
		"created": result.Created,
		"model":   result.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       result.Message,
			"finish_reason": result.FinishReason,
		}},
		"usage": usage,
	})
}

// writeUpstreamError passes a non-retryable upstream error straight through
// (the client sees the upstream status/message), recording usage.
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, r *http.Request, err error, apiKeyID int64, model, providerName string, startedAt time.Time, stream bool, requestHeaders http.Header, upstreamHeaders map[string]string) {
	status := http.StatusBadGateway
	message := err.Error()
	var statusErr *util.UpstreamStatusError
	if errors.As(err, &statusErr) {
		status = statusErr.Status
		message = statusErr.Body
	}

	if statusErr != nil {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("X-Request-ID", middleware.RequestID(r.Context()))
		w.WriteHeader(statusErr.Status)
		_, _ = io.WriteString(w, statusErr.Body)
	} else {
		httperr.WriteJSON(w, http.StatusBadGateway, httperr.New(message, "upstream_error", "upstream_error"))
	}
	errorCopy := message
	g.recordAI(middleware.RequestID(r.Context()), apiKeyID, model, providerName, nil, startedAt, status, stream, &errorCopy, requestHeaders, upstreamHeaders)
}

// writeFinalError records and reports the failure once every route failed.
func (g *Gateway) writeFinalError(w http.ResponseWriter, r *http.Request, lastErr error, apiKeyID int64, model string, startedAt time.Time, stream bool, requestHeaders http.Header, upstreamHeaders map[string]string) {
	requestID := middleware.RequestID(r.Context())
	status := http.StatusBadGateway
	code := "upstream_error"
	if errors.Is(lastErr, util.ErrUpstreamTimeout) {
		status = http.StatusGatewayTimeout
		code = "upstream_timeout"
	}
	message := "All provider routes failed"
	if lastErr != nil {
		message = lastErr.Error()
	}

	var statusErr *util.UpstreamStatusError
	if errors.As(lastErr, &statusErr) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("X-Request-ID", requestID)
		w.WriteHeader(statusErr.Status)
		_, _ = io.WriteString(w, statusErr.Body)
	} else if status == http.StatusGatewayTimeout {
		httperr.WriteJSON(w, status, httperr.New("Upstream request timed out", "upstream_error", code))
	} else {
		httperr.WriteJSON(w, status, httperr.New(message, "upstream_error", code))
	}
	g.recordAI(requestID, apiKeyID, model, "", nil, startedAt, status, stream, &message, requestHeaders, upstreamHeaders)
}

// streamMeta carries what the stream wrapper needs to record usage.
type streamMeta struct {
	requestID       string
	apiKeyID        int64
	model           string
	provider        string
	startedAt       time.Time
	requestHeaders  http.Header
	upstreamHeaders map[string]string
}

// writeStream forwards the upstream SSE stream chunk by chunk, enforcing an
// idle timeout and accumulating token usage as it goes.
func (g *Gateway) writeStream(w http.ResponseWriter, r *http.Request, upstream *http.Response, cancelUpstream context.CancelFunc, meta streamMeta) {
	defer upstream.Body.Close()
	defer cancelUpstream()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("X-Request-ID", meta.requestID)
	w.WriteHeader(http.StatusOK)

	promptTokens, completionTokens := 0, 0
	parser := util.NewFilteredSSEParser(func(event util.SSEEvent) {
		var parsed struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(event.Data), &parsed); err == nil && parsed.Usage != nil {
			if parsed.Usage.PromptTokens > 0 {
				promptTokens = parsed.Usage.PromptTokens
			}
			if parsed.Usage.CompletionTokens > 0 {
				completionTokens = parsed.Usage.CompletionTokens
			}
		}
	}, func(raw []byte) bool {
		// Most chunks carry no usage object; skip SSE line parsing and the JSON
		// decode unless the key is present at all.
		return bytes.Contains(raw, chatUsageMarker)
	})

	idle := time.Duration(g.cfg.StreamIdleTimeoutMs) * time.Millisecond
	if idle <= 0 {
		idle = 24 * time.Hour
	}
	// A timer cancels the upstream read instead of a reader goroutine feeding a
	// channel: reads happen directly in this loop, so there is no per-chunk
	// allocation or goroutine hand-off on the streaming hot path.
	var idleTimedOut atomic.Bool
	timer := time.AfterFunc(idle, func() {
		idleTimedOut.Store(true)
		cancelUpstream()
	})
	defer timer.Stop()

	status := http.StatusOK
	bufferPtr := util.GetStreamBuffer()
	defer util.PutStreamBuffer(bufferPtr)
	buffer := *bufferPtr
readLoop:
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			timer.Reset(idle)
			// Deliver the chunk before parsing it so usage accounting never
			// sits between the upstream byte and the client.
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			parser.PushBytes(buffer[:n])
		}
		if readErr != nil {
			switch {
			case idleTimedOut.Load():
				// Idle timeout: close the stream the same way the old
				// timer-based loop did (recorded with the default 200).
				parser.Flush()
				break readLoop
			case r.Context().Err() != nil:
				// Client went away: stop without recording a server error.
				return
			default:
				if !errors.Is(readErr, io.EOF) {
					status = http.StatusBadGateway
				}
				parser.Flush()
				break readLoop
			}
		}
	}

	g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, &providers.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}, meta.startedAt, status, true, nil, meta.requestHeaders, meta.upstreamHeaders)
}

// recordAI writes one usage_logs row for an AI request.
func (g *Gateway) recordAI(requestID string, apiKeyID int64, model, providerName string, usage *providers.Usage, startedAt time.Time, status int, stream bool, errMsg *string, requestHeaders http.Header, upstreamHeaders map[string]string) {
	latency := time.Since(startedAt).Milliseconds()
	promptTokens, completionTokens := 0, 0
	if usage != nil {
		promptTokens = usage.PromptTokens
		completionTokens = usage.CompletionTokens
	}
	keyID := apiKeyID
	modelCopy := model
	var providerPtr *string
	if providerName != "" {
		providerCopy := providerName
		providerPtr = &providerCopy
	}
	var headerSnapshots func() (*string, *string)
	if g.cfg.UsageHeaderSnapshots {
		headerSnapshots = func() (*string, *string) {
			return snapshotHTTPHeaders(requestHeaders), snapshotHeaderMap(upstreamHeaders)
		}
	}
	db.RecordUsageAsync(db.UsageRecord{
		RequestID:       requestID,
		APIKeyID:        &keyID,
		Kind:            "ai",
		Model:           &modelCopy,
		Provider:        providerPtr,
		InputTokens:     promptTokens,
		OutputTokens:    completionTokens,
		LatencyMs:       &latency,
		Status:          &status,
		Stream:          stream,
		Error:           errMsg,
		HeaderSnapshots: headerSnapshots,
	})
}

// clientHeaderMap is built only when a provider actually declares custom
// headers; most requests never allocate it.
func clientHeaderMap(header http.Header) map[string]string {
	out := make(map[string]string, len(header))
	for key, values := range header {
		if len(values) > 0 {
			out[strings.ToLower(key)] = values[0]
		}
	}
	return out
}

// providerNeedsFullBody reports whether an adapter reads the decoded request
// object. OpenAI/compatible providers forward the raw bytes and only need the
// summary; Anthropic and Responses translate the message tree and need the map.
func providerNeedsFullBody(kind string) bool {
	switch kind {
	case "anthropic", "openai-responses", "responses":
		return true
	default:
		return false
	}
}
