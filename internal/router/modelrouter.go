// Package router resolves a model alias to an ordered list of provider routes.
package router

import (
	"errors"
	"fmt"

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
}

// NewResolver builds a resolver.
func NewResolver(secrets *util.SecretBox) *Resolver {
	return &Resolver{secrets: secrets}
}

// Resolve returns the ordered routes for an alias (best priority first).
func (r *Resolver) Resolve(modelName string) ([]Route, error) {
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
// rather than sending ciphertext as a bearer token.
func (r *Resolver) ProviderConfig(route Route, requestTimeoutMs int) (providers.Config, error) {
	cfg := providers.Config{
		ID:               route.Provider.ID,
		Name:             route.Provider.Name,
		Type:             route.Provider.Type,
		BaseURL:          route.Provider.BaseURL,
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
