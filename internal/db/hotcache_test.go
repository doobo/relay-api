package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openHotCacheTestDB(t *testing.T) {
	t.Helper()
	if _, err := Open(filepath.Join(t.TempDir(), "hotcache.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestAPIKeyCacheInvalidation locks the behaviour the auth path relies on: a
// lookup is cached, and every api_keys write is visible on the next lookup.
func TestAPIKeyCacheInvalidation(t *testing.T) {
	openHotCacheTestDB(t)

	key, err := CreateAPIKey(NewAPIKey{
		Name: "original", Prefix: "sk-test", KeyHash: "hash-1", Scope: "both",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	first, err := GetAPIKeyByHash("hash-1")
	if err != nil || first == nil {
		t.Fatalf("first lookup: key=%v err=%v", first, err)
	}
	// Warm the cache again to prove hits work.
	if cached, err := GetAPIKeyByHash("hash-1"); err != nil || cached.Name != "original" {
		t.Fatalf("cached lookup: key=%v err=%v", cached, err)
	}

	renamed := "renamed"
	if _, err := UpdateAPIKey(key.ID, APIKeyPatch{Name: &renamed}); err != nil {
		t.Fatalf("update key: %v", err)
	}
	if updated, err := GetAPIKeyByHash("hash-1"); err != nil || updated.Name != "renamed" {
		t.Fatalf("stale key served after update: key=%v err=%v", updated, err)
	}

	if removed, err := DeleteAPIKey(key.ID); err != nil || !removed {
		t.Fatalf("delete key: removed=%v err=%v", removed, err)
	}
	if gone, err := GetAPIKeyByHash("hash-1"); err != nil || gone != nil {
		t.Fatalf("deleted key still served: key=%v err=%v", gone, err)
	}
}

// TestQuotaCountersTrackUsage verifies the in-memory quota counters: they load
// once from SQLite and then advance as records are recorded, without a query
// per check and without double counting after a batch flush.
func TestQuotaCountersTrackUsage(t *testing.T) {
	openHotCacheTestDB(t)

	key, err := CreateAPIKey(NewAPIKey{
		Name: "quota", Prefix: "sk-q", KeyHash: "hash-q", Scope: "both",
		DailyRequestLimit: 2,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	usage, err := APIKeyQuotaUsageCached(key.ID)
	if err != nil {
		t.Fatalf("initial usage: %v", err)
	}
	if usage.DayRequests != 0 || usage.DayTokens != 0 {
		t.Fatalf("initial usage = %+v, want zero", usage)
	}

	keyID := key.ID
	if err := RecordUsage(UsageRecord{
		RequestID: "req-1", APIKeyID: &keyID, Kind: "ai", InputTokens: 3, OutputTokens: 4,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	RecordUsageAsync(UsageRecord{
		RequestID: "req-2", APIKeyID: &keyID, Kind: "ai", InputTokens: 1, OutputTokens: 1,
	})
	if err := FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	usage, err = APIKeyQuotaUsageCached(key.ID)
	if err != nil {
		t.Fatalf("cached usage: %v", err)
	}
	if usage.DayRequests != 2 || usage.DayTokens != 9 {
		t.Fatalf("usage after records = %+v, want 2 requests / 9 tokens", usage)
	}
	if usage.MonthRequests != 2 || usage.WeekRequests != 2 {
		t.Fatalf("period counters = %+v, want the same request in week/month", usage)
	}
}

// TestAIResourceGenerationMovesOnWrites makes sure the resolver cache is
// invalidated by every relevant admin write.
func TestAIResourceGenerationMovesOnWrites(t *testing.T) {
	openHotCacheTestDB(t)

	before := AIResourceGeneration()
	provider, err := CreateProvider("p", "openai", "http://example.com", nil, nil, false, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if AIResourceGeneration() == before {
		t.Fatal("provider create did not bump the routing generation")
	}
	before = AIResourceGeneration()

	model, err := CreateModel("m", provider.ID, "upstream", true, 0)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	if AIResourceGeneration() == before {
		t.Fatal("model create did not bump the routing generation")
	}
	before = AIResourceGeneration()

	if _, err := CreateModelRoute(NewModelRoute{
		ModelName: "m2", ProviderID: provider.ID, UpstreamModel: "u", Enabled: true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if AIResourceGeneration() == before {
		t.Fatal("route create did not bump the routing generation")
	}

	before = AIResourceGeneration()
	upstream := "upstream-2"
	if _, err := UpdateModel(model.ID, ModelPatch{UpstreamModel: &upstream}); err != nil {
		t.Fatalf("update model: %v", err)
	}
	if AIResourceGeneration() == before {
		t.Fatal("model update did not bump the routing generation")
	}
}

// TestMarkAPIKeyUsedIsFlushed verifies last_used_at is queued through the
// usage writer and visible after FlushUsage.
func TestMarkAPIKeyUsedIsFlushed(t *testing.T) {
	openHotCacheTestDB(t)

	key, err := CreateAPIKey(NewAPIKey{
		Name: "used", Prefix: "sk-u", KeyHash: "hash-u", Scope: "both",
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	MarkAPIKeyUsed(key.ID)
	if err := FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush usage: %v", err)
	}

	var lastUsed *int64
	if err := Get().QueryRow(`SELECT last_used_at FROM api_keys WHERE id = ?`, key.ID).Scan(&lastUsed); err != nil {
		t.Fatalf("query last_used_at: %v", err)
	}
	if lastUsed == nil {
		t.Fatal("last_used_at was not recorded")
	}
}
