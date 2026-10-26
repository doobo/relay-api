// Package providers adapts OpenAI-format chat requests onto the different
// upstream wire formats (openai, anthropic, compatible, openai-responses,
// responses).
package providers

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Config is everything needed to reach one upstream.
type Config struct {
	ID      int64
	Name    string
	Type    string
	BaseURL string
	APIKey  *string
	// Headers are extra upstream headers. Values may contain ${name}
	// placeholders resolved from the client request headers, or ${_ai_session}
	// for the resolved client identity.
	Headers          map[string]string
	RequestTimeoutMs int
}

// Request is a normalized chat request.
type Request struct {
	Model       string
	Stream      bool
	Temperature *float64
	MaxTokens   *int
	// RawBody is the original client body, forwarded as-is so parameters the
	// gateway does not model (tools, response_format, top_p, ...) survive.
	RawBody map[string]any
	// RawBytes is the exact buffered client body. ResponsesPassthrough uses it
	// to avoid re-encoding the JSON, preserving formatting and number text.
	RawBytes []byte
	// RawResponse asks an OpenAI-compatible provider to return the upstream
	// non-streaming body in Result.RawBody and only parse usage. The chat
	// endpoint uses it so extra upstream fields survive unchanged.
	RawResponse bool
	// ClientHeaders are the lowercased client request headers, available to
	// custom provider headers through ${name}.
	ClientHeaders map[string]string
	// Session is the resolved client identity (the _ai_session context value).
	Session string
	// OnUpstreamHeaders, when set, receives the final headers about to be sent
	// upstream. The gateway uses it to record a snapshot for usage details.
	OnUpstreamHeaders func(map[string]string)
}

// Usage is token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Message is the assistant reply.
type Message struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is one function call emitted by the assistant.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the name and JSON-encoded arguments of a tool call.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Result is a non-streaming chat completion in OpenAI terms.
type Result struct {
	ID           string
	Created      int64
	Model        string
	Message      Message
	FinishReason *string
	Usage        *Usage
	Headers      map[string]string
	// RawBody is the untouched upstream body when the provider was asked for a
	// raw response. The chat handler writes it verbatim.
	RawBody []byte
}

// Provider is one upstream adapter.
type Provider interface {
	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, request Request) (*Result, error)
	// ChatStream performs a streaming completion and returns the upstream (or
	// transformed) response; the caller owns and closes Body.
	ChatStream(ctx context.Context, request Request) (*http.Response, error)
}

// randomSuffix is a human-readable id fragment for generated completions.
func randomSuffix() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// New builds a provider for the given config.
func New(cfg Config) Provider {
	switch cfg.Type {
	case "anthropic":
		return &Anthropic{OpenAI: OpenAI{cfg: cfg}}
	case "openai-responses", "responses":
		return &Responses{OpenAI: OpenAI{cfg: cfg}}
	default:
		// "openai" and "compatible" share the exact same wire format.
		return &OpenAI{cfg: cfg}
	}
}
