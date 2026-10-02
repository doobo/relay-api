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
	provider, err := db.CreateProvider("p1", "openai", "https://example.com", nil, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
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
			`{"modelName":"x","providerId":9999,"upstreamModel":"y"}`)
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

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
