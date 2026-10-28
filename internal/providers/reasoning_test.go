package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReasoningReplayRequiresOptIn is the default-behaviour guard: a provider
// that did not enable the option must forward the client body byte-for-byte and
// must never cache reasoning.
func TestReasoningReplayRequiresOptIn(t *testing.T) {
	ResetReasoningCache()
	t.Cleanup(ResetReasoningCache)

	raw := []byte(`{"model":"m","stream":false,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"hello"},` +
		`{"role":"user","content":"again"}]}`)
	request := Request{
		Model:             "m",
		ClientModel:       "m",
		ClientStreamKnown: true,
		Session:           "session-a",
		RawBytes:          raw,
	}

	RememberReasoning("deepseek", "m", "session-a", "hello", nil, "pondering")

	provider := &OpenAI{cfg: Config{Name: "deepseek"}}
	payload, err := provider.payload(request, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(payload) != string(raw) {
		t.Fatalf("a provider without the opt-in rewrote the body:\n%s", payload)
	}
}

// TestReasoningReplayDoesNotCacheWithoutOptIn covers the response side: a
// provider that did not opt in must not fill the cache.
func TestReasoningReplayDoesNotCacheWithoutOptIn(t *testing.T) {
	ResetReasoningCache()
	t.Cleanup(ResetReasoningCache)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi","reasoning_content":"pondering"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	provider := New(Config{Name: "p", Type: "openai", BaseURL: server.URL, RequestTimeoutMs: 1000})
	if _, err := provider.Chat(context.Background(), Request{
		Model:       "m",
		ClientModel: "m",
		RawBytes:    []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`),
		RawResponse: true,
	}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if HasReasoning() {
		t.Fatal("reasoning was cached for a provider without the opt-in")
	}
}

// TestReasoningReplayRestoresDroppedTurn is the regression this exists for: a
// client resends the assistant turn without reasoning, and the upstream would
// reject the request with "The `reasoning_content` in the thinking mode must be
// passed back to the API."
func TestReasoningReplayRestoresDroppedTurn(t *testing.T) {
	ResetReasoningCache()
	t.Cleanup(ResetReasoningCache)

	provider := &OpenAI{cfg: Config{Name: "deepseek", ReasoningReplay: true}}
	raw := []byte(`{"model":"m","stream":false,"tools":[{"type":"function","function":{"name":"t"}}],` +
		`"messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"t","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	request := Request{
		Model:             "m",
		ClientModel:       "m",
		ClientStreamKnown: true,
		ClientStream:      false,
		Session:           "session-a",
		RawBytes:          raw,
	}

	// Cold cache: the body must pass through byte-for-byte.
	payload, err := provider.payload(request, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(payload) != string(raw) {
		t.Fatalf("cold cache rewrote the body:\n%s", payload)
	}

	RememberReasoning("deepseek", "m", "session-a", "hello", []string{"call_1"}, "pondering")

	payload, err = provider.payload(request, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %#v", body.Messages)
	}
	if got := body.Messages[1]["reasoning_content"]; got != "pondering" {
		t.Fatalf("reasoning_content = %#v, want it replayed", got)
	}
	if _, present := body.Messages[0]["reasoning_content"]; present {
		t.Fatalf("user turn got reasoning: %#v", body.Messages[0])
	}
}

// TestReasoningReplayIsScoped guards against leaking one conversation's
// reasoning into another session, another model or another provider.
func TestReasoningReplayIsScoped(t *testing.T) {
	ResetReasoningCache()
	t.Cleanup(ResetReasoningCache)

	raw := []byte(`{"model":"m","stream":false,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"hello"},` +
		`{"role":"user","content":"again"}]}`)
	base := Request{
		Model:             "m",
		ClientModel:       "m",
		ClientStreamKnown: true,
		Session:           "session-a",
		RawBytes:          raw,
	}

	RememberReasoning("deepseek", "m", "session-a", "hello", nil, "pondering")

	cases := map[string]struct {
		provider string
		session  string
		want     bool
	}{
		"matching turn":  {"deepseek", "session-a", true},
		"other session":  {"deepseek", "session-b", false},
		"other provider": {"openai", "session-a", false},
	}
	for name, testCase := range cases {
		request := base
		request.Session = testCase.session
		provider := &OpenAI{cfg: Config{Name: testCase.provider, ReasoningReplay: true}}
		payload, err := provider.payload(request, false)
		if err != nil {
			t.Fatalf("%s: payload: %v", name, err)
		}
		got := strings.Contains(string(payload), `"reasoning_content":"pondering"`)
		if got != testCase.want {
			t.Fatalf("%s: replayed = %v, want %v (body %s)", name, got, testCase.want, payload)
		}
	}
}

// TestReasoningReplayKeepsClientReasoning verifies that reasoning the client did
// echo is not overwritten, and that the `reasoning` spelling is mirrored to
// `reasoning_content` for upstreams that only read the latter.
func TestReasoningReplayKeepsClientReasoning(t *testing.T) {
	ResetReasoningCache()
	t.Cleanup(ResetReasoningCache)

	RememberReasoning("deepseek", "m", "session-a", "hello", nil, "cached")

	provider := &OpenAI{cfg: Config{Name: "deepseek", ReasoningReplay: true}}
	raw := []byte(`{"model":"m","stream":false,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"hello","reasoning":"from-client"},` +
		`{"role":"user","content":"again"}]}`)
	payload, err := provider.payload(Request{
		Model:             "m",
		ClientModel:       "m",
		ClientStreamKnown: true,
		Session:           "session-a",
		RawBytes:          raw,
	}, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.Messages[1]["reasoning_content"]; got != "from-client" {
		t.Fatalf("reasoning_content = %#v, want the client's value", got)
	}
}
