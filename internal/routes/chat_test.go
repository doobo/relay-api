package routes

import (
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

	badProvider := mustProvider(t, "bad", "openai", bad.URL)
	goodProvider := mustProvider(t, "good", "openai", good.URL)
	anthropicProvider := mustProvider(t, "claude", "anthropic", good.URL)
	rejectProvider := mustProvider(t, "reject", "openai", reject.URL)

	// Alias "m" has two routes: bad first (500 -> failover), good second.
	if _, err := db.CreateModel("m", badProvider, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	insertRoute(t, "m", badProvider, 1)
	insertRoute(t, "m", goodProvider, 2)
	if _, err := db.CreateModel("claude", anthropicProvider, "claude-x", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := db.CreateModel("e400", rejectProvider, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}

	fullKey := "sk-test-key-123456"
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "test", Prefix: fullKey[:8], KeyHash: util.SHA256Hex(fullKey),
		Scope: "both", RateLimit: 10000,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	cfg := config.Load()
	gw := NewGateway(cfg, router.NewResolver(secrets))
	mux := http.NewServeMux()
	gw.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.RequestLog(middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(middleware.NewRateLimiter())(mux)))

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

	t.Run("unknown model", func(t *testing.T) {
		recorder := do(t, `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "model_not_found") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
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
		var count int
		if err := db.Get().QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE kind = 'ai' AND status = 200`).Scan(&count); err != nil {
			t.Fatalf("query usage: %v", err)
		}
		if count == 0 {
			t.Fatal("expected at least one recorded AI usage row")
		}
	})
}

func mustProvider(t *testing.T, name, kind, baseURL string) int64 {
	t.Helper()
	provider, err := db.CreateProvider(name, kind, baseURL, nil, true)
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
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
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
