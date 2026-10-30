package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

const anthropicTestMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":15,"cache_read_input_tokens":10}}`

const anthropicTestStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":10}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

// TestMessagesPassthrough checks that /v1/messages relays the client body with
// only the model rewritten, swaps the credential, and returns the upstream
// response unchanged for both JSON and streamed replies.
func TestMessagesPassthrough(t *testing.T) {
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "messages.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")

	database, err := db.Open(filepath.Join(t.TempDir(), "messages.db"))
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

	var mu sync.Mutex
	var seenBody []byte
	var seenHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seenBody = body
		seenHeader = r.Header.Clone()
		mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("content-type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicTestStream)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicTestMessage)
	}))
	defer upstream.Close()

	providerID := mustProvider(t, "claude", "anthropic", upstream.URL)
	if _, err := db.CreateModel("claude", providerID, "claude-x", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}

	clientKey := "sk-messages-client-123456"
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "messages", Prefix: clientKey[:8], KeyHash: util.SHA256Hex(clientKey),
		Scope: "both", RateLimit: 10000,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	cfg := config.Load()
	gw := NewGateway(cfg, router.NewResolver(secrets))
	mux := http.NewServeMux()
	gw.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.RequestLog(middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(middleware.NewRateLimiter())(middleware.AISession(mux))))

	send := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("x-api-key", clientKey)
		request.Header.Set("anthropic-version", "2023-06-01")
		request.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	toolBody := `{"model":"claude","max_tokens":64,"system":"be brief",` +
		`"tools":[{"name":"get_weather","description":"Weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],` +
		`"messages":[{"role":"user","content":"weather?"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"18C"}]}]}`

	recorder := send(t, toolBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != anthropicTestMessage {
		t.Fatalf("response not relayed verbatim: %s", recorder.Body.String())
	}

	mu.Lock()
	var sent, original map[string]any
	if err := json.Unmarshal(seenBody, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if err := json.Unmarshal([]byte(toolBody), &original); err != nil {
		t.Fatalf("client body: %v", err)
	}
	header := seenHeader
	mu.Unlock()

	original["model"] = "claude-x"
	if !reflect.DeepEqual(sent, original) {
		t.Fatalf("upstream body changed beyond model:\n got  %v\n want %v", sent, original)
	}
	if got := header.Get("x-api-key"); got == clientKey {
		t.Fatalf("gateway key leaked upstream as x-api-key")
	}
	if got := header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}

	streamBody := strings.Replace(toolBody, `"max_tokens":64,`, `"max_tokens":64,"stream":true,`, 1)
	recorder = send(t, streamBody)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != anthropicTestStream {
		t.Fatalf("stream not relayed verbatim:\n%s", recorder.Body.String())
	}
}

// TestAnthropicUsageTapTallies feeds a stream one byte at a time, so events
// straddle reads, and checks the usage it tallies.
func TestAnthropicUsageTapTallies(t *testing.T) {
	tap := newAnthropicUsageTap(iotest.OneByteReader(strings.NewReader(anthropicTestStream)))
	relayed, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(relayed) != anthropicTestStream {
		t.Fatalf("tap altered the stream")
	}
	usage := tap.finish().toUsage()
	if usage.PromptTokens != 35 || usage.CompletionTokens != 15 || usage.TotalTokens != 50 {
		t.Fatalf("usage = %+v", usage)
	}
}
