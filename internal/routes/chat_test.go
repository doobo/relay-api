package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

// TestGateway exercises the full /v1 handler chain against mock upstreams:
// failover, streaming, Anthropic conversion and non-retryable passthrough.
func TestGateway(t *testing.T) {
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "gateway.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")

	database, err := db.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	secrets, err := util.NewSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}

	// Mock upstreams: one that always fails, one healthy, one that returns 400.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
	}))
	defer reject.Close()
	good := httptest.NewServer(http.HandlerFunc(healthyUpstream))
	defer good.Close()
	var seenHeaders http.Header
	headerUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		healthyUpstream(w, r)
	}))
	defer headerUpstream.Close()

	badProvider := mustProvider(t, "bad", "openai", bad.URL)
	goodProvider := mustProvider(t, "good", "openai", good.URL)
	anthropicProvider := mustProvider(t, "claude", "anthropic", good.URL)
	responsesProvider := mustProvider(t, "responses", "openai-responses", good.URL)
	rejectProvider := mustProvider(t, "reject", "openai", reject.URL)
	headersJSON := `{"x-user-id":"${x-user-id}","x-session":"${_ai_session}","x-static":"fixed"}`
	headerProviderRow, err := db.CreateProvider("headers", "openai", headerUpstream.URL, nil, &headersJSON, true)
	if err != nil {
		t.Fatalf("create header provider: %v", err)
	}

	// Alias "m" has two routes: bad first (500 -> failover), good second.
	if _, err := db.CreateModel("m", badProvider, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	insertRoute(t, "m", badProvider, 1)
	insertRoute(t, "m", goodProvider, 2)
	if _, err := db.CreateModel("claude", anthropicProvider, "claude-x", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := db.CreateModel("gpt", responsesProvider, "gpt-6-luna", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := db.CreateModel("e400", rejectProvider, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := db.CreateModel("hdrs", headerProviderRow.ID, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}

	fullKey := "sk-test-key-123456"
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "test", Prefix: fullKey[:8], KeyHash: util.SHA256Hex(fullKey),
		Scope: "both", RateLimit: 10000,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	restrictedKey := "sk-restricted-key-123456"
	allowedModels := `["m"]`
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "restricted", Prefix: restrictedKey[:8], KeyHash: util.SHA256Hex(restrictedKey),
		Scope: "both", RateLimit: 10000, AllowedModels: &allowedModels,
	}); err != nil {
		t.Fatalf("create restricted api key: %v", err)
	}

	cfg := config.Load()
	gw := NewGateway(cfg, router.NewResolver(secrets))
	mux := http.NewServeMux()
	gw.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.RequestLog(middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(middleware.NewRateLimiter())(middleware.AISession(mux))))

	do := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("failover to healthy provider", func(t *testing.T) {
		recorder := do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var parsed struct {
			Object  string `json:"object"`
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if parsed.Object != "chat.completion" || len(parsed.Choices) != 1 || *parsed.Choices[0].Message.Content != "hello" {
			t.Fatalf("unexpected completion: %s", recorder.Body.String())
		}
		if parsed.Usage.TotalTokens != 5 {
			t.Fatalf("usage not forwarded: %s", recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"system_fingerprint":"fp_test"`) {
			t.Fatalf("extra upstream field dropped: %s", recorder.Body.String())
		}
	})

	t.Run("streaming", func(t *testing.T) {
		recorder := do(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if ct := recorder.Header().Get("content-type"); !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("content-type = %q", ct)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("unexpected stream: %s", body)
		}
	})

	t.Run("anthropic non-stream", func(t *testing.T) {
		recorder := do(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var parsed struct {
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(parsed.Choices) != 1 || *parsed.Choices[0].Message.Content != "bonjour" {
			t.Fatalf("unexpected anthropic conversion: %s", recorder.Body.String())
		}
		if parsed.Usage.PromptTokens != 4 || parsed.Usage.CompletionTokens != 2 {
			t.Fatalf("usage not mapped: %s", recorder.Body.String())
		}
	})

	t.Run("anthropic stream", func(t *testing.T) {
		recorder := do(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		body := recorder.Body.String()
		if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, `"content":"hi"`) {
			t.Fatalf("unexpected anthropic stream: %s", body)
		}
		if !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("missing DONE: %s", body)
		}
	})

	t.Run("non-retryable upstream status passes through", func(t *testing.T) {
		recorder := do(t, `{"model":"e400","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "bad request") {
			t.Fatalf("body not passed through: %s", recorder.Body.String())
		}
	})

	t.Run("responses non-stream", func(t *testing.T) {
		recorder := do(t, `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var parsed struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					ToolCalls []any   `json:"tool_calls"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content == nil || *parsed.Choices[0].Message.Content != "bonjour" {
			t.Fatalf("unexpected responses conversion: %s", recorder.Body.String())
		}
		if len(parsed.Choices[0].Message.ToolCalls) != 1 || parsed.Choices[0].FinishReason == nil || *parsed.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("tool call not mapped: %s", recorder.Body.String())
		}
		if parsed.Usage.PromptTokens != 4 || parsed.Usage.CompletionTokens != 2 {
			t.Fatalf("usage not mapped: %s", recorder.Body.String())
		}
	})

	t.Run("responses stream", func(t *testing.T) {
		recorder := do(t, `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		body := recorder.Body.String()
		if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, `"content":"hi"`) {
			t.Fatalf("unexpected responses stream: %s", body)
		}
		if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
			t.Fatalf("unexpected responses stream finish: %s", body)
		}
	})

	t.Run("custom provider headers use client values and session", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"hdrs","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("content-type", "application/json")
		request.Header.Set("X-User-Id", "alice")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if seenHeaders.Get("x-user-id") != "alice" || seenHeaders.Get("x-session") != "alice" || seenHeaders.Get("x-static") != "fixed" {
			t.Fatalf("custom headers not forwarded: %v", seenHeaders)
		}
	})

	t.Run("usage stores header snapshots", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"hdrs","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("content-type", "application/json")
		request.Header.Set("X-Opencode-Session", "oc-1")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if err := db.FlushUsage(context.Background()); err != nil {
			t.Fatalf("flush usage: %v", err)
		}
		var requestRaw, upstreamRaw string
		if err := db.Get().QueryRow(
			`SELECT request_headers, upstream_headers FROM usage_logs
			 WHERE model = 'hdrs' AND provider = 'headers' ORDER BY id DESC LIMIT 1`,
		).Scan(&requestRaw, &upstreamRaw); err != nil {
			t.Fatalf("query headers: %v", err)
		}
		var requestHeaders, upstreamHeaders map[string]string
		if err := json.Unmarshal([]byte(requestRaw), &requestHeaders); err != nil {
			t.Fatalf("decode request headers: %v", err)
		}
		if err := json.Unmarshal([]byte(upstreamRaw), &upstreamHeaders); err != nil {
			t.Fatalf("decode upstream headers: %v", err)
		}
		if requestHeaders["x-opencode-session"] != "oc-1" || requestHeaders["authorization"] != "[redacted]" {
			t.Fatalf("unexpected request snapshot: %+v", requestHeaders)
		}
		if upstreamHeaders["x-session"] != "oc-1" || upstreamHeaders["x-static"] != "fixed" {
			t.Fatalf("unexpected upstream snapshot: %+v", upstreamHeaders)
		}
	})

	t.Run("unknown model", func(t *testing.T) {
		recorder := do(t, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "model_not_found") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("allowed models restrict use and discovery", func(t *testing.T) {
		post := func(model string) *httptest.ResponseRecorder {
			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+restrictedKey)
			request.Header.Set("content-type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder
		}

		denied := post("claude")
		if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "model_not_allowed") {
			t.Fatalf("disallowed model status = %d, body = %s", denied.Code, denied.Body.String())
		}
		if allowed := post("m"); allowed.Code != http.StatusOK {
			t.Fatalf("allowed model status = %d, body = %s", allowed.Code, allowed.Body.String())
		}

		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+restrictedKey)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var listed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode models: %v", err)
		}
		if len(listed.Data) != 1 || listed.Data[0].ID != "m" {
			t.Fatalf("unexpected model list: %+v", listed.Data)
		}
	})

	t.Run("missing api key", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", recorder.Code)
		}
	})

	t.Run("usage recorded", func(t *testing.T) {
		// Chat records usage asynchronously; flush the batch writer so the rows
		// are visible before they are counted.
		if err := db.FlushUsage(context.Background()); err != nil {
			t.Fatalf("flush usage: %v", err)
		}
		var count int
		if err := db.Get().QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE kind = 'ai' AND status = 200`).Scan(&count); err != nil {
			t.Fatalf("query usage: %v", err)
		}
		if count == 0 {
			t.Fatal("expected at least one recorded AI usage row")
		}
		today, err := db.TodayStatsSince()
		if err != nil {
			t.Fatalf("today stats: %v", err)
		}
		if today.Requests == 0 {
			t.Fatal("live dashboard counters did not observe the async usage rows")
		}
	})
}

func mustProvider(t *testing.T, name, kind, baseURL string) int64 {
	t.Helper()
	provider, err := db.CreateProvider(name, kind, baseURL, nil, nil, true)
	if err != nil {
		t.Fatalf("create provider %s: %v", name, err)
	}
	return provider.ID
}

func insertRoute(t *testing.T, modelName string, providerID int64, priority int) {
	t.Helper()
	if _, err := db.Get().Exec(
		`INSERT INTO model_routes (model_name, provider_id, upstream_model, priority, weight, enabled) VALUES (?, ?, ?, ?, 100, 1)`,
		modelName, providerID, "upstream-model", priority,
	); err != nil {
		t.Fatalf("insert model route: %v", err)
	}
}

// healthyUpstream serves OpenAI chat completions, Anthropic messages and both
// streaming variants.
func healthyUpstream(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "upstream-model",
			"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": "hello"},
				"finish_reason": "stop",
			}},
			"usage":              map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
			"system_fingerprint": "fp_test",
		})
	case strings.HasSuffix(r.URL.Path, "/responses"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\",\"model\":\"gpt-6-luna\",\"created_at\":1}}\n\n")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"model\":\"gpt-6-luna\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4},\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}]}}\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "created_at": 1, "model": "gpt-6-luna", "status": "completed",
			"output": []any{
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "bonjour"}}},
				map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "weather", "arguments": "{}"},
			},
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 2, "total_tokens": 6},
		})
	case strings.HasSuffix(r.URL.Path, "/messages"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "model": "claude-x",
			"content":     []any{map[string]any{"type": "text", "text": "bonjour"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 4, "output_tokens": 2},
		})
	default:
		http.NotFound(w, r)
	}
}
