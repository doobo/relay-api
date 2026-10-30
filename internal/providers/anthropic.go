package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"relay-api/internal/util"
)

// Anthropic converts OpenAI-format requests to the Anthropic Messages API and
// converts responses (including SSE streams) back to OpenAI format, so clients
// never know the upstream is Anthropic. Tools travel both ways: OpenAI function
// tools become Anthropic tools, and tool_use blocks become tool_calls.
type Anthropic struct {
	OpenAI
}

func (p *Anthropic) headers(request Request) map[string]string {
	headers := map[string]string{
		"content-type":      "application/json",
		"anthropic-version": "2023-06-01",
	}
	if p.cfg.APIKey != nil && *p.cfg.APIKey != "" {
		headers["x-api-key"] = *p.cfg.APIKey
	}
	return applyCustomHeaders(headers, p.cfg, request)
}

// anthropicMessage is one turn of the Messages API. Content is always a list
// of content blocks, which lets tool_use and tool_result sit beside text.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// convertMessages maps OpenAI messages onto Anthropic's system + messages.
// Tool results become tool_result blocks on a user turn, and assistant tool
// calls become tool_use blocks. Adjacent turns with the same role are merged,
// because Anthropic requires alternating roles.
func convertMessages(rawBody map[string]any) (string, []anthropicMessage) {
	system := ""
	out := []anthropicMessage{}

	messages, _ := rawBody["messages"].([]any)
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			text := extractContentText(message["content"])
			if system != "" {
				system += "\n" + text
			} else {
				system = text
			}
		case "user":
			appendAnthropicBlocks(&out, "user", textBlocks(message["content"]))
		case "assistant":
			appendAnthropicBlocks(&out, "assistant", assistantBlocks(message))
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			appendAnthropicBlocks(&out, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": callID,
				"content":     extractContentText(message["content"]),
			}})
		}
	}
	return system, out
}

// appendAnthropicBlocks adds blocks as a turn, merging into the previous turn
// when it has the same role.
func appendAnthropicBlocks(out *[]anthropicMessage, role string, blocks []any) {
	if len(blocks) == 0 {
		return
	}
	if last := len(*out) - 1; last >= 0 && (*out)[last].Role == role {
		existing, _ := (*out)[last].Content.([]any)
		(*out)[last].Content = append(existing, blocks...)
		return
	}
	*out = append(*out, anthropicMessage{Role: role, Content: blocks})
}

// textBlocks turns a string or a text-part array into text blocks, skipping
// empty text (Anthropic rejects empty text blocks).
func textBlocks(content any) []any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": value}}
	case []any:
		var blocks []any
		for _, part := range value {
			object, ok := part.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := object["type"].(string)
			text, _ := object["text"].(string)
			if kind == "text" && text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
		}
		return blocks
	default:
		return nil
	}
}

// assistantBlocks converts an assistant turn: its text, then one tool_use block
// per tool call.
func assistantBlocks(message map[string]any) []any {
	blocks := textBlocks(message["content"])
	calls, _ := message["tool_calls"].([]any)
	for _, entry := range calls {
		call, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		id, _ := call["id"].(string)
		arguments, _ := fn["arguments"].(string)
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": toolInput(arguments),
		})
	}
	return blocks
}

// toolInput decodes a tool call's JSON arguments. Anthropic requires an object,
// so anything else becomes an empty object.
func toolInput(arguments string) map[string]any {
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil || input == nil {
		return map[string]any{}
	}
	return input
}

// convertAnthropicTools maps OpenAI function tools onto Anthropic tools. Other
// tool kinds (built-in tools the upstream would not understand) are dropped.
func convertAnthropicTools(raw any) []any {
	tools, _ := raw.([]any)
	out := make([]any, 0, len(tools))
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := tool["type"].(string); kind != "" && kind != "function" {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		item := map[string]any{"name": name, "input_schema": schema}
		if description, _ := fn["description"].(string); description != "" {
			item["description"] = description
		}
		out = append(out, item)
	}
	return out
}

// convertAnthropicToolChoice maps an OpenAI tool_choice onto Anthropic's. It
// returns nil when the choice should be left to the upstream default.
func convertAnthropicToolChoice(choice any) map[string]any {
	switch value := choice.(type) {
	case string:
		switch value {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		fn, _ := value["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
	}
	return nil
}

// anthropicStopSequences maps OpenAI stop (string or list) onto stop_sequences.
func anthropicStopSequences(raw any) []string {
	switch value := raw.(type) {
	case string:
		if value != "" {
			return []string{value}
		}
	case []any:
		var out []string
		for _, entry := range value {
			if text, ok := entry.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// extractContentText flattens a string or a content-part array into text.
func extractContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, part := range value {
			if object, ok := part.(map[string]any); ok {
				if text, ok := object["text"].(string); ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// parseAnthropicContent splits response content blocks into text and tool calls.
func parseAnthropicContent(content any) (string, []ToolCall) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var text strings.Builder
		var calls []ToolCall
		for _, block := range value {
			object, ok := block.(map[string]any)
			if !ok {
				continue
			}
			switch kind, _ := object["type"].(string); kind {
			case "text":
				if part, ok := object["text"].(string); ok {
					text.WriteString(part)
				}
			case "tool_use":
				id, _ := object["id"].(string)
				name, _ := object["name"].(string)
				arguments := "{}"
				if input := object["input"]; input != nil {
					if encoded, err := json.Marshal(input); err == nil {
						arguments = string(encoded)
					}
				}
				calls = append(calls, ToolCall{
					ID:       id,
					Type:     "function",
					Function: ToolFunction{Name: name, Arguments: arguments},
				})
			}
		}
		return text.String(), calls
	default:
		return "", nil
	}
}

func (p *Anthropic) toAnthropicBody(request Request, stream bool) map[string]any {
	system, messages := convertMessages(request.RawBody)
	body := map[string]any{
		"model":    request.Model,
		"messages": messages,
		"stream":   stream,
	}
	if request.MaxTokens != nil {
		body["max_tokens"] = *request.MaxTokens
	} else {
		body["max_tokens"] = 4096
	}
	if system != "" {
		body["system"] = system
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if tools := convertAnthropicTools(request.RawBody["tools"]); len(tools) > 0 {
		body["tools"] = tools
		choice := convertAnthropicToolChoice(request.RawBody["tool_choice"])
		if parallel, ok := request.RawBody["parallel_tool_calls"].(bool); ok && !parallel {
			if choice == nil {
				choice = map[string]any{"type": "auto"}
			}
			if choice["type"] != "none" {
				choice["disable_parallel_tool_use"] = true
			}
		}
		if choice != nil {
			body["tool_choice"] = choice
		}
	}
	if stops := anthropicStopSequences(request.RawBody["stop"]); len(stops) > 0 {
		body["stop_sequences"] = stops
	}
	return body
}

// anthropicFinishReason maps a Messages stop_reason onto an OpenAI finish_reason.
func anthropicFinishReason(stopReason string) *string {
	if stopReason == "" {
		return nil
	}
	value := "stop"
	switch stopReason {
	case "max_tokens":
		value = "length"
	case "tool_use":
		value = "tool_calls"
	}
	return &value
}

// Chat performs a non-streaming completion.
func (p *Anthropic) Chat(ctx context.Context, request Request) (*Result, error) {
	payload, err := json.Marshal(p.toAnthropicBody(request, false))
	if err != nil {
		return nil, err
	}
	rawURL, targetURL := p.cfg.endpoint("/messages")
	response, err := util.FetchUpstream(ctx, rawURL, util.FetchOptions{
		Method:    http.MethodPost,
		Headers:   p.headers(request),
		Body:      bytes.NewReader(payload),
		TimeoutMs: p.cfg.RequestTimeoutMs,
		URL:       targetURL,
	})
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &util.UpstreamStatusError{Status: response.StatusCode, Body: truncate(string(body), 500)}
	}

	var parsed struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		Content    any    `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	text, calls := parseAnthropicContent(parsed.Content)
	message := Message{Role: "assistant", ToolCalls: calls}
	if text != "" || len(calls) == 0 {
		message.Content = &text
	}
	result := &Result{
		ID:           parsed.ID,
		Created:      time.Now().Unix(),
		Model:        parsed.Model,
		Message:      message,
		FinishReason: anthropicFinishReason(parsed.StopReason),
		Usage: &Usage{
			PromptTokens:     parsed.Usage.InputTokens,
			CompletionTokens: parsed.Usage.OutputTokens,
			TotalTokens:      parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		},
		Headers: util.FilterResponseHeaders(response.Header),
	}
	if result.ID == "" {
		result.ID = "chatcmpl-" + randomSuffix()
	}
	if result.Model == "" {
		result.Model = request.Model
	}
	return result, nil
}

// ChatStream performs a streaming completion, converting Anthropic SSE events
// into OpenAI chunks on the fly.
func (p *Anthropic) ChatStream(ctx context.Context, request Request) (*http.Response, error) {
	payload, err := json.Marshal(p.toAnthropicBody(request, true))
	if err != nil {
		return nil, err
	}
	rawURL, targetURL := p.cfg.endpoint("/messages")
	upstream, err := util.FetchUpstream(ctx, rawURL, util.FetchOptions{
		Method:          http.MethodPost,
		Headers:         p.headers(request),
		Body:            bytes.NewReader(payload),
		TimeoutMs:       0,
		HeaderTimeoutMs: p.cfg.HeaderTimeoutMs,
		URL:             targetURL,
	})
	if err != nil {
		return nil, err
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body := util.ReadBodyLimited(upstream, 500)
		return nil, &util.UpstreamStatusError{Status: upstream.StatusCode, Body: body}
	}

	id := "chatcmpl-anthropic-" + randomSuffix()
	model := request.Model
	promptTokens := 0
	completionTokens := 0
	// toolIndexByBlock maps an Anthropic content-block index onto the position
	// of that tool call among the tool_calls OpenAI clients see.
	toolIndexByBlock := map[int]int{}
	toolCount := 0

	pipe := util.NewBufferedPipe()
	go func() {
		defer upstream.Body.Close()
		defer pipe.Close()

		send := func(payload any) error {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			_, err = pipe.Write([]byte("data: " + string(encoded) + "\n\n"))
			return err
		}
		sendChunk := func(delta map[string]any, finish any) {
			_ = send(map[string]any{
				"id": id, "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			})
		}

		parser := util.NewSSEParser(func(event util.SSEEvent) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
				return
			}
			switch parsed["type"] {
			case "content_block_start":
				block, _ := parsed["content_block"].(map[string]any)
				if kind, _ := block["type"].(string); kind == "tool_use" {
					toolIndex := toolCount
					toolCount++
					toolIndexByBlock[intFromAny(parsed["index"])] = toolIndex
					toolID, _ := block["id"].(string)
					name, _ := block["name"].(string)
					sendChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIndex, "id": toolID, "type": "function",
						"function": map[string]any{"name": name, "arguments": ""},
					}}}, nil)
				}
			case "content_block_delta":
				delta, _ := parsed["delta"].(map[string]any)
				switch kind, _ := delta["type"].(string); kind {
				case "text_delta":
					if text, ok := delta["text"].(string); ok {
						sendChunk(map[string]any{"content": text}, nil)
					}
				case "input_json_delta":
					toolIndex, known := toolIndexByBlock[intFromAny(parsed["index"])]
					partial, _ := delta["partial_json"].(string)
					if known && partial != "" {
						sendChunk(map[string]any{"tool_calls": []any{map[string]any{
							"index":    toolIndex,
							"function": map[string]any{"arguments": partial},
						}}}, nil)
					}
				}
			case "message_start":
				if message, ok := parsed["message"].(map[string]any); ok {
					if usage, ok := message["usage"].(map[string]any); ok {
						promptTokens = intFromAny(usage["input_tokens"])
					}
				}
			case "message_delta":
				if usage, ok := parsed["usage"].(map[string]any); ok {
					if value, ok := usage["output_tokens"]; ok {
						completionTokens = intFromAny(value)
					}
				}
				finish := "stop"
				if delta, ok := parsed["delta"].(map[string]any); ok {
					if reason, _ := delta["stop_reason"].(string); reason != "" {
						finish = *anthropicFinishReason(reason)
					}
				}
				_ = send(map[string]any{
					"id": id, "object": "chat.completion.chunk",
					"created": time.Now().Unix(), "model": model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
					"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens},
				})
			}
		})

		bufferPtr := util.GetStreamBuffer()
		defer util.PutStreamBuffer(bufferPtr)
		buffer := *bufferPtr
		for {
			n, err := upstream.Body.Read(buffer)
			if n > 0 {
				parser.PushBytes(buffer[:n])
			}
			if err != nil {
				break
			}
		}
		parser.Flush()
		_, _ = pipe.Write([]byte("data: [DONE]\n\n"))
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		Body: pipe,
	}, nil
}

// PassthroughOptions describes one raw Anthropic Messages call.
type PassthroughOptions struct {
	// Body is the exact client body. Only its top-level model may change.
	Body []byte
	// Header is the client request header. Its credentials are never forwarded.
	Header http.Header
	Stream bool
	// IdleTimeoutMs bounds the gap between streamed chunks. Zero disables it.
	IdleTimeoutMs int
}

// Passthrough sends a client Anthropic Messages body upstream unchanged, so
// tools, tool results, system prompts and every other field survive. Only the
// model (when the alias maps to another upstream model) and the credential are
// rewritten. The response is returned as-is, including non-2xx statuses, so the
// caller decides how to relay it.
func (p *Anthropic) Passthrough(ctx context.Context, request Request, opts PassthroughOptions) (*http.Response, error) {
	payload := opts.Body
	if request.ClientModel != "" && request.ClientModel != request.Model {
		modelValue, err := json.Marshal(request.Model)
		if err != nil {
			return nil, err
		}
		payload, err = util.RewriteTopLevelJSON(opts.Body, map[string]json.RawMessage{"model": modelValue})
		if err != nil {
			return nil, err
		}
	}

	headers := util.ForwardedRequestHeaders(opts.Header)
	delete(headers, "x-api-key")
	headers["content-type"] = "application/json"
	if headers["anthropic-version"] == "" {
		headers["anthropic-version"] = "2023-06-01"
	}
	if p.cfg.APIKey != nil && *p.cfg.APIKey != "" {
		headers["x-api-key"] = *p.cfg.APIKey
	}
	headers = applyCustomHeaders(headers, p.cfg, request)

	rawURL, targetURL := p.cfg.endpoint("/messages")
	fetch := util.FetchOptions{
		Method:  http.MethodPost,
		Headers: headers,
		Body:    bytes.NewReader(payload),
		URL:     targetURL,
	}
	if opts.Stream {
		fetch.HeaderTimeoutMs = p.cfg.HeaderTimeoutMs
		fetch.IdleTimeoutMs = opts.IdleTimeoutMs
	} else {
		fetch.TimeoutMs = p.cfg.RequestTimeoutMs
	}
	return util.FetchUpstream(ctx, rawURL, fetch)
}

// NewAnthropic builds the Anthropic adapter for a config.
func NewAnthropic(cfg Config) *Anthropic {
	return &Anthropic{OpenAI: OpenAI{cfg: cfg}}
}

// intFromAny coerces a JSON number (or numeric string) to int.
func intFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}
