package router

import (
	"errors"
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

// TestResolverHonorsModelEnableAndDelete locks the control-plane contract:
// model_routes only supplies failover targets for an alias, so disabling or
// deleting the alias must stop resolution even while route rows remain.
func TestResolverHonorsModelEnableAndDelete(t *testing.T) {
	resolver := newTestResolver(t)

	provider, err := db.CreateProvider("p", "openai", "https://example.com", nil, nil, false, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	model, err := db.CreateModel("alias", provider.ID, "upstream", true, 0)
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := db.CreateModelRoute(db.NewModelRoute{
		ModelName:     "alias",
		ProviderID:    provider.ID,
		UpstreamModel: "route-upstream",
		Priority:      1,
		Weight:        100,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}

	routes, err := resolver.Resolve("alias")
	if err != nil || len(routes) != 1 || routes[0].UpstreamModel != "route-upstream" {
		t.Fatalf("resolve with route: routes=%+v err=%v", routes, err)
	}

	// Disabling the alias must stop it, even though an enabled route remains.
	disabled := false
	if _, err := db.UpdateModel(model.ID, db.ModelPatch{Enabled: &disabled}); err != nil {
		t.Fatalf("disable model: %v", err)
	}
	if _, err := resolver.Resolve("alias"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("disabled model resolved: err=%v", err)
	}

	// Re-enabling restores routing. Deleting then removes the route rows too,
	// so the alias cannot survive as an orphan.
	enabled := true
	if _, err := db.UpdateModel(model.ID, db.ModelPatch{Enabled: &enabled}); err != nil {
		t.Fatalf("enable model: %v", err)
	}
	if routes, err := resolver.Resolve("alias"); err != nil || len(routes) != 1 {
		t.Fatalf("resolve after re-enable: routes=%+v err=%v", routes, err)
	}
	if _, err := db.DeleteModel(model.ID); err != nil {
		t.Fatalf("delete model: %v", err)
	}
	if _, err := resolver.Resolve("alias"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("deleted model resolved: err=%v", err)
	}
	if remaining, err := db.ListModelRoutesFor("alias"); err != nil || len(remaining) != 0 {
		t.Fatalf("orphan routes survived delete: %+v err=%v", remaining, err)
	}
}

// TestResolverRejectsRouteOnlyAlias pins that an alias defined only by a
// model_routes row (no models row) is not callable: it would otherwise be a
// hidden model the models list can neither show nor disable.
func TestResolverRejectsRouteOnlyAlias(t *testing.T) {
	resolver := newTestResolver(t)

	provider, err := db.CreateProvider("p", "openai", "https://example.com", nil, nil, false, true)
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := db.CreateModelRoute(db.NewModelRoute{
		ModelName:     "ghost",
		ProviderID:    provider.ID,
		UpstreamModel: "u",
		Priority:      1,
		Weight:        100,
		Enabled:       true,
	}); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if _, err := resolver.Resolve("ghost"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("route-only alias resolved: err=%v", err)
	}
}
