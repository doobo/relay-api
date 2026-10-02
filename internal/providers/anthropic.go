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
// never know the upstream is Anthropic.
type Anthropic struct {
	OpenAI
}

func (p *Anthropic) endpoint(path string) string {
	return strings.TrimRight(p.cfg.BaseURL, "/") + path
}

func (p *Anthropic) headers() map[string]string {
	headers := map[string]string{
		"content-type":      "application/json",
		"anthropic-version": "2023-06-01",
	}
	if p.cfg.APIKey != nil && *p.cfg.APIKey != "" {
		headers["x-api-key"] = *p.cfg.APIKey
	}
	return headers
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// convertMessages maps OpenAI messages onto Anthropic's system + messages.
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
		text := extractContentText(message["content"])
		switch role {
		case "system":
			if system != "" {
				system += "\n" + text
			} else {
				system = text
			}
		case "user", "assistant":
			out = append(out, anthropicMessage{Role: role, Content: text})
		}
	}
	return system, out
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

// extractBlocksText flattens Anthropic response content blocks into text.
func extractBlocksText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, block := range value {
			object, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := object["type"].(string); kind != "text" {
				continue
			}
			if text, ok := object["text"].(string); ok {
				builder.WriteString(text)
			}
		}
		return builder.String()
	default:
		return ""
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
	return body
}

func anthropicFinishReason(stopReason string) *string {
	if stopReason == "" {
		return nil
	}
	switch stopReason {
	case "max_tokens":
		value := "length"
		return &value
	case "stop_sequence":
		value := "stop"
		return &value
	default:
		value := stopReason
		return &value
	}
}

// Chat performs a non-streaming completion.
func (p *Anthropic) Chat(ctx context.Context, request Request) (*Result, error) {
	payload, err := json.Marshal(p.toAnthropicBody(request, false))
	if err != nil {
		return nil, err
	}
	response, err := util.FetchUpstream(ctx, p.endpoint("/messages"), util.FetchOptions{
		Method:    http.MethodPost,
		Headers:   p.headers(),
		Body:      bytes.NewReader(payload),
		TimeoutMs: p.cfg.RequestTimeoutMs,
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

	content := extractBlocksText(parsed.Content)
	result := &Result{
		ID:           parsed.ID,
		Created:      time.Now().Unix(),
		Model:        parsed.Model,
		Message:      Message{Role: "assistant", Content: &content},
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
	upstream, err := util.FetchUpstream(ctx, p.endpoint("/messages"), util.FetchOptions{
		Method:    http.MethodPost,
		Headers:   p.headers(),
		Body:      bytes.NewReader(payload),
		TimeoutMs: 0,
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

	reader, writer := io.Pipe()
	go func() {
		defer upstream.Body.Close()
		defer writer.Close()

		send := func(payload any) error {
			encoded, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			_, err = writer.Write([]byte("data: " + string(encoded) + "\n\n"))
			return err
		}

		parser := util.NewSSEParser(func(event util.SSEEvent) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
				return
			}
			switch parsed["type"] {
			case "content_block_delta":
				delta, _ := parsed["delta"].(map[string]any)
				if kind, _ := delta["type"].(string); kind == "text_delta" {
					if text, ok := delta["text"].(string); ok {
						_ = send(map[string]any{
							"id": id, "object": "chat.completion.chunk",
							"created": time.Now().Unix(), "model": model,
							"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
						})
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
					if reason, _ := delta["stop_reason"].(string); reason == "max_tokens" {
						finish = "length"
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

		buffer := make([]byte, 32*1024)
		for {
			n, err := upstream.Body.Read(buffer)
			if n > 0 {
				parser.Push(string(buffer[:n]))
			}
			if err != nil {
				break
			}
		}
		parser.Flush()
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":  []string{"text/event-stream"},
			"Cache-Control": []string{"no-cache"},
		},
		Body: reader,
	}, nil
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
