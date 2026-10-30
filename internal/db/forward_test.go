package db

import (
	"path/filepath"
	"testing"

	"relay-api/internal/util"
)

// TestAPIConfigCacheCompilesKeyAndURL locks the per-config work the cache does
// once: the upstream URL is parsed, the stored API key is decrypted, and both
// are refreshed when a write invalidates the cache. A decryption or parse
// failure must be cached as an error instead of silently serving the config
// without a key or URL.
func TestAPIConfigCacheCompilesKeyAndURL(t *testing.T) {
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")

	if _, err := Open(filepath.Join(t.TempDir(), "forward.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	box, err := util.NewSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	SetAPIConfigSecretBox(box)
	t.Cleanup(func() { SetAPIConfigSecretBox(nil) })

	encrypted, err := box.Encrypt("sk-cached")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	created, err := CreateAPIConfig(NewAPIConfig{
		Name: "cached", Method: "POST", URL: "https://example.com/v1", APIKey: &encrypted,
		Route: "free", Enabled: true,
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
		if config == nil {
			t.Fatalf("config %q not found", path)
		}
		return config
	}

	config := resolve(t, "cached")
	if config.ParsedURL == nil || config.ParsedURLErr != nil {
		t.Fatalf("URL not compiled: url=%v err=%v", config.ParsedURL, config.ParsedURLErr)
	}
	if config.ParsedURL.Host != "example.com" || config.ParsedURL.Path != "/v1" {
		t.Fatalf("compiled URL = %s", config.ParsedURL)
	}
	if config.ParsedKey == nil || *config.ParsedKey != "sk-cached" || config.ParsedKeyErr != nil {
		t.Fatalf("key not compiled: key=%v err=%v", config.ParsedKey, config.ParsedKeyErr)
	}

	// Rotating the key must be visible on the next resolve, not only after a
	// restart.
	rotated, err := box.Encrypt("sk-rotated")
	if err != nil {
		t.Fatalf("encrypt rotated: %v", err)
	}
	if _, err := UpdateAPIConfig(created.ID, APIConfigPatch{APIKeySet: true, APIKey: &rotated}); err != nil {
		t.Fatalf("update key: %v", err)
	}
	config = resolve(t, "cached")
	if config.ParsedKey == nil || *config.ParsedKey != "sk-rotated" {
		t.Fatalf("rotated key not compiled: %v", config.ParsedKey)
	}

	// A ciphertext that cannot be opened keeps its error; dropping the key
	// would forward the request without an Authorization header.
	broken := "enc:v1:AAAA:BBBB:CCCC"
	if _, err := UpdateAPIConfig(created.ID, APIConfigPatch{APIKeySet: true, APIKey: &broken}); err != nil {
		t.Fatalf("update broken key: %v", err)
	}
	config = resolve(t, "cached")
	if config.ParsedKey != nil || config.ParsedKeyErr == nil {
		t.Fatalf("broken key: key=%v err=%v", config.ParsedKey, config.ParsedKeyErr)
	}

	// A URL that fails the static checks reports the same error the
	// per-request path used to produce.
	if _, err := CreateAPIConfig(NewAPIConfig{
		Name: "badurl", Method: "POST", URL: "ftp://example.com", Route: "free", Enabled: true,
	}); err != nil {
		t.Fatalf("create bad config: %v", err)
	}
	config = resolve(t, "badurl")
	if config.ParsedURL != nil || config.ParsedURLErr == nil {
		t.Fatalf("bad URL: url=%v err=%v", config.ParsedURL, config.ParsedURLErr)
	}
}

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

	// A config created after the cache is warm must be visible immediately.
	createdLate, err := CreateAPIConfig(NewAPIConfig{
		Name: "late", Method: "POST", URL: "http://example.com", Route: "free", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create late config: %v", err)
	}
	if config := resolve(t, "late"); config == nil || config.ID != createdLate.ID {
		t.Fatalf("config created after warm cache not served: %+v", config)
	}
	if removed, err := DeleteAPIConfig(createdLate.ID); err != nil || !removed {
		t.Fatalf("delete late config: removed=%v err=%v", removed, err)
	}

	// A delete must also drop it from the cache.
	if removed, err := DeleteAPIConfig(created.ID); err != nil || !removed {
		t.Fatalf("delete config: removed=%v err=%v", removed, err)
	}
	if config := resolve(t, "svc2/a"); config != nil {
		t.Fatalf("deleted config still served: %+v", config)
	}
}
