// Package router resolves a model alias to an ordered list of provider routes.
package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"relay-api/internal/db"
	"relay-api/internal/providers"
	"relay-api/internal/util"
)

var (
	// ErrModelNotFound means the alias has no enabled route at all.
	ErrModelNotFound = errors.New("model not found")
	// ErrProviderUnavailable means the alias' provider is missing/disabled.
	ErrProviderUnavailable = errors.New("provider unavailable")
)

// Route is one resolved upstream target for an alias.
type Route struct {
	Model         string
	Provider      db.ProviderRow
	UpstreamModel string
	Priority      int
}

// Resolver resolves aliases using the model_routes table when present, falling
// back to the single `models` entry.
type Resolver struct {
	secrets *util.SecretBox

	// Routing data only changes through the admin API, so resolved routes and
	// decrypted provider configs are cached per db.AIResourceGeneration(). A
	// write bumps the generation and the next lookup repopulates the maps,
	// taking the per-request model_routes/providers query and key decryption
	// off the relay path.
	mu      sync.Mutex
	gen     uint64
	routes  map[string][]Route
	configs map[int64]providers.Config
}

// NewResolver builds a resolver.
func NewResolver(secrets *util.SecretBox) *Resolver {
	return &Resolver{
		secrets: secrets,
		routes:  make(map[string][]Route),
		configs: make(map[int64]providers.Config),
	}
}

// refreshLocked drops cached routing data when the database generation moved.
// Caller holds r.mu.
func (r *Resolver) refreshLocked(gen uint64) {
	if r.gen == gen {
		return
	}
	r.gen = gen
	r.routes = make(map[string][]Route)
	r.configs = make(map[int64]providers.Config)
}

// Resolve returns the ordered routes for an alias (best priority first). The
// returned slice is shared and must be treated as read-only.
func (r *Resolver) Resolve(modelName string) ([]Route, error) {
	gen := db.AIResourceGeneration()
	r.mu.Lock()
	r.refreshLocked(gen)
	if routes, ok := r.routes[modelName]; ok {
		r.mu.Unlock()
		return routes, nil
	}
	r.mu.Unlock()

	routes, err := r.resolveUncached(modelName)
	if err != nil {
		return nil, err
	}
	// Only successful, non-empty resolutions are cached: a client can send
	// arbitrary model names, and negative entries would grow without bound.
	if len(routes) > 0 {
		r.mu.Lock()
		if r.gen == gen {
			r.routes[modelName] = routes
		}
		r.mu.Unlock()
	}
	return routes, nil
}

// resolveUncached reads the route and provider rows for one alias.
func (r *Resolver) resolveUncached(modelName string) ([]Route, error) {
	routes, err := db.ListModelRoutesFor(modelName)
	if err != nil {
		return nil, err
	}
	if len(routes) > 0 {
		out := make([]Route, 0, len(routes))
		for _, route := range routes {
			provider, err := db.GetProvider(route.ProviderID)
			if err != nil {
				return nil, err
			}
			if provider == nil || !provider.Enabled {
				continue
			}
			out = append(out, Route{
				Model:         modelName,
				Provider:      *provider,
				UpstreamModel: route.UpstreamModel,
				Priority:      route.Priority,
			})
		}
		return out, nil
	}

	model, err := db.GetModelByName(modelName)
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, ErrModelNotFound
	}
	provider, err := db.GetProvider(model.ProviderID)
	if err != nil {
		return nil, err
	}
	if provider == nil || !provider.Enabled {
		return nil, ErrProviderUnavailable
	}
	return []Route{{
		Model:         modelName,
		Provider:      *provider,
		UpstreamModel: model.UpstreamModel,
		Priority:      model.Priority,
	}}, nil
}

// ProviderConfig decrypts the provider key for upstream calls. It fails loudly
// rather than sending ciphertext as a bearer token. Decrypted configs are
// cached per provider and generation.
func (r *Resolver) ProviderConfig(route Route, requestTimeoutMs int) (providers.Config, error) {
	gen := db.AIResourceGeneration()
	r.mu.Lock()
	r.refreshLocked(gen)
	cfg, ok := r.configs[route.Provider.ID]
	r.mu.Unlock()
	if ok {
		cfg.RequestTimeoutMs = requestTimeoutMs
		return cfg, nil
	}

	// A caller may still hold a Route resolved before the last write. Re-read
	// the provider row on a cache miss so the first request after a change
	// never sends the old key, base URL or headers upstream.
	fresh := route
	if provider, err := db.GetProvider(route.Provider.ID); err != nil {
		return providers.Config{}, err
	} else if provider != nil {
		fresh.Provider = *provider
	}

	cfg, err := r.providerConfigUncached(fresh, requestTimeoutMs)
	if err != nil {
		return providers.Config{}, err
	}
	r.mu.Lock()
	if r.gen == gen {
		r.configs[route.Provider.ID] = cfg
	}
	r.mu.Unlock()
	return cfg, nil
}

// providerConfigUncached decodes headers and decrypts the stored API key.
func (r *Resolver) providerConfigUncached(route Route, requestTimeoutMs int) (providers.Config, error) {
	cfg := providers.Config{
		ID:               route.Provider.ID,
		Name:             route.Provider.Name,
		Type:             route.Provider.Type,
		BaseURL:          route.Provider.BaseURL,
		Headers:          parseProviderHeaders(route.Provider.Headers),
		RequestTimeoutMs: requestTimeoutMs,
	}
	if route.Provider.APIKey != nil && *route.Provider.APIKey != "" {
		plaintext, err := r.secrets.Decrypt(*route.Provider.APIKey)
		if err != nil {
			return providers.Config{}, fmt.Errorf(
				"stored API key for provider '%s' cannot be decrypted - re-enter it in the admin UI (encryption key lost or changed)",
				route.Provider.Name,
			)
		}
		cfg.APIKey = &plaintext
	}
	return cfg, nil
}

// parseProviderHeaders decodes the stored custom-header JSON object.
func parseProviderHeaders(raw *string) map[string]string {
	if raw == nil || *raw == "" {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(*raw), &headers); err != nil {
		return nil
	}
	return headers
}
