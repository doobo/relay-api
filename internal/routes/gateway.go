package routes

import (
	"net/http"
	"time"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/resilience"
	"relay-api/internal/router"
)

// Gateway serves the OpenAI-compatible API (/v1/*).
type Gateway struct {
	cfg      config.Config
	resolver *router.Resolver
	// breaker skips a provider for a cooldown after repeated upstream failures,
	// so a database outage at one gateway is not amplified by every request
	// (and every retry) still being sent to it.
	breaker *resilience.Breaker
}

// NewGateway builds the gateway routes.
func NewGateway(cfg config.Config, resolver *router.Resolver) *Gateway {
	return &Gateway{
		cfg:      cfg,
		resolver: resolver,
		breaker: resilience.NewBreaker(
			cfg.UpstreamBreakerThreshold,
			time.Duration(cfg.UpstreamBreakerCooldownMs)*time.Millisecond,
		),
	}
}

// Register mounts the gateway routes.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", g.models)
	mux.HandleFunc("/v1/chat/completions", g.chat)
	mux.HandleFunc("/v1/responses", g.responses)
}

// models lists enabled model aliases in OpenAI format.
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	models, err := db.ListModels()
	if err != nil {
		internalError(w, "list models", err)
		return
	}
	// A key with an allowed-model list only discovers those models.
	var allowed []string
	if auth := middleware.APIAuthFrom(r.Context()); auth != nil {
		allowed = auth.Key.AllowedModelList()
	}
	data := []map[string]any{}
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		if len(allowed) > 0 && !containsString(allowed, model.Name) {
			continue
		}
		data = append(data, map[string]any{
			"id":       model.Name,
			"object":   "model",
			"created":  model.CreatedAt / 1000,
			"owned_by": "personal-gateway",
		})
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
