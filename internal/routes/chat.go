package routes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/providers"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

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

	rawBody := middleware.ParsedJSON(r.Context())
	if rawBody == nil {
		invalidRequest(w, "Invalid JSON body")
		return
	}
	model, _ := rawBody["model"].(string)
	if model == "" {
		invalidRequest(w, "model is required")
		return
	}
	messages, _ := rawBody["messages"].([]any)
	if len(messages) == 0 {
		invalidRequest(w, "messages is required and must be a non-empty array")
		return
	}
	stream, _ := rawBody["stream"].(bool)
	temperature := float64Value(rawBody["temperature"])
	maxTokens := intValue(rawBody["max_tokens"])

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

	providerRequest := providers.Request{
		Model:       routes[0].UpstreamModel,
		Stream:      stream,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		RawBody:     rawBody,
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

		if stream {
			upstream, err := provider.ChatStream(r.Context(), request)
			if err != nil {
				if shouldFailover(err) {
					lastErr = err
					continue
				}
				g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, true)
				return
			}
			g.writeStream(w, r, upstream, streamMeta{
				requestID: requestID,
				apiKeyID:  apiKeyID,
				model:     model,
				provider:  route.Provider.Name,
				startedAt: startedAt,
			})
			return
		}

		result, err := provider.Chat(r.Context(), request)
		if err != nil {
			if shouldFailover(err) {
				lastErr = err
				continue
			}
			g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, false)
			return
		}
		g.writeCompletion(w, result, requestID)
		g.recordAI(requestID, apiKeyID, model, route.Provider.Name, result.Usage, startedAt, http.StatusOK, false, nil)
		return
	}

	g.writeFinalError(w, r, lastErr, apiKeyID, model, startedAt, stream)
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
	usage := result.Usage
	if usage == nil {
		usage = &providers.Usage{}
	}
	for key, value := range result.Headers {
		w.Header().Set(key, value)
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
func (g *Gateway) writeUpstreamError(w http.ResponseWriter, r *http.Request, err error, apiKeyID int64, model, providerName string, startedAt time.Time, stream bool) {
	status := http.StatusBadGateway
	message := err.Error()
	var statusErr *util.UpstreamStatusError
	if errors.As(err, &statusErr) {
		status = statusErr.Status
		message = statusErr.Body
	}
	errorCopy := message
	g.recordAI(middleware.RequestID(r.Context()), apiKeyID, model, providerName, nil, startedAt, status, stream, &errorCopy)

	if statusErr != nil {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("X-Request-ID", middleware.RequestID(r.Context()))
		w.WriteHeader(statusErr.Status)
		_, _ = io.WriteString(w, statusErr.Body)
		return
	}
	httperr.WriteJSON(w, http.StatusBadGateway, httperr.New(message, "upstream_error", "upstream_error"))
}

// writeFinalError records and reports the failure once every route failed.
func (g *Gateway) writeFinalError(w http.ResponseWriter, r *http.Request, lastErr error, apiKeyID int64, model string, startedAt time.Time, stream bool) {
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
	g.recordAI(requestID, apiKeyID, model, "", nil, startedAt, status, stream, &message)

	var statusErr *util.UpstreamStatusError
	if errors.As(lastErr, &statusErr) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("X-Request-ID", requestID)
		w.WriteHeader(statusErr.Status)
		_, _ = io.WriteString(w, statusErr.Body)
		return
	}
	if status == http.StatusGatewayTimeout {
		httperr.WriteJSON(w, status, httperr.New("Upstream request timed out", "upstream_error", code))
		return
	}
	httperr.WriteJSON(w, status, httperr.New(message, "upstream_error", code))
}

// streamMeta carries what the stream wrapper needs to record usage.
type streamMeta struct {
	requestID string
	apiKeyID  int64
	model     string
	provider  string
	startedAt time.Time
}

// writeStream forwards the upstream SSE stream chunk by chunk, enforcing an
// idle timeout and accumulating token usage as it goes.
func (g *Gateway) writeStream(w http.ResponseWriter, r *http.Request, upstream *http.Response, meta streamMeta) {
	defer upstream.Body.Close()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("X-Request-ID", meta.requestID)
	w.WriteHeader(http.StatusOK)

	promptTokens, completionTokens := 0, 0
	parser := util.NewSSEParser(func(event util.SSEEvent) {
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
	})

	type readChunk struct {
		data []byte
		err  error
	}
	chunks := make(chan readChunk)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 32*1024)
		for {
			n, err := upstream.Body.Read(buffer)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buffer[:n])
				select {
				case chunks <- readChunk{data: chunk}:
				case <-r.Context().Done():
					return
				}
			}
			if err != nil {
				select {
				case chunks <- readChunk{err: err}:
				case <-r.Context().Done():
				}
				return
			}
		}
	}()

	idle := time.Duration(g.cfg.StreamIdleTimeoutMs) * time.Millisecond
	if idle <= 0 {
		idle = 24 * time.Hour
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()

	status := http.StatusOK
streamLoop:
	for {
		select {
		case <-r.Context().Done():
			// Client went away: stop without recording a server error.
			return
		case <-timer.C:
			break streamLoop // idle timeout closes the stream
		case chunk, ok := <-chunks:
			if !ok {
				parser.Flush()
				break streamLoop
			}
			if len(chunk.data) > 0 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
				parser.Push(string(chunk.data))
				if _, err := w.Write(chunk.data); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.err != nil {
				if !errors.Is(chunk.err, io.EOF) {
					status = http.StatusBadGateway
				}
				parser.Flush()
				break streamLoop
			}
		}
	}

	g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, &providers.Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}, meta.startedAt, status, true, nil)
}

// recordAI writes one usage_logs row for an AI request.
func (g *Gateway) recordAI(requestID string, apiKeyID int64, model, providerName string, usage *providers.Usage, startedAt time.Time, status int, stream bool, errMsg *string) {
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
	_ = db.RecordUsage(db.UsageRecord{
		RequestID:    requestID,
		APIKeyID:     &keyID,
		Kind:         "ai",
		Model:        &modelCopy,
		Provider:     providerPtr,
		InputTokens:  promptTokens,
		OutputTokens: completionTokens,
		LatencyMs:    &latency,
		Status:       &status,
		Stream:       stream,
		Error:        errMsg,
	})
}

func float64Value(value any) *float64 {
	if number, ok := value.(float64); ok {
		return &number
	}
	return nil
}

func intValue(value any) *int {
	if number, ok := value.(float64); ok {
		converted := int(number)
		return &converted
	}
	return nil
}
