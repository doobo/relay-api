// Package router resolves a model alias to an ordered list of provider routes.
package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	//
	// Hits take the read lock only; the write lock is reserved for refreshes
	// and cache fills so concurrent AI requests do not serialise on every
	// resolution.
	mu      sync.RWMutex
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
// Caller holds r.mu for writing.
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
	r.mu.RLock()
	if r.gen == gen {
		if routes, ok := r.routes[modelName]; ok {
			r.mu.RUnlock()
			return routes, nil
		}
	}
	r.mu.RUnlock()

	// A miss may also mean the routing generation moved. Refresh under the
	// write lock so the cache is usable again, and re-check in case another
	// request filled this alias while we were between locks.
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
			if cached, ok := r.routes[modelName]; ok {
				routes = cached
			} else {
				r.routes[modelName] = routes
			}
		}
		r.mu.Unlock()
	}
	return routes, nil
}

// resolveUncached reads the route and provider rows for one alias.
func (r *Resolver) resolveUncached(modelName string) ([]Route, error) {
	// The models row is the alias registry and the enable/disable switch;
	// model_routes only supplies optional failover targets for that alias. The
	// alias is therefore checked first: a model that is disabled (or deleted)
	// must stop resolving even when model_routes rows still reference it,
	// otherwise disabling or deleting a model in the admin UI would leave it
	// callable through /v1.
	model, err := db.GetModelByName(modelName)
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, ErrModelNotFound
	}

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
func (r *Resolver) ProviderConfig(route Route, requestTimeoutMs, headerTimeoutMs int) (providers.Config, error) {
	gen := db.AIResourceGeneration()
	r.mu.RLock()
	var cfg providers.Config
	ok := false
	if r.gen == gen {
		cfg, ok = r.configs[route.Provider.ID]
	}
	r.mu.RUnlock()
	if ok {
		cfg.RequestTimeoutMs = requestTimeoutMs
		cfg.HeaderTimeoutMs = headerTimeoutMs
		return cfg, nil
	}

	// A miss may also mean the routing generation moved; refresh so the cache
	// is usable again and re-check another request's fill before going to
	// SQLite.
	r.mu.Lock()
	r.refreshLocked(gen)
	if cached, cachedOK := r.configs[route.Provider.ID]; cachedOK {
		r.mu.Unlock()
		cached.RequestTimeoutMs = requestTimeoutMs
		cached.HeaderTimeoutMs = headerTimeoutMs
		return cached, nil
	}
	r.mu.Unlock()

	// A caller may still hold a Route resolved before the last write. Re-read
	// the provider row on a cache miss so the first request after a change
	// never sends the old key, base URL or headers upstream.
	fresh := route
	if provider, err := db.GetProvider(route.Provider.ID); err != nil {
		return providers.Config{}, err
	} else if provider != nil {
		fresh.Provider = *provider
	}

	cfg, err := r.providerConfigUncached(fresh, requestTimeoutMs, headerTimeoutMs)
	if err != nil {
		return providers.Config{}, err
	}
	r.mu.Lock()
	if r.gen == gen {
		if cached, cachedOK := r.configs[route.Provider.ID]; cachedOK {
			cfg = cached
		} else {
			r.configs[route.Provider.ID] = cfg
		}
	}
	r.mu.Unlock()
	cfg.RequestTimeoutMs = requestTimeoutMs
	cfg.HeaderTimeoutMs = headerTimeoutMs
	return cfg, nil
}

// providerConfigUncached decodes headers and decrypts the stored API key.
func (r *Resolver) providerConfigUncached(route Route, requestTimeoutMs, headerTimeoutMs int) (providers.Config, error) {
	headers := parseProviderHeaders(route.Provider.Headers)
	cfg := providers.Config{
		ID:               route.Provider.ID,
		Name:             route.Provider.Name,
		Type:             route.Provider.Type,
		BaseURL:          route.Provider.BaseURL,
		BaseURLParsed:    parseProviderBaseURL(route.Provider.BaseURL),
		Headers:          headers,
		HeaderTemplates:  providers.CompileHeaders(headers),
		ReasoningReplay:  route.Provider.ReasoningReplay,
		RequestTimeoutMs: requestTimeoutMs,
		HeaderTimeoutMs:  headerTimeoutMs,
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

// parseProviderBaseURL parses the base URL once when the provider config is
// cached. A parse error leaves it nil so the provider falls back to the string
// path and reports the same error as before on use.
func parseProviderBaseURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return parsed
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
