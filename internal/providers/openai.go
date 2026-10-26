package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"relay-api/internal/util"
)

// OpenAI speaks the OpenAI chat-completions wire format. Compatible upstreams
// (DeepSeek, Qwen, vLLM, Ollama, ...) reuse it unchanged.
type OpenAI struct {
	cfg Config
}

func (p *OpenAI) headers(request Request) map[string]string {
	headers := map[string]string{"content-type": "application/json"}
	if p.cfg.APIKey != nil && *p.cfg.APIKey != "" {
		headers["authorization"] = "Bearer " + *p.cfg.APIKey
	}
	return applyCustomHeaders(headers, p.cfg, request)
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

// payload pins model/stream on the exact client bytes when they are available,
// falling back to a map round-trip for callers that pass only RawBody.
func (p *OpenAI) payload(request Request, stream bool) ([]byte, error) {
	if len(request.RawBytes) > 0 {
		// The alias usually resolves to the same upstream model name and the
		// client already sent the right stream flag. In that case the raw bytes
		// are already exactly what upstream should receive: skip the full JSON
		// scan/rewrite entirely.
		if request.clientModel() == request.Model && request.clientStreamMatches(stream) {
			return request.RawBytes, nil
		}
		modelValue, err := json.Marshal(request.Model)
		if err != nil {
			return nil, err
		}
		streamValue := json.RawMessage("false")
		if stream {
			streamValue = json.RawMessage("true")
		}
		if rewritten, err := util.RewriteTopLevelJSON(request.RawBytes, map[string]json.RawMessage{
			"model":  modelValue,
			"stream": streamValue,
		}); err == nil {
			return rewritten, nil
		}
		// The rewrite should not fail for a body the handler accepted, but if
		// it does, decode the raw bytes so no client field is dropped.
		if request.RawBody == nil {
			var parsed map[string]any
			if json.Unmarshal(request.RawBytes, &parsed) == nil {
				request.RawBody = parsed
			}
		}
	}
	return json.Marshal(p.buildBody(request, stream))
}

// clientModel returns the model from the raw client body. ClientModel is the
// fast path; RawBody keeps callers that only set the decoded map working.
func (request Request) clientModel() string {
	if request.ClientModel != "" {
		return request.ClientModel
	}
	if request.RawBody == nil {
		return ""
	}
	value, _ := request.RawBody["model"].(string)
	return value
}

// clientStreamMatches reports whether the client body already carries the
// requested stream flag. An absent flag means false.
func (request Request) clientStreamMatches(stream bool) bool {
	if request.ClientStreamKnown {
		return request.ClientStream == stream
	}
	if request.RawBody == nil {
		return false
	}
	value, present := request.RawBody["stream"]
	if !present {
		return !stream
	}
	flag, ok := value.(bool)
	return ok && flag == stream
}

// Chat performs a non-streaming completion.
func (p *OpenAI) Chat(ctx context.Context, request Request) (*Result, error) {
	payload, err := p.payload(request, false)
	if err != nil {
		return nil, err
	}
	rawURL, targetURL := p.cfg.endpoint("/chat/completions")
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

	if request.RawResponse {
		// Validate the body and extract usage without decoding the choices, so
		// the handler can write the upstream bytes verbatim.
		var usageOnly struct {
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal(body, &usageOnly); err != nil {
			return nil, err
		}
		return &Result{
			Usage:   usageOnly.Usage,
			Headers: util.FilterResponseHeaders(response.Header),
			RawBody: body,
		}, nil
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
		result.Message.ReasoningContent = choice.Message.ReasoningContent
		result.Message.ToolCalls = choice.Message.ToolCalls
		result.FinishReason = choice.FinishReason
	}
	return result, nil
}

// ChatStream performs a streaming completion, returning the upstream response.
func (p *OpenAI) ChatStream(ctx context.Context, request Request) (*http.Response, error) {
	payload, err := p.payload(request, true)
	if err != nil {
		return nil, err
	}
	rawURL, targetURL := p.cfg.endpoint("/chat/completions")
	response, err := util.FetchUpstream(ctx, rawURL, util.FetchOptions{
		Method:  http.MethodPost,
		Headers: p.headers(request),
		Body:    bytes.NewReader(payload),
		// No overall timeout for streams; the caller enforces an idle timeout.
		TimeoutMs: 0,
		URL:       targetURL,
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
