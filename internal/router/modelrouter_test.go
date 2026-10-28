package router

import (
	"path/filepath"
	"testing"

	"relay-api/internal/db"
	"relay-api/internal/util"
)

func newTestResolver(t *testing.T) *Resolver {
	t.Helper()
	t.Setenv("SECRET_KEY_FILE", filepath.Join(t.TempDir(), ".secret-key"))
	t.Setenv("SECRET_ENCRYPTION_KEY", "")
	if _, err := db.Open(filepath.Join(t.TempDir(), "router.db")); err != nil {
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
	return NewResolver(secrets)
}

// TestResolverCacheInvalidation locks the cache contract: resolved routes and
// decrypted provider configs are served from memory, and an admin write is
// visible on the very next resolve without a restart.
func TestResolverCacheInvalidation(t *testing.T) {
	resolver := newTestResolver(t)

	encrypted, err := resolver.secrets.Encrypt("sk-first")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	provider, err := db.CreateProvider("p", "openai", "https://first.example", &encrypted, nil, false, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	model, err := db.CreateModel("alias", provider.ID, "upstream-first", true, 0)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}

	routes, err := resolver.Resolve("alias")
	if err != nil || len(routes) != 1 {
		t.Fatalf("initial resolve: routes=%v err=%v", routes, err)
	}
	if routes[0].UpstreamModel != "upstream-first" {
		t.Fatalf("initial upstream = %q", routes[0].UpstreamModel)
	}

	cfg, err := resolver.ProviderConfig(routes[0], 1000, 0)
	if err != nil {
		t.Fatalf("initial provider config: %v", err)
	}
	if cfg.BaseURL != "https://first.example" || cfg.APIKey == nil || *cfg.APIKey != "sk-first" {
		t.Fatalf("initial config = %+v", cfg)
	}

	// A model update must reach the resolver without a restart.
	upstream := "upstream-second"
	if _, err := db.UpdateModel(model.ID, db.ModelPatch{UpstreamModel: &upstream}); err != nil {
		t.Fatalf("update model: %v", err)
	}
	routes, err = resolver.Resolve("alias")
	if err != nil || len(routes) != 1 {
		t.Fatalf("resolve after model update: routes=%v err=%v", routes, err)
	}
	if routes[0].UpstreamModel != "upstream-second" {
		t.Fatalf("stale upstream after update = %q", routes[0].UpstreamModel)
	}

	// A provider update must refresh the cached decrypted config too.
	baseURL := "https://second.example"
	reencrypted, err := resolver.secrets.Encrypt("sk-second")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := db.UpdateProvider(provider.ID, db.ProviderPatch{
		BaseURL: &baseURL, APIKeySet: true, APIKey: &reencrypted,
	}); err != nil {
		t.Fatalf("update provider: %v", err)
	}
	cfg, err = resolver.ProviderConfig(routes[0], 1000, 0)
	if err != nil {
		t.Fatalf("provider config after update: %v", err)
	}
	if cfg.BaseURL != "https://second.example" || cfg.APIKey == nil || *cfg.APIKey != "sk-second" {
		t.Fatalf("stale config after update = %+v", cfg)
	}
}
