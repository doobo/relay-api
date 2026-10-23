package routes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/providers"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

// responses implements POST /v1/responses, the OpenAI Responses API surface
// Codex CLI uses by default. The relay's provider adapters already speak chat
// completions to every upstream, so this handler translates a Responses request
// into a chat-completions request, runs it through the same resolver/failover
// path, and translates the chat result (or chat SSE stream) back into Responses
// output items and events.
func (g *Gateway) responses(w http.ResponseWriter, r *http.Request) {
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
	stream, _ := rawBody["stream"].(bool)

	// Conversion to chat is lazy: a route served by a `responses` provider
	// forwards the original body untouched and never pays for the translation.
	var (
		chatBody    map[string]any
		customTools map[string]bool
		converted   bool
	)
	ensureChatBody := func() (map[string]any, map[string]bool, error) {
		if converted {
			return chatBody, customTools, nil
		}
		body, custom, err := responsesToChatRequest(rawBody)
		if err != nil {
			return nil, nil, err
		}
		chatBody, customTools, converted = body, custom, true
		return chatBody, customTools, nil
	}

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
	clientHeaders := map[string]string{}
	for key, values := range r.Header {
		if len(values) > 0 {
			clientHeaders[strings.ToLower(key)] = values[0]
		}
	}

	var requestHeaders *string
	if g.cfg.UsageHeaderSnapshots {
		requestHeaders = snapshotHTTPHeaders(r.Header)
	}
	var upstreamHeaders *string

	baseRequest := providers.Request{
		Stream:        stream,
		ClientHeaders: clientHeaders,
		Session:       middleware.AISessionFrom(r.Context()),
		OnUpstreamHeaders: func(headers map[string]string) {
			if g.cfg.UsageHeaderSnapshots {
				upstreamHeaders = snapshotHeaderMap(headers)
			}
		},
	}

	var lastErr error
	for _, route := range routes {
		providerCfg, err := g.resolver.ProviderConfig(route, g.cfg.RequestTimeoutMs)
		if err != nil {
			httperr.Write(w, http.StatusInternalServerError, err.Error(), "internal_error", "provider_key_undecryptable")
			return
		}
		meta := responsesMeta{
			requestID:       requestID,
			apiKeyID:        apiKeyID,
			model:           model,
			provider:        route.Provider.Name,
			startedAt:       startedAt,
			requestHeaders:  requestHeaders,
			upstreamHeaders: upstreamHeaders,
			stream:          stream,
		}

		// A `responses` provider speaks the Responses protocol natively: forward
		// the client body and return the upstream body/SSE verbatim.
		if route.Provider.Type == "responses" {
			passCtx, cancelPass := context.WithCancel(r.Context())
			request := baseRequest
			request.Model = route.UpstreamModel
			request.RawBody = rawBody
			request.RawBytes = middleware.RequestBody(r.Context())
			upstream, err := providers.ResponsesPassthrough(passCtx, providerCfg, request)
			if err != nil {
				cancelPass()
				if shouldFailover(err) {
					lastErr = err
					continue
				}
				g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, stream, requestHeaders, upstreamHeaders)
				return
			}
			if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
				body := util.ReadBodyLimited(upstream, 500)
				cancelPass()
				statusErr := &util.UpstreamStatusError{Status: upstream.StatusCode, Body: body}
				if util.IsFailoverStatus(upstream.StatusCode) {
					lastErr = statusErr
					continue
				}
				g.writeUpstreamError(w, r, statusErr, apiKeyID, model, route.Provider.Name, startedAt, stream, requestHeaders, upstreamHeaders)
				return
			}
			g.writeResponsesPassthrough(w, r, upstream, cancelPass, meta)
			return
		}

		body, custom, err := ensureChatBody()
		if err != nil {
			invalidRequest(w, err.Error())
			return
		}
		provider := providers.New(providerCfg)
		request := baseRequest
		request.Model = route.UpstreamModel
		request.RawBody = body

		if stream {
			streamCtx, cancelStream := context.WithCancel(r.Context())
			upstream, err := provider.ChatStream(streamCtx, request)
			if err != nil {
				cancelStream()
				if shouldFailover(err) {
					lastErr = err
					continue
				}
				g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, true, requestHeaders, upstreamHeaders)
				return
			}
			g.writeResponsesStream(w, r, upstream, cancelStream, rawBody, custom, meta)
			return
		}

		result, err := provider.Chat(r.Context(), request)
		if err != nil {
			if shouldFailover(err) {
				lastErr = err
				continue
			}
			g.writeUpstreamError(w, r, err, apiKeyID, model, route.Provider.Name, startedAt, false, requestHeaders, upstreamHeaders)
			return
		}

		output, usage := buildResponsesOutput(result, custom)
		responseID := "resp_" + randomResponsesID()
		payload := buildResponsesResponse(responseID, model, "completed", output, usage, rawBody, time.Now().Unix())
		payload["end_turn"] = len(result.Message.ToolCalls) == 0

		var responseHeaders map[string]string
		if g.cfg.UsageHeaderSnapshots {
			responseHeaders = result.Headers
		}
		for key, value := range responseHeaders {
			w.Header().Set(key, value)
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(payload)

		recordUsage := &providers.Usage{
			PromptTokens:     usage.input,
			CompletionTokens: usage.output,
			TotalTokens:      usage.total,
		}
		g.recordAI(requestID, apiKeyID, model, route.Provider.Name, recordUsage, startedAt, http.StatusOK, false, nil, requestHeaders, upstreamHeaders)
		return
	}

	g.writeFinalError(w, r, lastErr, apiKeyID, model, startedAt, stream, requestHeaders, upstreamHeaders)
}

// ------------------------------------------------------------------ request

// responsesToChatRequest converts a Responses API request into the chat
// completions body the provider adapters already know how to forward.
func responsesToChatRequest(raw map[string]any) (map[string]any, map[string]bool, error) {
	body := map[string]any{}
	customTools := map[string]bool{}

	if tools, ok := raw["tools"].([]any); ok {
		chatTools := make([]any, 0, len(tools))
		for _, entry := range tools {
			tool, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := tool["type"].(string)
			switch kind {
			case "function":
				chatTools = append(chatTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        tool["name"],
						"description": tool["description"],
						"parameters":  tool["parameters"],
						"strict":      tool["strict"],
					},
				})
			case "custom":
				name, _ := tool["name"].(string)
				if name == "" {
					continue
				}
				customTools[name] = true
				chatTools = append(chatTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": tool["description"],
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"input": map[string]any{"type": "string", "description": "Raw tool input"},
							},
							"required":             []any{"input"},
							"additionalProperties": false,
						},
					},
				})
			default:
				// Built-in Responses tools (web_search, computer_use, ...) have
				// no Chat Completions equivalent and are dropped.
			}
		}
		if len(chatTools) > 0 {
			body["tools"] = chatTools
		}
	}
	if choice, ok := raw["tool_choice"]; ok {
		body["tool_choice"] = convertResponsesToolChoice(choice)
	}
	if parallel, ok := raw["parallel_tool_calls"]; ok {
		body["parallel_tool_calls"] = parallel
	}

	messages := responsesInputToChatMessages(raw["input"], customTools)
	if instructions, _ := raw["instructions"].(string); strings.TrimSpace(instructions) != "" {
		messages = append([]any{map[string]any{"role": "system", "content": instructions}}, messages...)
	}
	if len(messages) == 0 {
		return nil, nil, errors.New("input is required and must contain at least one message")
	}
	body["messages"] = messages

	for _, key := range []string{"temperature", "top_p", "user", "metadata", "service_tier"} {
		if value, ok := raw[key]; ok {
			body[key] = value
		}
	}
	if effort := responsesReasoningEffort(raw); effort != "" {
		body["reasoning_effort"] = effort
	}
	if max, ok := raw["max_output_tokens"]; ok {
		body["max_tokens"] = max
	}
	if text, ok := raw["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if converted := chatResponseFormat(format); converted != nil {
				body["response_format"] = converted
			}
		}
	}
	if stream, _ := raw["stream"].(bool); stream {
		// Ask chat upstreams for the final usage chunk so Responses clients get
		// real token accounting instead of zeros.
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body, customTools, nil
}

// responsesInputToChatMessages maps Responses input items onto chat messages,
// including function/custom tool calls and their outputs.
func responsesInputToChatMessages(input any, customTools map[string]bool) []any {
	if text, ok := input.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}
	}
	items, ok := input.([]any)
	if !ok {
		if item, ok := input.(map[string]any); ok {
			items = []any{item}
		}
	}

	messages := make([]any, 0, len(items))
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := item["type"].(string)
		switch kind {
		case "", "message":
			role, _ := item["role"].(string)
			if role == "" {
				continue
			}
			if role == "developer" {
				role = "system"
			}
			content := responsesContentToChat(item["content"])
			if content == nil {
				continue
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "function_call", "custom_tool_call":
			name, _ := item["name"].(string)
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID = "call_" + randomResponsesID()
			}
			arguments := ""
			if kind == "custom_tool_call" {
				rawInput, _ := item["input"].(string)
				encoded, _ := json.Marshal(map[string]any{"input": rawInput})
				arguments = string(encoded)
			} else {
				arguments, _ = item["arguments"].(string)
			}
			if arguments == "" {
				arguments = "{}"
			}
			appendChatToolCall(&messages, callID, name, arguments)
		case "function_call_output", "custom_tool_call_output":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				continue
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      responsesOutputToText(item["output"]),
			})
		case "reasoning", "local_shell_call", "web_search_call", "image_generation_call",
			"additional_tools", "compaction", "compaction_summary", "context_compaction":
			// No chat-completions equivalent; drop it rather than fail the turn.
			continue
		default:
			continue
		}
	}
	return messages
}

// responsesContentToChat turns Responses content parts into a chat content
// string or (when images are present) a chat content-part array.
func responsesContentToChat(content any) any {
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := content.([]any)
	if !ok {
		return nil
	}

	var text strings.Builder
	chatParts := make([]any, 0, len(parts))
	for _, entry := range parts {
		part, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		switch kind, _ := part["type"].(string); kind {
		case "input_text", "output_text", "text":
			if value, ok := part["text"].(string); ok {
				text.WriteString(value)
			}
		case "input_image":
			url, _ := part["image_url"].(string)
			if url == "" {
				// file_id images cannot be resolved by the relay.
				continue
			}
			image := map[string]any{"url": url}
			if detail, ok := part["detail"]; ok {
				image["detail"] = detail
			}
			chatParts = append(chatParts, map[string]any{
				"type":      "image_url",
				"image_url": image,
			})
		}
	}

	if len(chatParts) == 0 {
		return text.String()
	}
	if text.Len() > 0 {
		chatParts = append([]any{map[string]any{"type": "text", "text": text.String()}}, chatParts...)
	}
	return chatParts
}

// appendChatToolCall attaches one call to the trailing assistant message, or
// starts a new assistant message when the previous item is not assistant text.
func appendChatToolCall(messages *[]any, callID, name, arguments string) {
	call := map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
	if len(*messages) > 0 {
		if last, ok := (*messages)[len(*messages)-1].(map[string]any); ok && last["role"] == "assistant" {
			calls, _ := last["tool_calls"].([]any)
			last["tool_calls"] = append(calls, call)
			return
		}
	}
	*messages = append(*messages, map[string]any{
		"role":       "assistant",
		"content":    nil,
		"tool_calls": []any{call},
	})
}

// responsesOutputToText flattens a function_call_output payload to text.
func responsesOutputToText(output any) string {
	switch typed := output.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, entry := range typed {
			part, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := part["type"].(string); kind == "input_text" || kind == "output_text" || kind == "text" {
				if text, ok := part["text"].(string); ok {
					if builder.Len() > 0 {
						builder.WriteString("\n")
					}
					builder.WriteString(text)
				}
			}
		}
		if builder.Len() > 0 {
			return builder.String()
		}
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func convertResponsesToolChoice(choice any) any {
	switch typed := choice.(type) {
	case string:
		return typed
	case map[string]any:
		kind, _ := typed["type"].(string)
		name, _ := typed["name"].(string)
		if (kind == "function" || kind == "custom") && name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return "auto"
}

func responsesReasoningEffort(raw map[string]any) string {
	reasoning, ok := raw["reasoning"].(map[string]any)
	if !ok {
		return ""
	}
	if effort, ok := reasoning["effort"].(string); ok {
		return effort
	}
	return ""
}

func chatResponseFormat(format map[string]any) any {
	kind, _ := format["type"].(string)
	switch kind {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		schema := map[string]any{
			"name":   format["name"],
			"schema": format["schema"],
			"strict": format["strict"],
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}
	default:
		return nil
	}
}

// ----------------------------------------------------------------- response

type responsesUsage struct {
	input  int
	output int
	total  int
}

type responsesMeta struct {
	requestID       string
	apiKeyID        int64
	model           string
	provider        string
	startedAt       time.Time
	requestHeaders  *string
	upstreamHeaders *string
	stream          bool
}

func buildResponsesOutput(result *providers.Result, customTools map[string]bool) ([]any, responsesUsage) {
	output := make([]any, 0, len(result.Message.ToolCalls)+2)
	if result.Message.ReasoningContent != nil && *result.Message.ReasoningContent != "" {
		output = append(output, map[string]any{
			"type": "reasoning",
			"id":   "rs_" + randomResponsesID(),
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": *result.Message.ReasoningContent,
			}},
		})
	}
	if result.Message.Content != nil && *result.Message.Content != "" {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      "msg_" + randomResponsesID(),
			"role":    "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": *result.Message.Content}},
		})
	}
	for _, call := range result.Message.ToolCalls {
		output = append(output, responseToolCallItem(call, customTools))
	}

	usage := responsesUsage{}
	if result.Usage != nil {
		usage.input = result.Usage.PromptTokens
		usage.output = result.Usage.CompletionTokens
		usage.total = result.Usage.TotalTokens
		if usage.total == 0 {
			usage.total = usage.input + usage.output
		}
	}
	return output, usage
}

func responseToolCallItem(call providers.ToolCall, customTools map[string]bool) map[string]any {
	callID := call.ID
	if callID == "" {
		callID = "call_" + randomResponsesID()
	}
	itemID := "fc_" + randomResponsesID()
	if customTools[call.Function.Name] {
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      itemID,
			"call_id": callID,
			"name":    call.Function.Name,
			"input":   customToolInput(call.Function.Arguments),
			"status":  "completed",
		}
	}
	return map[string]any{
		"type":      "function_call",
		"id":        itemID,
		"call_id":   callID,
		"name":      call.Function.Name,
		"arguments": call.Function.Arguments,
		"status":    "completed",
	}
}

func customToolInput(arguments string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) == nil {
		if input, ok := parsed["input"].(string); ok {
			return input
		}
		if input, ok := parsed["input"]; ok {
			encoded, _ := json.Marshal(input)
			return string(encoded)
		}
	}
	return arguments
}

func buildResponsesResponse(id, model, status string, output []any, usage responsesUsage, raw map[string]any, createdAt int64) map[string]any {
	response := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": createdAt,
		"status":     status,
		"model":      model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":          usage.input,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens":         usage.output,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          usage.total,
		},
		"parallel_tool_calls": raw["parallel_tool_calls"],
		"tool_choice":         raw["tool_choice"],
		"tools":               raw["tools"],
		"error":               nil,
		"incomplete_details":  nil,
	}
	return response
}

// ------------------------------------------------------------------ streaming

type responsesToolAccumulator struct {
	id        string
	name      string
	arguments strings.Builder
}

func (g *Gateway) writeResponsesStream(
	w http.ResponseWriter,
	r *http.Request,
	upstream *http.Response,
	cancelUpstream context.CancelFunc,
	raw map[string]any,
	customTools map[string]bool,
	meta responsesMeta,
) {
	defer upstream.Body.Close()
	defer cancelUpstream()

	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("X-Request-ID", meta.requestID)
	w.WriteHeader(http.StatusOK)

	responseID := "resp_" + randomResponsesID()
	createdAt := time.Now().Unix()

	emit := func(event string, payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	created := buildResponsesResponse(responseID, meta.model, "in_progress", []any{}, responsesUsage{}, raw, createdAt)
	created["end_turn"] = nil
	emit("response.created", map[string]any{"type": "response.created", "response": created})
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": created})

	var (
		text             strings.Builder
		reasoningText    strings.Builder
		messageItemID    = "msg_" + randomResponsesID()
		reasoningItemID  = "rs_" + randomResponsesID()
		contentStarted   bool
		reasoningStarted bool
		toolCalls        = map[int]*responsesToolAccumulator{}
		toolOrder        []int
		promptTokens     int
		completionTokens int
		totalTokens      int
	)

	startContent := func() {
		if contentStarted {
			return
		}
		contentStarted = true
		emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 1,
			"item": map[string]any{
				"type":    "message",
				"id":      messageItemID,
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
		})
		emit("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       messageItemID,
			"output_index":  1,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		})
	}

	startReasoning := func() {
		if reasoningStarted {
			return
		}
		reasoningStarted = true
		emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]any{
				"type":    "reasoning",
				"id":      reasoningItemID,
				"summary": []any{},
			},
		})
		emit("response.reasoning_summary_part.added", map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       reasoningItemID,
			"output_index":  0,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}

	var doneFlag atomic.Bool
	parser := util.NewSSEParser(func(event util.SSEEvent) {
		data := strings.TrimSpace(event.Data)
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				doneFlag.Store(true)
			}
			return
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent *string `json:"reasoning_content"`
					Reasoning        *string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				promptTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				completionTokens = chunk.Usage.CompletionTokens
			}
			if chunk.Usage.TotalTokens > 0 {
				totalTokens = chunk.Usage.TotalTokens
			}
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			reasoning := ""
			if delta.ReasoningContent != nil {
				reasoning = *delta.ReasoningContent
			} else if delta.Reasoning != nil {
				reasoning = *delta.Reasoning
			}
			if reasoning != "" {
				startReasoning()
				reasoningText.WriteString(reasoning)
				emit("response.reasoning_summary_text.delta", map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"item_id":       reasoningItemID,
					"output_index":  0,
					"summary_index": 0,
					"delta":         reasoning,
				})
			}
			if delta.Content != nil && *delta.Content != "" {
				startContent()
				text.WriteString(*delta.Content)
				emit("response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  1,
					"content_index": 0,
					"delta":         *delta.Content,
				})
			}
			for _, call := range delta.ToolCalls {
				accumulator := toolCalls[call.Index]
				if accumulator == nil {
					accumulator = &responsesToolAccumulator{}
					toolCalls[call.Index] = accumulator
					toolOrder = append(toolOrder, call.Index)
				}
				if call.ID != "" {
					accumulator.id = call.ID
				}
				if call.Function.Name != "" {
					accumulator.name = call.Function.Name
				}
				accumulator.arguments.WriteString(call.Function.Arguments)
			}
		}
	})

	idle := time.Duration(g.cfg.StreamIdleTimeoutMs) * time.Millisecond
	if idle <= 0 {
		idle = 24 * time.Hour
	}
	var idleTimedOut atomic.Bool
	timer := time.AfterFunc(idle, func() {
		idleTimedOut.Store(true)
		cancelUpstream()
	})
	defer timer.Stop()

	status := http.StatusOK
	failureMessage := ""
	buffer := make([]byte, 32*1024)
readLoop:
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			timer.Reset(idle)
			parser.Push(string(buffer[:n]))
			if doneFlag.Load() {
				break readLoop
			}
		}
		if readErr != nil {
			switch {
			case idleTimedOut.Load():
				status = http.StatusGatewayTimeout
				failureMessage = "upstream stream idle timeout"
				break readLoop
			case r.Context().Err() != nil:
				return
			case errors.Is(readErr, io.EOF):
				parser.Flush()
				break readLoop
			default:
				status = http.StatusBadGateway
				failureMessage = readErr.Error()
				break readLoop
			}
		}
	}
	if doneFlag.Load() {
		parser.Flush()
	}

	if status != http.StatusOK {
		failed := buildResponsesResponse(responseID, meta.model, "failed", []any{}, responsesUsage{}, raw, createdAt)
		failed["error"] = map[string]any{"code": "upstream_error", "message": failureMessage}
		emit("response.failed", map[string]any{"type": "response.failed", "response": failed})
		g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, &providers.Usage{}, meta.startedAt, status, true, &failureMessage, meta.requestHeaders, meta.upstreamHeaders)
		return
	}
	output := make([]any, 0, 3+len(toolOrder))
	if reasoningStarted {
		emit("response.reasoning_summary_text.done", map[string]any{
			"type":          "response.reasoning_summary_text.done",
			"item_id":       reasoningItemID,
			"output_index":  0,
			"summary_index": 0,
			"text":          reasoningText.String(),
		})
		reasoningItem := map[string]any{
			"type": "reasoning",
			"id":   reasoningItemID,
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": reasoningText.String(),
			}},
		}
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": reasoningItem,
		})
		output = append(output, reasoningItem)
	}
	if contentStarted {
		emit("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       messageItemID,
			"output_index":  1,
			"content_index": 0,
			"text":          text.String(),
		})
		messageItem := map[string]any{
			"type":    "message",
			"id":      messageItemID,
			"role":    "assistant",
			"status":  "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text.String()}},
		}
		emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": messageItemID,
			"output_index": 1, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text.String()},
		})
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 1, "item": messageItem,
		})
		output = append(output, messageItem)
	}

	sort.Ints(toolOrder)
	for index, key := range toolOrder {
		accumulator := toolCalls[key]
		if accumulator == nil || accumulator.name == "" {
			continue
		}
		callID := accumulator.id
		if callID == "" {
			callID = "call_" + randomResponsesID()
		}
		item := responseToolCallItem(providers.ToolCall{
			ID:   callID,
			Type: "function",
			Function: providers.ToolFunction{
				Name:      accumulator.name,
				Arguments: accumulator.arguments.String(),
			},
		}, customTools)
		emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 2 + index, "item": item,
		})
		output = append(output, item)
	}

	usage := responsesUsage{input: promptTokens, output: completionTokens, total: totalTokens}
	if usage.total == 0 {
		usage.total = usage.input + usage.output
	}
	completed := buildResponsesResponse(responseID, meta.model, "completed", output, usage, raw, createdAt)
	completed["end_turn"] = len(toolOrder) == 0
	emit("response.completed", map[string]any{"type": "response.completed", "response": completed})

	g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, &providers.Usage{
		PromptTokens:     usage.input,
		CompletionTokens: usage.output,
		TotalTokens:      usage.total,
	}, meta.startedAt, http.StatusOK, true, nil, meta.requestHeaders, meta.upstreamHeaders)
}

// responsesUsagePayload mirrors the usage object both in a non-streaming
// Responses body and inside response.completed.
type responsesUsagePayload struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// writeResponsesPassthrough streams an upstream Responses body back unchanged.
// It only parses usage for accounting; request/response bodies and SSE events
// are never rewritten.
func (g *Gateway) writeResponsesPassthrough(w http.ResponseWriter, r *http.Request, upstream *http.Response, cancelUpstream context.CancelFunc, meta responsesMeta) {
	defer upstream.Body.Close()
	defer cancelUpstream()

	writeUpstreamHeaders(w, upstream.Header, meta.requestID)
	// FilterResponseHeaders normally drops content-encoding because the chat
	// path re-encodes bodies. Here the bytes are forwarded verbatim, so keep an
	// encoding the transport did not already decode.
	if encoding := upstream.Header.Get("content-encoding"); encoding != "" {
		w.Header().Set("content-encoding", encoding)
	}
	w.WriteHeader(upstream.StatusCode)

	promptTokens, completionTokens, totalTokens := 0, 0, 0
	record := func(status int, errMsg *string) {
		usage := &providers.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		g.recordAI(meta.requestID, meta.apiKeyID, meta.model, meta.provider, usage, meta.startedAt, status, meta.stream, errMsg, meta.requestHeaders, meta.upstreamHeaders)
	}

	if !meta.stream {
		body, err := io.ReadAll(upstream.Body)
		if err != nil {
			message := err.Error()
			record(http.StatusBadGateway, &message)
			return
		}
		_, _ = w.Write(body)
		var payload struct {
			Usage *responsesUsagePayload `json:"usage"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Usage != nil {
			promptTokens = payload.Usage.InputTokens
			completionTokens = payload.Usage.OutputTokens
			totalTokens = payload.Usage.TotalTokens
		}
		record(upstream.StatusCode, nil)
		return
	}

	flusher, _ := w.(http.Flusher)
	streamDone := false
	streamFailed := false
	parser := util.NewSSEParser(func(event util.SSEEvent) {
		if event.Data == "[DONE]" {
			streamDone = true
			return
		}
		// A Responses stream terminates with one of these events. Stop reading
		// even if the upstream keeps the SSE connection open.
		if strings.Contains(event.Data, `"type":"response.completed"`) ||
			strings.Contains(event.Data, `"type": "response.completed"`) {
			streamDone = true
		}
		if strings.Contains(event.Data, `"type":"response.failed"`) ||
			strings.Contains(event.Data, `"type": "response.failed"`) {
			streamDone = true
			streamFailed = true
		}
		if !strings.Contains(event.Data, `"usage"`) {
			return
		}
		var payload struct {
			Usage    *responsesUsagePayload `json:"usage"`
			Response *struct {
				Usage *responsesUsagePayload `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(event.Data), &payload) != nil {
			return
		}
		usage := payload.Usage
		if payload.Response != nil && payload.Response.Usage != nil {
			usage = payload.Response.Usage
		}
		if usage == nil {
			return
		}
		promptTokens = usage.InputTokens
		completionTokens = usage.OutputTokens
		totalTokens = usage.TotalTokens
	})

	idle := time.Duration(g.cfg.StreamIdleTimeoutMs) * time.Millisecond
	if idle <= 0 {
		idle = 24 * time.Hour
	}
	var idleTimedOut atomic.Bool
	timer := time.AfterFunc(idle, func() {
		idleTimedOut.Store(true)
		cancelUpstream()
	})
	defer timer.Stop()

	status := upstream.StatusCode
	var failure *string
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := upstream.Body.Read(buffer)
		if n > 0 {
			timer.Reset(idle)
			if _, err := w.Write(buffer[:n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			parser.Push(string(buffer[:n]))
			if streamDone {
				if streamFailed {
					status = http.StatusBadGateway
					message := "upstream response failed"
					failure = &message
				}
				break
			}
		}
		if readErr == nil {
			continue
		}
		switch {
		case idleTimedOut.Load():
			status = http.StatusGatewayTimeout
			message := "upstream stream idle timeout"
			failure = &message
			parser.Flush()
		case r.Context().Err() != nil:
			// Client went away; nothing left to record for.
			return
		case errors.Is(readErr, io.EOF):
			parser.Flush()
		default:
			status = http.StatusBadGateway
			message := readErr.Error()
			failure = &message
			parser.Flush()
		}
		break
	}
	record(status, failure)
}

// randomResponsesID returns a short hex id for response/output items.
func randomResponsesID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
