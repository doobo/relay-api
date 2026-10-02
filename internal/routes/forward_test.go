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
	"relay-api/internal/util"
)

type capturedRequest struct {
	Method   string
	Path     string
	RawQuery string
	Headers  http.Header
	Body     string
}

func TestForward(t *testing.T) {
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "gateway.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	// The httptest mock listens on 127.0.0.1; allow private upstreams for every
	// subtest except the SSRF one, which clears it again.
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "1")

	if _, err := db.Open(filepath.Join(t.TempDir(), "forward.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(db.Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	secrets, err := util.NewSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}

	var (
		mu      sync.Mutex
		lastReq capturedRequest
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastReq = capturedRequest{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Headers: r.Header.Clone(), Body: string(body)}
		mu.Unlock()

		city := ""
		var decoded map[string]any
		if json.Unmarshal(body, &decoded) == nil {
			city, _ = decoded["city"].(string)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": "sunny in " + city,
			"path":   r.URL.Path,
			"method": r.Method,
		})
	}))
	defer mock.Close()

	capture := func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return lastReq
	}

	// Configs.
	mustConfig(t, db.NewAPIConfig{
		Name: "weather", Method: "POST", URL: mock.URL + "/weather",
		Headers:          stringPtr(`{"X-Cfg":"{{headers.xcustom}}"}`),
		RequestTemplate:  stringPtr(`{"city":"{{body.city}}"}`),
		ResponseTemplate: stringPtr(`{"content":"{{data.result}}"}`),
		TimeoutMs:        15000, Route: "open", Enabled: true,
	})
	mustConfig(t, db.NewAPIConfig{
		Name: "api/**", Method: "POST", URL: mock.URL + "/base",
		TimeoutMs: 15000, Route: "open", Enabled: true,
	})
	mustConfig(t, db.NewAPIConfig{
		Name: "any", Method: "NONE", URL: mock.URL + "/echo",
		TimeoutMs: 15000, Route: "open", Enabled: true,
	})
	mustConfig(t, db.NewAPIConfig{
		Name: "local", Method: "POST", URL: "http://127.0.0.1:9/",
		TimeoutMs: 15000, Route: "open", Enabled: true,
	})
	mustConfig(t, db.NewAPIConfig{
		Name: "stream/**", Method: "POST", URL: mock.URL,
		TimeoutMs: 15000, Route: "open", Enabled: true,
	})

	fullKey := "sk-forward-key-1234"
	if _, err := db.CreateAPIKey(db.NewAPIKey{Name: "test", Prefix: fullKey[:8], KeyHash: util.SHA256Hex(fullKey), Scope: "both", RateLimit: 10000}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	restrictedKey := "sk-restricted-key-12"
	allowed := `["something-else"]`
	if _, err := db.CreateAPIKey(db.NewAPIKey{Name: "restricted", Prefix: restrictedKey[:8], KeyHash: util.SHA256Hex(restrictedKey), Scope: "both", RateLimit: 10000, AllowedAPIs: &allowed}); err != nil {
		t.Fatalf("create restricted key: %v", err)
	}

	cfg := config.Load()
	openMux := http.NewServeMux()
	NewForwardRoutes(cfg, secrets, "open").Register(openMux, "/open")
	openMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	handler := middleware.RequestLog(middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(middleware.NewRateLimiter())(openMux)))

	call := func(t *testing.T, method, path, bearer, body string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("request and response templates", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/open/weather", fullKey, `{"city":"Hangzhou"}`, map[string]string{"Xcustom": "abc"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		if recorder.Body.String() != `{"content":"sunny in Hangzhou"}`+"\n" && strings.TrimSpace(recorder.Body.String()) != `{"content":"sunny in Hangzhou"}` {
			t.Fatalf("unexpected body: %s", recorder.Body.String())
		}
		seen := capture()
		if seen.Method != http.MethodPost || seen.Path != "/weather" {
			t.Fatalf("upstream saw %s %s", seen.Method, seen.Path)
		}
		if seen.Headers.Get("X-Cfg") != "abc" {
			t.Fatalf("config header not templated: %q", seen.Headers.Get("X-Cfg"))
		}
		if strings.TrimSpace(seen.Body) != `{"city":"Hangzhou"}` {
			t.Fatalf("request template not applied: %s", seen.Body)
		}
	})

	t.Run("wildcard forwards the tail and query", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/open/api/users/1?fields=name", fullKey, `{"k":"v"}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		seen := capture()
		if seen.Path != "/base/users/1" {
			t.Fatalf("path = %q", seen.Path)
		}
		if seen.RawQuery != "fields=name" {
			t.Fatalf("query = %q", seen.RawQuery)
		}
		if seen.Body != `{"k":"v"}` {
			t.Fatalf("body passthrough = %q", seen.Body)
		}
	})

	t.Run("method NONE follows the client verb", func(t *testing.T) {
		recorder := call(t, http.MethodGet, "/open/any", fullKey, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		if seen := capture(); seen.Method != http.MethodGet {
			t.Fatalf("upstream method = %s", seen.Method)
		}
	})

	t.Run("SSRF blocks private upstream", func(t *testing.T) {
		t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "")
		recorder := call(t, http.MethodPost, "/open/local", fullKey, `{}`, nil)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "blocked private/internal host") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("wildcard hint on bare prefix", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/open/stream", fullKey, `{}`, nil)
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "stream/**") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("allowed_apis denies the config", func(t *testing.T) {
		recorder := call(t, http.MethodPost, "/open/weather", restrictedKey, `{"city":"x"}`, nil)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "config_not_allowed") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func mustConfig(t *testing.T, input db.NewAPIConfig) {
	t.Helper()
	if _, err := db.CreateAPIConfig(input); err != nil {
		t.Fatalf("create config %s: %v", input.Name, err)
	}
}
