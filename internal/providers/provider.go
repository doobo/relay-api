// Package providers adapts OpenAI-format chat requests onto the different
// upstream wire formats (openai, anthropic, compatible, openai-responses,
// responses).
package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config is everything needed to reach one upstream.
type Config struct {
	ID      int64
	Name    string
	Type    string
	BaseURL string
	// BaseURLParsed is BaseURL parsed once by the resolver. When present the
	// provider builds each endpoint from it, avoiding a url.Parse on every
	// request. Nil falls back to parsing BaseURL per request (tests and
	// directly constructed configs).
	BaseURLParsed *url.URL
	APIKey        *string
	// Headers are extra upstream headers. Values may contain ${name}
	// placeholders resolved from the client request headers, or ${_ai_session}
	// for the resolved client identity.
	Headers map[string]string
	// HeaderTemplates is Headers pre-parsed by the resolver. Nil falls back to
	// compiling Headers per request (tests and directly constructed configs).
	HeaderTemplates []HeaderTemplate
	// ReasoningReplay opts this provider into remembering and re-attaching
	// reasoning_content across turns (see reasoning.go). Off by default.
	ReasoningReplay  bool
	RequestTimeoutMs int
	// HeaderTimeoutMs bounds the wait for response headers on streaming
	// calls. Zero disables it.
	HeaderTimeoutMs int
}

// endpoint resolves one upstream path against the provider's base URL. It
// returns either a parsed URL (fast path) or a string for net/http to parse.
func (c Config) endpoint(path string) (string, *url.URL) {
	if c.BaseURLParsed == nil {
		return strings.TrimRight(c.BaseURL, "/") + path, nil
	}
	target := *c.BaseURLParsed
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawPath = ""
	return "", &target
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
	// ClientModel and ClientStream are the top-level model/stream values read
	// from the raw client body. ClientStreamKnown marks ClientStream as
	// authoritative even when the field was absent (which means false). They
	// let OpenAI.payload prove the raw bytes need no rewrite without decoding
	// the body into a map.
	ClientModel       string
	ClientStream      bool
	ClientStreamKnown bool
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
