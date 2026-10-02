// Package providers adapts OpenAI-format chat requests onto the different
// upstream wire formats (openai, anthropic, compatible).
package providers

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Config is everything needed to reach one upstream.
type Config struct {
	ID               int64
	Name             string
	Type             string
	BaseURL          string
	APIKey           *string
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
}

// Usage is token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Message is the assistant reply.
type Message struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
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
	default:
		// "openai" and "compatible" share the exact same wire format.
		return &OpenAI{cfg: cfg}
	}
}
