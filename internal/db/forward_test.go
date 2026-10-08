package db

import (
	"path/filepath"
	"testing"
)

// TestResolveAPIConfigCacheInvalidation locks the behaviour that makes the
// in-memory config cache safe: every api_configs write must invalidate it, so a
// change is visible to the very next resolve without a restart.
func TestResolveAPIConfigCacheInvalidation(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "forward.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	created, err := CreateAPIConfig(NewAPIConfig{
		Name: "svc/**", Method: "POST", URL: "http://example.com", Route: "free", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	resolve := func(t *testing.T, path string) *APIConfigRow {
		t.Helper()
		config, _, err := ResolveAPIConfig(path, "free")
		if err != nil {
			t.Fatalf("resolve %q: %v", path, err)
		}
		return config
	}

	// Warm the cache; the wildcard must resolve and hand back the tail.
	if config, rest, err := ResolveAPIConfig("svc/a", "free"); err != nil || config == nil || rest != "a" {
		t.Fatalf("initial resolve: config=%v rest=%q err=%v", config, rest, err)
	}

	// A rename must be visible immediately, without a restart.
	renamed := "svc2/**"
	if _, err := UpdateAPIConfig(created.ID, APIConfigPatch{Name: &renamed}); err != nil {
		t.Fatalf("update config: %v", err)
	}
	if config := resolve(t, "svc/a"); config != nil {
		t.Fatalf("stale config served after rename: %+v", config)
	}
	if config := resolve(t, "svc2/a"); config == nil {
		t.Fatal("renamed config not served after update")
	}

	// A delete must also drop it from the cache.
	if removed, err := DeleteAPIConfig(created.ID); err != nil || !removed {
		t.Fatalf("delete config: removed=%v err=%v", removed, err)
	}
	if config := resolve(t, "svc2/a"); config != nil {
		t.Fatalf("deleted config still served: %+v", config)
	}
}
