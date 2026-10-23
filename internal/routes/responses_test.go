package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

// responsesUpstream is a chat-completions upstream that returns text, reasoning
// and two tool calls (one function, one custom) in both JSON and SSE form.
func responsesUpstream(capture func(map[string]any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if capture != nil {
			capture(body)
		}
		stream, _ := body["stream"].(bool)
		if stream {
			w.Header().Set("content-type", "text/event-stream")
			writeChunk := func(payload map[string]any) {
				encoded, _ := json.Marshal(payload)
				_, _ = io.WriteString(w, "data: "+string(encoded)+"\n\n")
			}
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"reasoning_content": "thinking "},
			}}})
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "Hello "},
			}}})
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "world"},
			}}})
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1",
					"function": map[string]any{"name": "shell", "arguments": `{"command":`},
				}}},
			}}})
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "function": map[string]any{"arguments": `"ls"}`},
				}}},
			}}})
			writeChunk(map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 1, "id": "call_2",
					"function": map[string]any{"name": "apply_patch", "arguments": `{"input":"*** Begin Patch"}`},
				}}},
			}}})
			writeChunk(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}

		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "upstream",
			"choices": []any{map[string]any{
				"message": map[string]any{
					"role":              "assistant",
					"content":           "Hello world",
					"reasoning_content": "thinking",
					"tool_calls": []any{
						map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
							"name": "shell", "arguments": `{"command":"ls"}`,
						}},
						map[string]any{"id": "call_2", "type": "function", "function": map[string]any{
							"name": "apply_patch", "arguments": `{"input":"*** Begin Patch"}`,
						}},
					},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	})
}

func TestResponsesCompatibility(t *testing.T) {
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "gateway.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "1")

	if _, err := db.Open(filepath.Join(t.TempDir(), "responses.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(db.Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	secrets, err := util.NewSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}

	var (
		mu   sync.Mutex
		seen map[string]any
	)
	upstream := httptest.NewServer(responsesUpstream(func(body map[string]any) {
		mu.Lock()
		seen = body
		mu.Unlock()
	}))
	defer upstream.Close()

	providerID := mustProvider(t, "openai", "openai", upstream.URL)
	if _, err := db.CreateModel("codex", providerID, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}
	fullKey := "sk-responses-key-1234"
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "test", Prefix: fullKey[:8], KeyHash: util.SHA256Hex(fullKey),
		Scope: "ai", RateLimit: 10000,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	cfg := config.Load()
	gw := NewGateway(cfg, router.NewResolver(secrets))
	mux := http.NewServeMux()
	gw.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.RequestLog(middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(middleware.NewRateLimiter())(middleware.AISession(mux))))

	requestBody := func(stream bool) string {
		encoded, _ := json.Marshal(map[string]any{
			"model":        "codex",
			"instructions": "be terse",
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "list files"},
				}},
				map[string]any{"type": "message", "role": "assistant", "content": []any{
					map[string]any{"type": "output_text", "text": "running"},
				}},
				map[string]any{"type": "function_call", "call_id": "call_x", "name": "shell", "arguments": `{"command":"ls"}`},
				map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "file.txt"},
			},
			"tools": []any{
				map[string]any{"type": "function", "name": "shell", "description": "run", "parameters": map[string]any{
					"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
				}},
				map[string]any{"type": "custom", "name": "apply_patch", "description": "patch"},
			},
			"tool_choice":         "auto",
			"parallel_tool_calls": false,
			"stream":              stream,
		})
		return string(encoded)
	}

	do := func(t *testing.T, stream bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody(stream)))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("non-stream output and request conversion", func(t *testing.T) {
		recorder := do(t, false)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Object string `json:"object"`
			Status string `json:"status"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Input     string `json:"input"`
			} `json:"output"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if response.Object != "response" || response.Status != "completed" {
			t.Fatalf("unexpected response: %s", recorder.Body.String())
		}
		var haveMessage, haveShell, havePatch, haveReasoning bool
		for _, item := range response.Output {
			switch item.Type {
			case "reasoning":
				haveReasoning = true
			case "message":
				haveMessage = len(item.Content) == 1 && item.Content[0].Text == "Hello world"
			case "function_call":
				haveShell = item.Name == "shell" && item.Arguments == `{"command":"ls"}`
			case "custom_tool_call":
				havePatch = item.Name == "apply_patch" && item.Input == "*** Begin Patch"
			}
		}
		if !haveReasoning || !haveMessage || !haveShell || !havePatch {
			t.Fatalf("missing output items: %s", recorder.Body.String())
		}
		if response.Usage.InputTokens != 5 || response.Usage.OutputTokens != 3 {
			t.Fatalf("usage not mapped: %s", recorder.Body.String())
		}

		mu.Lock()
		upstreamBody := seen
		mu.Unlock()
		messages, _ := upstreamBody["messages"].([]any)
		if len(messages) != 4 {
			t.Fatalf("chat messages = %#v", messages)
		}
		if system, _ := messages[0].(map[string]any); system["role"] != "system" || system["content"] != "be terse" {
			t.Fatalf("instructions not converted: %#v", messages[0])
		}
		if assistant, _ := messages[2].(map[string]any); assistant["content"] != "running" || assistant["tool_calls"] == nil {
			t.Fatalf("function call not merged into assistant message: %#v", messages[2])
		}
		if tool, _ := messages[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "call_x" || tool["content"] != "file.txt" {
			t.Fatalf("function output not converted: %#v", messages[3])
		}
		tools, _ := upstreamBody["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("chat tools = %#v", tools)
		}
		if custom, _ := tools[1].(map[string]any); custom["type"] != "function" {
			t.Fatalf("custom tool not mapped: %#v", tools[1])
		}
	})

	t.Run("stream emits Responses events", func(t *testing.T) {
		recorder := do(t, true)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		for _, want := range []string{
			"response.created",
			"response.reasoning_summary_text.delta",
			`"delta":"Hello "`,
			"response.output_text.done",
			`"type":"message"`,
			`"type":"function_call"`,
			`"call_id":"call_1"`,
			`"type":"custom_tool_call"`,
			`"input":"*** Begin Patch"`,
			"response.completed",
			`"total_tokens":8`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("stream missing %q:\n%s", want, body)
			}
		}
	})
}
