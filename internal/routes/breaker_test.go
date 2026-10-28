package routes

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/router"
	"relay-api/internal/util"
)

// TestGatewayCircuitBreakerSkipsFailingProvider proves the breaker short-
// circuits a dead upstream instead of letting every request (and retry) hit
// it: the first request records the failure, the second is answered locally
// with 503 and never reaches the failing server.
func TestGatewayCircuitBreakerSkipsFailingProvider(t *testing.T) {
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))

	database, err := db.Open(filepath.Join(t.TempDir(), "breaker.db"))
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

	var calls atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"server_error","message":"Database error"}}`))
	}))
	defer failing.Close()

	providerID := mustProvider(t, "failing", "openai", failing.URL)
	if _, err := db.CreateModel("m", providerID, "upstream-model", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}

	fullKey := "sk-breaker-key-123456"
	if _, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "test", Prefix: fullKey[:8], KeyHash: util.SHA256Hex(fullKey),
		Scope: "both", RateLimit: 10000,
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	cfg := config.Load()
	cfg.UpstreamMaxRetries = 0
	cfg.UpstreamBreakerThreshold = 1
	cfg.UpstreamBreakerCooldownMs = 60_000

	gateway := NewGateway(cfg, router.NewResolver(secrets))
	mux := http.NewServeMux()
	gateway.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.BodyParser(cfg.RequestSizeLimitBytes)(
		middleware.APIKeyAuth(middleware.NewRateLimiter())(middleware.AISession(mux)),
	)

	do := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("content-type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if first := do(); first.Code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	second := do()
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "upstream_unavailable") {
		t.Fatalf("second body = %s, want upstream_unavailable", second.Body.String())
	}
	if retryAfter := second.Header().Get("Retry-After"); retryAfter != "60" {
		t.Fatalf("Retry-After = %q, want 60", retryAfter)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (breaker must skip the second request)", got)
	}
}
