package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"relay-api/internal/util"
)

// OpenAI speaks the OpenAI chat-completions wire format. Compatible upstreams
// (DeepSeek, Qwen, vLLM, Ollama, ...) reuse it unchanged.
type OpenAI struct {
	cfg Config
}

func (p *OpenAI) endpoint(path string) string {
	return strings.TrimRight(p.cfg.BaseURL, "/") + path
}

func (p *OpenAI) headers() map[string]string {
	headers := map[string]string{"content-type": "application/json"}
	if p.cfg.APIKey != nil && *p.cfg.APIKey != "" {
		headers["authorization"] = "Bearer " + *p.cfg.APIKey
	}
	return headers
}

// buildBody copies the raw client body and pins model/stream.
func (p *OpenAI) buildBody(request Request, stream bool) map[string]any {
	body := make(map[string]any, len(request.RawBody)+2)
	for key, value := range request.RawBody {
		body[key] = value
	}
	body["model"] = request.Model
	body["stream"] = stream
	return body
}

// Chat performs a non-streaming completion.
func (p *OpenAI) Chat(ctx context.Context, request Request) (*Result, error) {
	payload, err := json.Marshal(p.buildBody(request, false))
	if err != nil {
		return nil, err
	}
	response, err := util.FetchUpstream(ctx, p.endpoint("/chat/completions"), util.FetchOptions{
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
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	result := &Result{
		ID:           parsed.ID,
		Created:      parsed.Created,
		Model:        parsed.Model,
		Message:      Message{Role: "assistant"},
		FinishReason: nil,
		Usage:        parsed.Usage,
		Headers:      util.FilterResponseHeaders(response.Header),
	}
	if result.ID == "" {
		result.ID = "chatcmpl-" + randomSuffix()
	}
	if result.Model == "" {
		result.Model = request.Model
	}
	if len(parsed.Choices) > 0 {
		choice := parsed.Choices[0]
		if choice.Message.Role != "" {
			result.Message.Role = choice.Message.Role
		}
		result.Message.Content = choice.Message.Content
		result.FinishReason = choice.FinishReason
	}
	return result, nil
}

// ChatStream performs a streaming completion, returning the upstream response.
func (p *OpenAI) ChatStream(ctx context.Context, request Request) (*http.Response, error) {
	payload, err := json.Marshal(p.buildBody(request, true))
	if err != nil {
		return nil, err
	}
	response, err := util.FetchUpstream(ctx, p.endpoint("/chat/completions"), util.FetchOptions{
		Method:  http.MethodPost,
		Headers: p.headers(),
		Body:    bytes.NewReader(payload),
		// No overall timeout for streams; the caller enforces an idle timeout.
		TimeoutMs: 0,
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := util.ReadBodyLimited(response, 500)
		return nil, &util.UpstreamStatusError{Status: response.StatusCode, Body: body}
	}
	return response, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
