package routes

import (
	"net/http"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/router"
)

// Gateway serves the OpenAI-compatible API (/v1/*).
type Gateway struct {
	cfg      config.Config
	resolver *router.Resolver
}

// NewGateway builds the gateway routes.
func NewGateway(cfg config.Config, resolver *router.Resolver) *Gateway {
	return &Gateway{cfg: cfg, resolver: resolver}
}

// Register mounts the gateway routes.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", g.models)
	mux.HandleFunc("/v1/chat/completions", g.chat)
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
	data := []map[string]any{}
	for _, model := range models {
		if !model.Enabled {
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
