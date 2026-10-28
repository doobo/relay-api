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

// assistantMarker is the cheap gate for reasoning replay: without an assistant
// turn in the history there is nothing to repair, so the hot path never pays for
// a JSON decode.
var assistantMarker = []byte(`"assistant"`)

// mayReplayReasoning reports whether the outgoing body is worth decoding to look
// for assistant turns whose reasoning must be restored. It is deliberately
// conservative: a provider without the opt-in, a cold cache or a first turn
// costs one memchr scan.
func (p *OpenAI) mayReplayReasoning(request Request) bool {
	if !p.cfg.ReasoningReplay || !HasReasoning() {
		return false
	}
	if len(request.RawBytes) > 0 {
		return bytes.Contains(request.RawBytes, assistantMarker)
	}
	messages, _ := request.RawBody["messages"].([]any)
	return len(messages) > 0
}

// payload pins model/stream on the exact client bytes when they are available,
// falling back to a map round-trip for callers that pass only RawBody.
//
// The raw bytes are returned untouched whenever possible: upstreams that never
// emit reasoning (the common case) must not pay for a JSON decode on every
// request. A decode only happens when the provider opted into reasoning replay
// and a cached assistant turn may need its reasoning restored (see reasoning.go).
func (p *OpenAI) payload(request Request, stream bool) ([]byte, error) {
	if len(request.RawBytes) > 0 {
		unchanged := request.clientModel() == request.Model && request.clientStreamMatches(stream)
		if unchanged && !p.mayReplayReasoning(request) {
			return request.RawBytes, nil
		}
		if p.mayReplayReasoning(request) {
			if body, err := decodeJSONObject(request.RawBytes); err == nil {
				p.replayReasoning(request, body)
				body["model"] = request.Model
				body["stream"] = stream
				return json.Marshal(body)
			}
		}
		if unchanged {
			return request.RawBytes, nil
		}
		if rewritten, err := rewriteModelStream(request, stream); err == nil {
			return rewritten, nil
		}
		// The rewrite should not fail for a body the handler accepted, but if it
		// does, decode the raw bytes so no client field is dropped.
		if request.RawBody == nil {
			if body, err := decodeJSONObject(request.RawBytes); err == nil {
				request.RawBody = body
			}
		}
	}
	body := p.buildBody(request, stream)
	if p.mayReplayReasoning(request) {
		p.replayReasoning(request, body)
	}
	return json.Marshal(body)
}

// rewriteModelStream pins model/stream without decoding the rest of the body.
func rewriteModelStream(request Request, stream bool) ([]byte, error) {
	modelValue, err := json.Marshal(request.Model)
	if err != nil {
		return nil, err
	}
	streamValue := json.RawMessage("false")
	if stream {
		streamValue = json.RawMessage("true")
	}
	return util.RewriteTopLevelJSON(request.RawBytes, map[string]json.RawMessage{
		"model":  modelValue,
		"stream": streamValue,
	})
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// replayReasoning re-attaches cached reasoning to assistant turns the client
// resent without it. It reports whether the body changed.
func (p *OpenAI) replayReasoning(request Request, body map[string]any) bool {
	if !p.cfg.ReasoningReplay {
		return false
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	changed := false
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		if reasoning := messageReasoning(message); reasoning != "" {
			// Some clients echo the reasoning under `reasoning`; the DeepSeek
			// family only reads `reasoning_content`, so mirror it.
			if _, present := message["reasoning_content"]; !present {
				message["reasoning_content"] = reasoning
				changed = true
			}
			continue
		}
		reasoning, ok := RecallReasoning(
			p.cfg.Name,
			request.clientModel(),
			request.Session,
			assistantTurnText(message),
			assistantTurnToolIDs(message),
		)
		if !ok {
			continue
		}
		message["reasoning_content"] = reasoning
		changed = true
	}
	return changed
}

// messageReasoning returns reasoning already present on an assistant message,
// accepting the OpenAI (`reasoning_content`), alternative (`reasoning`) and
// Anthropic (`content[].thinking`) spellings.
func messageReasoning(message map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if value, ok := message[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	switch content := message["content"].(type) {
	case []any:
		var builder strings.Builder
		for _, part := range content {
			object, ok := part.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := object["type"].(string)
			if kind != "thinking" && kind != "reasoning" {
				continue
			}
			if text, ok := object["thinking"].(string); ok {
				builder.WriteString(text)
			} else if text, ok := object["text"].(string); ok {
				builder.WriteString(text)
			}
		}
		return builder.String()
	default:
		return ""
	}
}

// assistantTurnText flattens a resent assistant message's text so it can be
// fingerprinted. Tool arguments are excluded on purpose: clients re-encode them
// and the serialization is not guaranteed to be stable.
func assistantTurnText(message map[string]any) string {
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		var builder strings.Builder
		for _, part := range content {
			object, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := object["type"].(string); kind == "thinking" || kind == "reasoning" {
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

// assistantTurnToolIDs collects the tool-call ids of a resent assistant message.
// The ids are stable across a round-trip (arguments are not), so they make the
// turn identifiable even when the content itself is empty.
func assistantTurnToolIDs(message map[string]any) []string {
	calls, _ := message["tool_calls"].([]any)
	if len(calls) == 0 {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := call["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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
		// Validate the body and extract usage without re-encoding the choices, so
		// the handler can write the upstream bytes verbatim.
		var usageOnly struct {
			Usage   *Usage `json:"usage"`
			Choices []struct {
				Message struct {
					Content          *string    `json:"content"`
					ReasoningContent *string    `json:"reasoning_content"`
					Reasoning        *string    `json:"reasoning"`
					ToolCalls        []ToolCall `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &usageOnly); err != nil {
			return nil, err
		}
		if len(usageOnly.Choices) > 0 {
			message := usageOnly.Choices[0].Message
			p.rememberTurn(request, message.Content, message.ToolCalls, message.ReasoningContent, message.Reasoning)
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
		p.rememberTurn(request, choice.Message.Content, choice.Message.ToolCalls, choice.Message.ReasoningContent, nil)
	}
	return result, nil
}

// rememberTurn caches the reasoning of one assistant response so the next
// request can restore it when the client drops it (see reasoning.go). It is a
// no-op unless the provider opted into reasoning replay.
func (p *OpenAI) rememberTurn(request Request, content *string, toolCalls []ToolCall, reasoningContent, reasoning *string) {
	if !p.cfg.ReasoningReplay {
		return
	}
	reasoningText := ""
	switch {
	case reasoningContent != nil:
		reasoningText = *reasoningContent
	case reasoning != nil:
		reasoningText = *reasoning
	}
	if reasoningText == "" {
		return
	}
	ids := make([]string, 0, len(toolCalls))
	for _, call := range toolCalls {
		if call.ID != "" {
			ids = append(ids, call.ID)
		}
	}
	RememberReasoning(p.cfg.Name, request.clientModel(), request.Session, messageContent(content), ids, reasoningText)
}

func messageContent(content *string) string {
	if content == nil {
		return ""
	}
	return *content
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
		// The header deadline still bounds an upstream that never responds.
		TimeoutMs:       0,
		HeaderTimeoutMs: p.cfg.HeaderTimeoutMs,
		URL:             targetURL,
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
