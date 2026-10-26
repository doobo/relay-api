package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIRawRequestAndResponsePassthrough(t *testing.T) {
	const upstreamBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3},"system_fingerprint":"fp_test"}`
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer server.Close()

	provider := New(Config{Name: "openai", Type: "openai", BaseURL: server.URL, RequestTimeoutMs: 1000})
	raw := []byte(`{"model":"alias","stream":true,"messages":[{"role":"user","content":"hi"}],"custom":42}`)
	result, err := provider.Chat(context.Background(), Request{
		Model:       "upstream-model",
		RawBytes:    raw,
		RawResponse: true,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	wantRequest := `{"model":"upstream-model","stream":false,"messages":[{"role":"user","content":"hi"}],"custom":42}`
	if string(seen) != wantRequest {
		t.Fatalf("request body = %s, want %s", seen, wantRequest)
	}
	if string(result.RawBody) != upstreamBody {
		t.Fatalf("raw response changed: %s", result.RawBody)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestOpenAIPayloadSkipsRewriteWhenUnchanged(t *testing.T) {
	provider := &OpenAI{}

	raw := []byte(`{"model":"upstream-model","stream":true,"messages":[{"role":"user","content":"hi"}],"custom":42}`)
	got, err := provider.payload(Request{
		Model:    "upstream-model",
		RawBytes: raw,
		RawBody:  map[string]any{"model": "upstream-model", "stream": true},
	}, true)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("payload rewrote unchanged body: got %s", got)
	}

	// A missing stream flag already means false for chat completions.
	rawStatic := []byte(`{"model":"upstream-model","messages":[]}`)
	got, err = provider.payload(Request{
		Model:    "upstream-model",
		RawBytes: rawStatic,
		RawBody:  map[string]any{"model": "upstream-model"},
	}, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(got) != string(rawStatic) {
		t.Fatalf("payload rewrote unchanged body: got %s", got)
	}
}

func TestOpenAIPayloadSkipsRewriteWithSummaryFields(t *testing.T) {
	provider := &OpenAI{}
	raw := []byte(`{"model":"upstream-model","stream":false,"messages":[{"role":"user","content":"hi"}],"keep":true}`)
	got, err := provider.payload(Request{
		Model:             "upstream-model",
		RawBytes:          raw,
		ClientModel:       "upstream-model",
		ClientStream:      false,
		ClientStreamKnown: true,
	}, false)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("payload rewrote unchanged body: got %s", got)
	}
}
