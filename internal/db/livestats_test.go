package db

import (
	"path/filepath"
	"testing"
)

func TestLiveStatsTrackPersistedUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "livestats.db")
	if _, err := Open(path); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = Close() }()
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	model, provider := "gpt-live", "provider-live"
	latency := int64(100)
	okStatus := 200
	badStatus := 500
	configID := int64(7)
	records := []UsageRecord{
		{RequestID: "a1", Kind: "ai", Model: &model, Provider: &provider, InputTokens: 3, OutputTokens: 4, LatencyMs: &latency, Status: &okStatus},
		{RequestID: "a2", Kind: "ai", Model: &model, Provider: &provider, InputTokens: 5, OutputTokens: 5, LatencyMs: &latency, Status: &badStatus},
		{RequestID: "f1", Kind: "api", APIConfigID: &configID, LatencyMs: &latency, Status: &okStatus},
	}
	for _, record := range records {
		if err := RecordUsage(record); err != nil {
			t.Fatalf("record usage: %v", err)
		}
	}

	today, err := TodayStatsSince()
	if err != nil {
		t.Fatalf("today stats: %v", err)
	}
	if today.Requests != 3 || today.Tokens != 17 || today.Errors != 1 || today.AvgLatencyMs != 100 {
		t.Fatalf("unexpected today stats: %+v", today)
	}

	models, err := ModelStatsToday()
	if err != nil {
		t.Fatalf("model stats: %v", err)
	}
	if len(models) != 1 || models[0].Model != model || models[0].Requests != 2 || models[0].Tokens != 17 {
		t.Fatalf("unexpected model stats: %+v", models)
	}

	providers, err := ProviderStatsToday()
	if err != nil {
		t.Fatalf("provider stats: %v", err)
	}
	if len(providers) != 1 || providers[0].Provider != provider || providers[0].Requests != 2 || providers[0].Errors != 1 {
		t.Fatalf("unexpected provider stats: %+v", providers)
	}

	// The SQL fallback must produce the same numbers when the live counters are
	// unavailable (for example after a failed seed).
	resetLiveStats()
	defer initLiveStats()
	fallback, err := TodayStatsSince()
	if err != nil {
		t.Fatalf("fallback today stats: %v", err)
	}
	if fallback.Requests != today.Requests || fallback.Tokens != today.Tokens || fallback.Errors != today.Errors {
		t.Fatalf("SQL fallback = %+v, live = %+v", fallback, today)
	}
}

func TestConfigStatsCacheServesCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configstats.db")
	if _, err := Open(path); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = Close() }()
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	configID := int64(3)
	status := 200
	latency := int64(40)
	if err := RecordUsage(UsageRecord{RequestID: "c1", Kind: "api", APIConfigID: &configID, Status: &status, LatencyMs: &latency}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	first, err := APIConfigStatsAll()
	if err != nil {
		t.Fatalf("config stats: %v", err)
	}
	if len(first) != 1 || first[0].Requests != 1 {
		t.Fatalf("unexpected config stats: %+v", first)
	}

	// Mutating the returned slice must not corrupt the cache.
	first[0].Requests = 999
	second, err := APIConfigStatsAll()
	if err != nil {
		t.Fatalf("config stats again: %v", err)
	}
	if second[0].Requests != 1 {
		t.Fatalf("cache was mutated through its result: %+v", second)
	}

	one, err := APIConfigStatsFor(configID)
	if err != nil {
		t.Fatalf("config stats for: %v", err)
	}
	if one.Requests != 1 || one.APIConfigID != configID {
		t.Fatalf("unexpected single config stats: %+v", one)
	}
}
