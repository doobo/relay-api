package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/util"
)

// adminHarness wires the /admin resource routes directly (without the auth
// middleware, which is covered elsewhere).
type adminHarness struct {
	handler http.Handler
	secrets *util.SecretBox
}

func newAdminHarness(t *testing.T) adminHarness {
	t.Helper()
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "gateway.db"))
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	t.Setenv("ALLOW_PRIVATE_UPSTREAMS", "1")

	if _, err := db.Open(filepath.Join(t.TempDir(), "admin.db")); err != nil {
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

	mux := http.NewServeMux()
	NewAdminResources(secrets, 7).Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	return adminHarness{handler: mux, secrets: secrets}
}

func (h adminHarness) call(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

// decodeInto fails the test when the recorder body is not JSON.
func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

func TestAdminModelRoutes(t *testing.T) {
	harness := newAdminHarness(t)
	provider, err := db.CreateProvider("p1", "openai", "https://example.com", nil, nil, false, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := db.CreateModel("gpt-4o", provider.ID, "gpt-4o-2024", true, 100); err != nil {
		t.Fatalf("create model: %v", err)
	}

	recorder := harness.call(t, http.MethodPost, "/admin/model-routes",
		`{"modelName":"gpt-4o","providerId":`+itoa(provider.ID)+`,"upstreamModel":"gpt-4o-2024"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created modelRouteDTO
	decodeInto(t, recorder, &created)
	if created.ModelName != "gpt-4o" || created.Priority != 100 || created.Weight != 100 || created.Enabled != 1 {
		t.Fatalf("unexpected created route: %+v", created)
	}

	t.Run("unknown provider is rejected", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPost, "/admin/model-routes",
			`{"modelName":"gpt-4o","providerId":9999,"upstreamModel":"y"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unknown model alias is rejected", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPost, "/admin/model-routes",
			`{"modelName":"no-such-model","providerId":`+itoa(provider.ID)+`,"upstreamModel":"y"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("list returns the route", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/model-routes", "")
		var body struct {
			Data []modelRouteDTO `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) != 1 || body.Data[0].ID != created.ID {
			t.Fatalf("unexpected list: %+v", body.Data)
		}
	})

	t.Run("delete then 404", func(t *testing.T) {
		recorder := harness.call(t, http.MethodDelete, "/admin/model-routes/"+itoa(created.ID), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("delete status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		again := harness.call(t, http.MethodDelete, "/admin/model-routes/"+itoa(created.ID), "")
		if again.Code != http.StatusNotFound {
			t.Fatalf("second delete status = %d", again.Code)
		}
	})
}

func TestAdminConfigStatsAndAssociations(t *testing.T) {
	harness := newAdminHarness(t)
	config, err := db.CreateAPIConfig(db.NewAPIConfig{
		Name: "weather", Method: "POST", URL: "https://example.com/weather",
		TimeoutMs: 15000, Route: "open", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	// Two calls today: one OK, one error.
	latency := int64(120)
	okStatus := http.StatusOK
	badStatus := http.StatusInternalServerError
	if err := db.RecordUsage(db.UsageRecord{
		RequestID: "r1", Kind: "api", APIConfigID: &config.ID, LatencyMs: &latency, Status: &okStatus,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := db.RecordUsage(db.UsageRecord{
		RequestID: "r2", Kind: "api", APIConfigID: &config.ID, LatencyMs: &latency, Status: &badStatus,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	t.Run("config stats", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-configs/"+itoa(config.ID)+"/stats", "")
		var stats configStatRowDTO
		decodeInto(t, recorder, &stats)
		if stats.Requests != 2 || stats.Errors != 1 || stats.AvgLatencyMs != 120 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	allowed := `["weather"]`
	other := `["something-else"]`
	restricted, err := db.CreateAPIKey(db.NewAPIKey{Name: "restricted", Prefix: "sk-r1", KeyHash: util.SHA256Hex("sk-r1"), Scope: "both", RateLimit: 60, AllowedAPIs: &allowed})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	unrestricted, err := db.CreateAPIKey(db.NewAPIKey{Name: "open", Prefix: "sk-o1", KeyHash: util.SHA256Hex("sk-o1"), Scope: "both", RateLimit: 60})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := db.CreateAPIKey(db.NewAPIKey{Name: "other", Prefix: "sk-x1", KeyHash: util.SHA256Hex("sk-x1"), Scope: "both", RateLimit: 60, AllowedAPIs: &other}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	t.Run("config keys includes unrestricted and matching", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-configs/"+itoa(config.ID)+"/keys", "")
		var body struct {
			Data []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
				Hash string `json:"key_hash"`
			} `json:"data"`
		}
		decodeInto(t, recorder, &body)
		names := map[string]bool{}
		for _, key := range body.Data {
			names[key.Name] = true
			if key.Hash != "" {
				t.Fatalf("key_hash leaked for %s", key.Name)
			}
		}
		if len(names) != 2 || !names["restricted"] || !names["open"] || names["other"] {
			t.Fatalf("unexpected keys: %+v", names)
		}
	})

	t.Run("key configs lists only allowed", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-keys/"+itoa(restricted.ID)+"/configs", "")
		var body struct {
			Data []struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Route string `json:"route"`
			} `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) != 1 || body.Data[0].Name != "weather" {
			t.Fatalf("unexpected key configs: %+v", body.Data)
		}
		if body.Data[0].Route != "open" {
			t.Fatalf("route = %q", body.Data[0].Route)
		}
	})

	t.Run("unrestricted key sees every config", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-keys/"+itoa(unrestricted.ID)+"/configs", "")
		var body struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) != 1 || body.Data[0].Name != "weather" {
			t.Fatalf("unexpected configs: %+v", body.Data)
		}
	})
}

func TestAdminAPIKeyAllowedModels(t *testing.T) {
	harness := newAdminHarness(t)

	recorder := harness.call(t, http.MethodPost, "/admin/api-keys",
		`{"name":"limited","allowedModels":["m1","m2"]}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created apiKeyDTO
	decodeInto(t, recorder, &created)
	if created.AllowedModels == nil || *created.AllowedModels != `["m1","m2"]` {
		t.Fatalf("unexpected allowed models: %+v", created.AllowedModels)
	}

	t.Run("update replaces the list", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/api-keys/"+itoa(created.ID),
			`{"allowedModels":["m3"]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var updated apiKeyDTO
		decodeInto(t, recorder, &updated)
		if updated.AllowedModels == nil || *updated.AllowedModels != `["m3"]` {
			t.Fatalf("unexpected allowed models: %+v", updated.AllowedModels)
		}
	})

	t.Run("omitting the field keeps the list", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/api-keys/"+itoa(created.ID),
			`{"name":"renamed"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var updated apiKeyDTO
		decodeInto(t, recorder, &updated)
		if updated.AllowedModels == nil || *updated.AllowedModels != `["m3"]` {
			t.Fatalf("unexpected allowed models: %+v", updated.AllowedModels)
		}
	})

	t.Run("empty list clears the restriction", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/api-keys/"+itoa(created.ID),
			`{"allowedModels":[]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var updated apiKeyDTO
		decodeInto(t, recorder, &updated)
		if updated.AllowedModels != nil {
			t.Fatalf("allowed models not cleared: %+v", updated.AllowedModels)
		}
	})
}

func TestAdminUsageLogsAndCleanup(t *testing.T) {
	harness := newAdminHarness(t)

	model := "gpt-4o"
	provider := "openai"
	latency := int64(50)
	okStatus := http.StatusOK
	for i := 0; i < 3; i++ {
		if err := db.RecordUsage(db.UsageRecord{
			RequestID: "req-" + itoa(int64(i)), Kind: "ai", Model: &model, Provider: &provider,
			LatencyMs: &latency, Status: &okStatus,
		}); err != nil {
			t.Fatalf("record usage: %v", err)
		}
	}
	if err := db.RecordUsage(db.UsageRecord{RequestID: "api-1", Kind: "api", LatencyMs: &latency, Status: &okStatus}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	t.Run("usage is newest-first and filterable", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/usage?limit=2", "")
		var body struct {
			Data []usageLogDTO `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) != 2 {
			t.Fatalf("limit not honored: %d rows", len(body.Data))
		}
		if body.Data[0].RequestID != "api-1" {
			t.Fatalf("newest row = %q", body.Data[0].RequestID)
		}

		recorder = harness.call(t, http.MethodGet, "/admin/usage?kind=ai", "")
		decodeInto(t, recorder, &body)
		if len(body.Data) != 3 {
			t.Fatalf("kind filter returned %d rows", len(body.Data))
		}

		recorder = harness.call(t, http.MethodGet, "/admin/usage?model=gpt-4o", "")
		decodeInto(t, recorder, &body)
		if len(body.Data) != 3 {
			t.Fatalf("model filter returned %d rows", len(body.Data))
		}
	})

	t.Run("stats aggregates today", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/stats", "")
		var body struct {
			Today struct {
				Requests int `json:"requests"`
			} `json:"today"`
			Models []modelStatDTO `json:"models"`
		}
		decodeInto(t, recorder, &body)
		if body.Today.Requests != 4 {
			t.Fatalf("today requests = %d", body.Today.Requests)
		}
		if len(body.Models) != 1 || body.Models[0].Model != "gpt-4o" || body.Models[0].Requests != 3 {
			t.Fatalf("unexpected models: %+v", body.Models)
		}
	})

	// An old audit entry plus a fresh one: only the old row is purged.
	oldCutoff := time.Now().UnixMilli() - 10*86_400_000
	if _, err := db.Get().Exec(`INSERT INTO audit_logs (action, target, source_ip, created_at) VALUES ('old', 'x', NULL, ?)`, oldCutoff); err != nil {
		t.Fatalf("insert old audit: %v", err)
	}
	if _, err := db.Get().Exec(`INSERT INTO usage_logs (request_id, kind, status, created_at) VALUES ('old-usage', 'ai', 200, ?)`, oldCutoff); err != nil {
		t.Fatalf("insert old usage: %v", err)
	}
	if err := db.RecordAudit("fresh", "y", "1.2.3.4"); err != nil {
		t.Fatalf("record audit: %v", err)
	}

	t.Run("logs list newest first", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/logs", "")
		var body struct {
			Data []auditLogDTO `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) < 2 || body.Data[0].Action != "fresh" {
			t.Fatalf("unexpected logs: %+v", body.Data)
		}
	})

	t.Run("cleanup purges only old rows", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPost, "/admin/logs/cleanup", "")
		var body struct {
			OK      bool `json:"ok"`
			Deleted struct {
				Usage int64 `json:"usage"`
				Audit int64 `json:"audit"`
			} `json:"deleted"`
		}
		decodeInto(t, recorder, &body)
		if !body.OK || body.Deleted.Usage != 1 || body.Deleted.Audit != 1 {
			t.Fatalf("unexpected cleanup result: %s", recorder.Body.String())
		}
		// The cleanup itself is audited, and the fresh entry survives.
		var auditCount int
		if err := db.Get().QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'logs_cleanup'`).Scan(&auditCount); err != nil {
			t.Fatalf("count audit: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("cleanup audit rows = %d", auditCount)
		}
	})
}

func TestAdminUsageDetail(t *testing.T) {
	harness := newAdminHarness(t)
	requestHeaders := `{"x-user-id":"alice","authorization":"[redacted]"}`
	upstreamHeaders := `{"x-user-id":"alice","x-opencode-session":"oc-1"}`
	if _, err := db.Get().Exec(
		`INSERT INTO usage_logs (request_id, kind, model, status, request_headers, upstream_headers, created_at)
		 VALUES ('req-detail', 'ai', 'gpt', 200, ?, ?, ?)`,
		requestHeaders, upstreamHeaders, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
	var id int64
	if err := db.Get().QueryRow(`SELECT id FROM usage_logs WHERE request_id = 'req-detail'`).Scan(&id); err != nil {
		t.Fatalf("get id: %v", err)
	}

	recorder := harness.call(t, http.MethodGet, "/admin/usage/"+itoa(id), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var detail usageDetailDTO
	decodeInto(t, recorder, &detail)
	if detail.RequestID != "req-detail" || detail.RequestHeaders["x-user-id"] != "alice" ||
		detail.UpstreamHeaders["x-opencode-session"] != "oc-1" {
		t.Fatalf("unexpected detail: %+v", detail)
	}

	missing := harness.call(t, http.MethodGet, "/admin/usage/9999", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", missing.Code)
	}
}

func TestAdminProviderHeaders(t *testing.T) {
	harness := newAdminHarness(t)

	recorder := harness.call(t, http.MethodPost, "/admin/providers",
		`{"name":"custom","type":"openai","baseUrl":"https://example.com","headers":{"x-user-id":"${uuid}","x-session":"${_ai_session}"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created providerDTO
	decodeInto(t, recorder, &created)
	if created.Headers["x-user-id"] != "${uuid}" || created.Headers["x-session"] != "${_ai_session}" {
		t.Fatalf("unexpected headers: %+v", created.Headers)
	}

	recorder = harness.call(t, http.MethodPut, "/admin/providers/"+itoa(created.ID),
		`{"headers":{"x-static":"fixed"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var updated providerDTO
	decodeInto(t, recorder, &updated)
	if len(updated.Headers) != 1 || updated.Headers["x-static"] != "fixed" {
		t.Fatalf("unexpected updated headers: %+v", updated.Headers)
	}

	t.Run("empty object clears headers", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/providers/"+itoa(created.ID), `{"headers":{}}`)
		var cleared providerDTO
		decodeInto(t, recorder, &cleared)
		if len(cleared.Headers) != 0 {
			t.Fatalf("headers not cleared: %+v", cleared.Headers)
		}
	})

	t.Run("invalid header name is rejected", func(t *testing.T) {
		recorder := harness.call(t, http.MethodPut, "/admin/providers/"+itoa(created.ID),
			`{"headers":{"bad header":"value"}}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestAdminAPIKeyStatsAndUsageFilter(t *testing.T) {
	harness := newAdminHarness(t)

	key, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "key-one", Prefix: "sk-one", KeyHash: "hash-one", Scope: "both",
		DailyRequestLimit: 10, DailyTokenLimit: 1000,
		WeeklyRequestLimit: 20, WeeklyTokenLimit: 2000,
		MonthlyRequestLimit: 30, MonthlyTokenLimit: 3000,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	other, err := db.CreateAPIKey(db.NewAPIKey{
		Name: "key-two", Prefix: "sk-two", KeyHash: "hash-two", Scope: "both",
	})
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}

	status := http.StatusOK
	keyID, otherID := key.ID, other.ID
	if err := db.RecordUsage(db.UsageRecord{
		RequestID: "r1", APIKeyID: &keyID, Kind: "ai", InputTokens: 3, OutputTokens: 4, Status: &status,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := db.RecordUsage(db.UsageRecord{
		RequestID: "r2", APIKeyID: &otherID, Kind: "ai", InputTokens: 9, OutputTokens: 9, Status: &status,
	}); err != nil {
		t.Fatalf("record other usage: %v", err)
	}

	t.Run("usage filter by key", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/usage?api_key_id="+itoa(key.ID), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var body struct {
			Data []usageLogDTO `json:"data"`
		}
		decodeInto(t, recorder, &body)
		if len(body.Data) != 1 || body.Data[0].RequestID != "r1" {
			t.Fatalf("unexpected filtered usage: %+v", body.Data)
		}
	})

	t.Run("key stats", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-keys/"+itoa(key.ID)+"/stats", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var stats apiKeyStatsDTO
		decodeInto(t, recorder, &stats)
		if stats.Key.ID != key.ID || stats.Key.Name != "key-one" {
			t.Fatalf("unexpected key: %+v", stats.Key)
		}
		if stats.Daily.UsedRequests != 1 || stats.Daily.UsedTokens != 7 {
			t.Fatalf("daily usage = %+v", stats.Daily)
		}
		if stats.Daily.RequestLimit != 10 || stats.Daily.TokenLimit != 1000 {
			t.Fatalf("daily limits = %+v", stats.Daily)
		}
		if stats.Weekly.UsedRequests != 1 || stats.Monthly.UsedRequests != 1 {
			t.Fatalf("week/month usage = %+v / %+v", stats.Weekly, stats.Monthly)
		}
	})

	t.Run("unknown key is 404", func(t *testing.T) {
		recorder := harness.call(t, http.MethodGet, "/admin/api-keys/9999/stats", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d", recorder.Code)
		}
	})
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
