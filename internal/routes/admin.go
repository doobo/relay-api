package routes

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/util"
)

// maxAdminBodyBytes caps admin resource bodies (templates can be a few KB).
const maxAdminBodyBytes = 1 << 20

// AdminResources serves the /admin resource APIs (providers, models, model
// routes, api keys, api configs) plus the dashboard stats/usage/logs views.
// Read requests pass; writes require an admin (the caller mounts the subtree
// behind RequireAdminRole).
type AdminResources struct {
	secrets          *util.SecretBox
	logRetentionDays int
}

// NewAdminResources builds the resource handlers. logRetentionDays drives the
// manual POST /admin/logs/cleanup purge.
func NewAdminResources(secrets *util.SecretBox, logRetentionDays int) *AdminResources {
	return &AdminResources{secrets: secrets, logRetentionDays: logRetentionDays}
}

// Register mounts the resource routes.
func (h *AdminResources) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/stats", h.stats)
	mux.HandleFunc("/admin/usage", h.usage)
	mux.HandleFunc("/admin/logs", h.logs)
	mux.HandleFunc("/admin/logs/cleanup", h.logsCleanup)
	mux.HandleFunc("/admin/providers", h.providers)
	mux.HandleFunc("/admin/providers/{id}", h.providerByID)
	mux.HandleFunc("/admin/models", h.models)
	mux.HandleFunc("/admin/models/{id}", h.modelByID)
	mux.HandleFunc("/admin/model-routes", h.modelRoutes)
	mux.HandleFunc("/admin/model-routes/{id}", h.modelRouteByID)
	mux.HandleFunc("/admin/api-keys", h.apiKeys)
	mux.HandleFunc("/admin/api-keys/{id}", h.apiKeyByID)
	mux.HandleFunc("/admin/api-keys/{id}/configs", h.apiKeyConfigs)
	mux.HandleFunc("/admin/api-configs", h.apiConfigs)
	mux.HandleFunc("/admin/api-configs/{id}", h.apiConfigByID)
	mux.HandleFunc("/admin/api-configs/{id}/stats", h.apiConfigStats)
	mux.HandleFunc("/admin/api-configs/{id}/keys", h.apiConfigKeys)
}

// ----------------------------------------------------------------- body utils

// decodeBody reads a JSON object into dst and also returns its raw fields, so
// handlers can tell an absent field (keep) from an explicit null (clear).
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) (map[string]json.RawMessage, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		httperr.Write(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "invalid_json")
		return nil, false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		httperr.Write(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "invalid_json")
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &fields)
	return fields, true
}

func fieldPresent(fields map[string]json.RawMessage, key string) bool {
	_, ok := fields[key]
	return ok
}

// boolOr reports whether the raw field is JSON true/false.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// nullableString resolves an API-key style field: set=false means keep the
// current value; set=true with value=nil means clear it.
func nullableString(fields map[string]json.RawMessage, key string) (set bool, value *string) {
	raw, ok := fields[key]
	if !ok {
		return false, nil
	}
	if isJSONNull(raw) {
		return true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return true, nil
	}
	return true, &s
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}

// ----------------------------------------------------------------- providers

type providerDTO struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	BaseURL      string  `json:"base_url"`
	Enabled      int     `json:"enabled"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	HasAPIKey    bool    `json:"has_api_key"`
	APIKeyMasked *string `json:"api_key_masked"`
	APIKeyError  bool    `json:"api_key_error,omitempty"`
}

func (h *AdminResources) providerDTO(provider db.ProviderRow) providerDTO {
	dto := providerDTO{
		ID:        provider.ID,
		Name:      provider.Name,
		Type:      provider.Type,
		BaseURL:   provider.BaseURL,
		Enabled:   boolInt(provider.Enabled),
		CreatedAt: provider.CreatedAt,
		UpdatedAt: provider.UpdatedAt,
	}
	if provider.APIKey != nil && *provider.APIKey != "" {
		plaintext, err := h.secrets.Decrypt(*provider.APIKey)
		switch {
		case err != nil:
			// Encryption key lost/changed: surface an explicit state instead of
			// failing the whole list.
			masked := "(undecryptable)"
			dto.HasAPIKey = true
			dto.APIKeyError = true
			dto.APIKeyMasked = &masked
		case plaintext != "":
			masked := util.MaskSecret(plaintext)
			dto.HasAPIKey = true
			dto.APIKeyMasked = &masked
		}
	}
	return dto
}

func (h *AdminResources) providers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		providers, err := db.ListProviders()
		if err != nil {
			internalError(w, "list providers", err)
			return
		}
		data := make([]providerDTO, 0, len(providers))
		for _, provider := range providers {
			data = append(data, h.providerDTO(provider))
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
	case http.MethodPost:
		h.createProvider(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminResources) createProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string  `json:"name"`
		Type    string  `json:"type"`
		BaseURL string  `json:"baseUrl"`
		APIKey  *string `json:"apiKey"`
		Enabled *bool   `json:"enabled"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.Name == "" || body.BaseURL == "" {
		invalidRequest(w, "name and baseUrl are required")
		return
	}
	if !validProviderType(body.Type) {
		invalidRequest(w, "type must be openai, anthropic or compatible")
		return
	}
	if _, err := util.ValidateUpstreamURL(body.BaseURL); err != nil {
		invalidRequest(w, err.Error())
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	encrypted, err := h.encryptOptional(body.APIKey)
	if err != nil {
		internalError(w, "encrypt provider key", err)
		return
	}
	provider, err := db.CreateProvider(body.Name, body.Type, body.BaseURL, encrypted, enabled)
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "create provider", err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, h.providerDTO(*provider))
}

func (h *AdminResources) providerByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		provider, err := db.GetProvider(id)
		if err != nil {
			internalError(w, "get provider", err)
			return
		}
		if provider == nil {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, h.providerDTO(*provider))
	case http.MethodPut:
		h.updateProvider(w, r, id)
	case http.MethodDelete:
		h.deleteProvider(w, r, id)
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *AdminResources) updateProvider(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Name    *string `json:"name"`
		Type    *string `json:"type"`
		BaseURL *string `json:"baseUrl"`
		Enabled *bool   `json:"enabled"`
	}
	fields, ok := decodeBody(w, r, &body)
	if !ok {
		return
	}
	if body.Name != nil && *body.Name == "" {
		invalidRequest(w, "name must not be empty")
		return
	}
	if body.Type != nil && !validProviderType(*body.Type) {
		invalidRequest(w, "type must be openai, anthropic or compatible")
		return
	}
	if body.BaseURL != nil {
		if _, err := util.ValidateUpstreamURL(*body.BaseURL); err != nil {
			invalidRequest(w, err.Error())
			return
		}
	}
	patch := db.ProviderPatch{
		Name:    body.Name,
		Type:    body.Type,
		BaseURL: body.BaseURL,
		Enabled: body.Enabled,
	}
	if set, value := nullableString(fields, "apiKey"); set {
		patch.APIKeySet = true
		encrypted, err := h.encryptOptional(value)
		if err != nil {
			internalError(w, "encrypt provider key", err)
			return
		}
		patch.APIKey = encrypted
	}
	provider, err := db.UpdateProvider(id, patch)
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "update provider", err)
		return
	}
	if provider == nil {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, h.providerDTO(*provider))
}

func (h *AdminResources) deleteProvider(w http.ResponseWriter, r *http.Request, id int64) {
	provider, err := db.GetProvider(id)
	if err != nil {
		internalError(w, "get provider", err)
		return
	}
	if provider == nil {
		httperr.NotFound(w)
		return
	}

	// A provider is referenced by model aliases (models.provider_id) and by
	// failover routes, so say what is in the way instead of surfacing a
	// constraint error.
	dependents, err := db.ListModelNamesByProvider(id)
	if err != nil {
		internalError(w, "list provider dependents", err)
		return
	}
	if len(dependents) > 0 {
		shown := dependents
		more := ""
		if len(shown) > 5 {
			more = ", +" + strconv.Itoa(len(shown)-5) + " more"
			shown = shown[:5]
		}
		plural := "es"
		if len(dependents) == 1 {
			plural = ""
		}
		conflict(
			w,
			"Cannot delete provider '"+provider.Name+"': "+strconv.Itoa(len(dependents))+" model alias"+plural+
				" still use it ("+strings.Join(shown, ", ")+more+"). Point them at another provider or delete them first.",
			"provider_in_use",
		)
		return
	}

	removed, err := db.DeleteProvider(id)
	if err != nil {
		internalError(w, "delete provider", err)
		return
	}
	if !removed {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func validProviderType(kind string) bool {
	return kind == "openai" || kind == "anthropic" || kind == "compatible"
}

// encryptOptional encrypts a non-empty secret; nil/empty stays nil.
func (h *AdminResources) encryptOptional(value *string) (*string, error) {
	if value == nil || *value == "" {
		return nil, nil
	}
	encrypted, err := h.secrets.Encrypt(*value)
	if err != nil {
		return nil, err
	}
	return &encrypted, nil
}

// -------------------------------------------------------------------- models

type modelDTO struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	ProviderID    int64  `json:"provider_id"`
	UpstreamModel string `json:"upstream_model"`
	Enabled       int    `json:"enabled"`
	Priority      int    `json:"priority"`
	CreatedAt     int64  `json:"created_at"`
}

func toModelDTO(model db.ModelRow) modelDTO {
	return modelDTO{
		ID:            model.ID,
		Name:          model.Name,
		ProviderID:    model.ProviderID,
		UpstreamModel: model.UpstreamModel,
		Enabled:       boolInt(model.Enabled),
		Priority:      model.Priority,
		CreatedAt:     model.CreatedAt,
	}
}

func (h *AdminResources) models(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		models, err := db.ListModels()
		if err != nil {
			internalError(w, "list models", err)
			return
		}
		data := make([]modelDTO, 0, len(models))
		for _, model := range models {
			data = append(data, toModelDTO(model))
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
	case http.MethodPost:
		h.createModel(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminResources) createModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string `json:"name"`
		ProviderID    int64  `json:"providerId"`
		UpstreamModel string `json:"upstreamModel"`
		Enabled       *bool  `json:"enabled"`
		Priority      *int   `json:"priority"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.Name == "" || body.UpstreamModel == "" || body.ProviderID <= 0 {
		invalidRequest(w, "name, providerId and upstreamModel are required")
		return
	}
	provider, err := db.GetProvider(body.ProviderID)
	if err != nil {
		internalError(w, "get provider", err)
		return
	}
	if provider == nil {
		invalidRequest(w, "Provider "+strconv.FormatInt(body.ProviderID, 10)+" does not exist")
		return
	}
	duplicate, err := db.FindModelByName(body.Name, 0)
	if err != nil {
		internalError(w, "check duplicate model", err)
		return
	}
	if duplicate != nil {
		conflict(w, "Model alias '"+body.Name+"' already exists", "duplicate_model")
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	priority := 100
	if body.Priority != nil {
		priority = *body.Priority
	}
	model, err := db.CreateModel(body.Name, body.ProviderID, body.UpstreamModel, enabled, priority)
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "create model", err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, toModelDTO(*model))
}

func (h *AdminResources) modelByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		model, err := db.GetModel(id)
		if err != nil {
			internalError(w, "get model", err)
			return
		}
		if model == nil {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, toModelDTO(*model))
	case http.MethodPut:
		h.updateModel(w, r, id)
	case http.MethodDelete:
		removed, err := db.DeleteModel(id)
		if err != nil {
			internalError(w, "delete model", err)
			return
		}
		if !removed {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *AdminResources) updateModel(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Name          *string `json:"name"`
		ProviderID    *int64  `json:"providerId"`
		UpstreamModel *string `json:"upstreamModel"`
		Enabled       *bool   `json:"enabled"`
		Priority      *int    `json:"priority"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.ProviderID != nil {
		provider, err := db.GetProvider(*body.ProviderID)
		if err != nil {
			internalError(w, "get provider", err)
			return
		}
		if provider == nil {
			invalidRequest(w, "Provider "+strconv.FormatInt(*body.ProviderID, 10)+" does not exist")
			return
		}
	}
	if body.Name != nil {
		duplicate, err := db.FindModelByName(*body.Name, id)
		if err != nil {
			internalError(w, "check duplicate model", err)
			return
		}
		if duplicate != nil {
			conflict(w, "Model alias '"+*body.Name+"' already exists", "duplicate_model")
			return
		}
	}
	model, err := db.UpdateModel(id, db.ModelPatch{
		Name:          body.Name,
		ProviderID:    body.ProviderID,
		UpstreamModel: body.UpstreamModel,
		Enabled:       body.Enabled,
		Priority:      body.Priority,
	})
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "update model", err)
		return
	}
	if model == nil {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, toModelDTO(*model))
}

// ------------------------------------------------------------------ api keys

type apiKeyDTO struct {
	ID                  int64   `json:"id"`
	Name                string  `json:"name"`
	Prefix              string  `json:"prefix"`
	Scope               string  `json:"scope"`
	AllowedModels       *string `json:"allowed_models"`
	AllowedAPIs         *string `json:"allowed_apis"`
	RateLimit           int     `json:"rate_limit"`
	DailyRequestLimit   int     `json:"daily_request_limit"`
	DailyTokenLimit     int     `json:"daily_token_limit"`
	WeeklyRequestLimit  int     `json:"weekly_request_limit"`
	WeeklyTokenLimit    int     `json:"weekly_token_limit"`
	MonthlyRequestLimit int     `json:"monthly_request_limit"`
	MonthlyTokenLimit   int     `json:"monthly_token_limit"`
	Enabled             int     `json:"enabled"`
	ExpiresAt           *int64  `json:"expires_at"`
	CreatedAt           int64   `json:"created_at"`
	LastUsedAt          *int64  `json:"last_used_at"`
	Key                 string  `json:"key,omitempty"`
}

func toAPIKeyDTO(key db.APIKeyRow) apiKeyDTO {
	return apiKeyDTO{
		ID:                  key.ID,
		Name:                key.Name,
		Prefix:              key.Prefix,
		Scope:               key.Scope,
		AllowedModels:       key.AllowedModels,
		AllowedAPIs:         key.AllowedAPIs,
		RateLimit:           key.RateLimit,
		DailyRequestLimit:   key.DailyRequestLimit,
		DailyTokenLimit:     key.DailyTokenLimit,
		WeeklyRequestLimit:  key.WeeklyRequestLimit,
		WeeklyTokenLimit:    key.WeeklyTokenLimit,
		MonthlyRequestLimit: key.MonthlyRequestLimit,
		MonthlyTokenLimit:   key.MonthlyTokenLimit,
		Enabled:             boolInt(key.Enabled),
		ExpiresAt:           key.ExpiresAt,
		CreatedAt:           key.CreatedAt,
		LastUsedAt:          key.LastUsedAt,
	}
}

func (h *AdminResources) apiKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys, err := db.ListAPIKeys()
		if err != nil {
			internalError(w, "list api keys", err)
			return
		}
		data := make([]apiKeyDTO, 0, len(keys))
		for _, key := range keys {
			data = append(data, toAPIKeyDTO(key))
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
	case http.MethodPost:
		h.createAPIKey(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminResources) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name                string   `json:"name"`
		Scope               *string  `json:"scope"`
		AllowedModels       []string `json:"allowedModels"`
		AllowedAPIs         []string `json:"allowedApis"`
		RateLimit           *int     `json:"rateLimit"`
		ExpiresInDays       *int     `json:"expiresInDays"`
		DailyRequestLimit   *int     `json:"dailyRequestLimit"`
		DailyTokenLimit     *int     `json:"dailyTokenLimit"`
		WeeklyRequestLimit  *int     `json:"weeklyRequestLimit"`
		WeeklyTokenLimit    *int     `json:"weeklyTokenLimit"`
		MonthlyRequestLimit *int     `json:"monthlyRequestLimit"`
		MonthlyTokenLimit   *int     `json:"monthlyTokenLimit"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.Name == "" {
		invalidRequest(w, "name is required")
		return
	}
	scope := "both"
	if body.Scope != nil {
		if !validScope(*body.Scope) {
			invalidRequest(w, "scope must be ai, api or both")
			return
		}
		scope = *body.Scope
	}
	rateLimit := 60
	if body.RateLimit != nil && *body.RateLimit > 0 {
		rateLimit = *body.RateLimit
	}

	fullKey := "sk-" + util.NewAPISecret()
	input := db.NewAPIKey{
		Name:                body.Name,
		Prefix:              fullKey[:8],
		KeyHash:             util.SHA256Hex(fullKey),
		Scope:               scope,
		AllowedModels:       jsonStringSlice(body.AllowedModels),
		AllowedAPIs:         jsonStringSlice(body.AllowedAPIs),
		RateLimit:           rateLimit,
		DailyRequestLimit:   intOr(body.DailyRequestLimit, 0),
		DailyTokenLimit:     intOr(body.DailyTokenLimit, 0),
		WeeklyRequestLimit:  intOr(body.WeeklyRequestLimit, 0),
		WeeklyTokenLimit:    intOr(body.WeeklyTokenLimit, 0),
		MonthlyRequestLimit: intOr(body.MonthlyRequestLimit, 0),
		MonthlyTokenLimit:   intOr(body.MonthlyTokenLimit, 0),
	}
	if body.ExpiresInDays != nil && *body.ExpiresInDays > 0 {
		expiresAt := nowMs() + int64(*body.ExpiresInDays)*86_400_000
		input.ExpiresAt = &expiresAt
	}
	key, err := db.CreateAPIKey(input)
	if err != nil {
		internalError(w, "create api key", err)
		return
	}
	dto := toAPIKeyDTO(*key)
	// The full key is shown exactly once, at creation.
	dto.Key = fullKey
	httperr.WriteJSON(w, http.StatusCreated, dto)
}

func (h *AdminResources) apiKeyByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		key, err := db.GetAPIKey(id)
		if err != nil {
			internalError(w, "get api key", err)
			return
		}
		if key == nil {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, toAPIKeyDTO(*key))
	case http.MethodPut:
		h.updateAPIKey(w, r, id)
	case http.MethodDelete:
		removed, err := db.DeleteAPIKey(id)
		if err != nil {
			internalError(w, "delete api key", err)
			return
		}
		if !removed {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *AdminResources) updateAPIKey(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Name                *string `json:"name"`
		Scope               *string `json:"scope"`
		RateLimit           *int    `json:"rateLimit"`
		Enabled             *bool   `json:"enabled"`
		DailyRequestLimit   *int    `json:"dailyRequestLimit"`
		DailyTokenLimit     *int    `json:"dailyTokenLimit"`
		WeeklyRequestLimit  *int    `json:"weeklyRequestLimit"`
		WeeklyTokenLimit    *int    `json:"weeklyTokenLimit"`
		MonthlyRequestLimit *int    `json:"monthlyRequestLimit"`
		MonthlyTokenLimit   *int    `json:"monthlyTokenLimit"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.Name != nil && *body.Name == "" {
		invalidRequest(w, "name must not be empty")
		return
	}
	if body.Scope != nil && !validScope(*body.Scope) {
		invalidRequest(w, "scope must be ai, api or both")
		return
	}
	if body.RateLimit != nil && *body.RateLimit <= 0 {
		invalidRequest(w, "rateLimit must be positive")
		return
	}
	key, err := db.UpdateAPIKey(id, db.APIKeyPatch{
		Name:                body.Name,
		Scope:               body.Scope,
		RateLimit:           body.RateLimit,
		Enabled:             body.Enabled,
		DailyRequestLimit:   body.DailyRequestLimit,
		DailyTokenLimit:     body.DailyTokenLimit,
		WeeklyRequestLimit:  body.WeeklyRequestLimit,
		WeeklyTokenLimit:    body.WeeklyTokenLimit,
		MonthlyRequestLimit: body.MonthlyRequestLimit,
		MonthlyTokenLimit:   body.MonthlyTokenLimit,
	})
	if err != nil {
		internalError(w, "update api key", err)
		return
	}
	if key == nil {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, toAPIKeyDTO(*key))
}

func validScope(scope string) bool {
	return scope == "ai" || scope == "api" || scope == "both"
}

// jsonStringSlice stores a []string as a JSON text column; nil for empty.
func jsonStringSlice(values []string) *string {
	if len(values) == 0 {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

func intOr(value *int, fallback int) int {
	if value != nil {
		return *value
	}
	return fallback
}

// ----------------------------------------------------------------- api configs

type configStatsDTO struct {
	Requests     int   `json:"requests"`
	Errors       int   `json:"errors"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
}

type apiConfigDTO struct {
	ID               int64          `json:"id"`
	Name             string         `json:"name"`
	Description      *string        `json:"description"`
	Method           string         `json:"method"`
	URL              string         `json:"url"`
	Headers          *string        `json:"headers"`
	RequestTemplate  *string        `json:"request_template"`
	ResponseTemplate *string        `json:"response_template"`
	TimeoutMs        int            `json:"timeout_ms"`
	StreamTimeoutMs  int            `json:"stream_timeout_ms"`
	StreamMaxBodyMb  int            `json:"stream_max_body_mb"`
	Route            string         `json:"route"`
	Enabled          int            `json:"enabled"`
	CreatedAt        int64          `json:"created_at"`
	UpdatedAt        int64          `json:"updated_at"`
	HasAPIKey        bool           `json:"has_api_key"`
	APIKeyMasked     *string        `json:"api_key_masked"`
	APIKeyError      bool           `json:"api_key_error,omitempty"`
	Stats            configStatsDTO `json:"stats"`
}

func (h *AdminResources) configDTO(config db.APIConfigRow, stats configStatsDTO) apiConfigDTO {
	dto := apiConfigDTO{
		ID:               config.ID,
		Name:             config.Name,
		Description:      config.Description,
		Method:           config.Method,
		URL:              config.URL,
		Headers:          config.Headers,
		RequestTemplate:  config.RequestTemplate,
		ResponseTemplate: config.ResponseTemplate,
		TimeoutMs:        config.TimeoutMs,
		StreamTimeoutMs:  config.StreamTimeoutMs,
		StreamMaxBodyMb:  config.StreamMaxBodyMb,
		Route:            config.Route,
		Enabled:          boolInt(config.Enabled),
		CreatedAt:        config.CreatedAt,
		UpdatedAt:        config.UpdatedAt,
		Stats:            stats,
	}
	if config.APIKey != nil && *config.APIKey != "" {
		plaintext, err := h.secrets.Decrypt(*config.APIKey)
		switch {
		case err != nil:
			masked := "(undecryptable)"
			dto.HasAPIKey = true
			dto.APIKeyError = true
			dto.APIKeyMasked = &masked
		case plaintext != "":
			masked := util.MaskSecret(plaintext)
			dto.HasAPIKey = true
			dto.APIKeyMasked = &masked
		}
	}
	return dto
}

func (h *AdminResources) apiConfigs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listAPIConfigs(w)
	case http.MethodPost:
		h.createAPIConfig(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminResources) listAPIConfigs(w http.ResponseWriter) {
	configs, err := db.ListAPIConfigs()
	if err != nil {
		internalError(w, "list api configs", err)
		return
	}
	statsRows, err := db.APIConfigStatsAll()
	if err != nil {
		internalError(w, "aggregate api config stats", err)
		return
	}
	statsByID := make(map[int64]configStatsDTO, len(statsRows))
	for _, row := range statsRows {
		statsByID[row.APIConfigID] = configStatsDTO{
			Requests:     row.Requests,
			Errors:       row.Errors,
			AvgLatencyMs: int64(row.AvgLatencyMs + 0.5),
		}
	}
	data := make([]apiConfigDTO, 0, len(configs))
	for _, config := range configs {
		data = append(data, h.configDTO(config, statsByID[config.ID]))
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (h *AdminResources) createAPIConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string          `json:"name"`
		Description      *string         `json:"description"`
		Method           *string         `json:"method"`
		URL              string          `json:"url"`
		Headers          json.RawMessage `json:"headers"`
		RequestTemplate  json.RawMessage `json:"requestTemplate"`
		ResponseTemplate json.RawMessage `json:"responseTemplate"`
		APIKey           *string         `json:"apiKey"`
		TimeoutMs        *int            `json:"timeoutMs"`
		StreamTimeoutMs  *int            `json:"streamTimeoutMs"`
		StreamMaxBodyMb  *int            `json:"streamMaxBodyMb"`
		Route            *string         `json:"route"`
		Enabled          *bool           `json:"enabled"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if !util.IsValidConfigName(body.Name) {
		invalidRequest(w, "wildcards must be a whole segment ('api/*', 'api/**')")
		return
	}
	if body.URL == "" {
		invalidRequest(w, "url is required")
		return
	}
	if _, err := util.ValidateUpstreamURL(body.URL); err != nil {
		invalidRequest(w, err.Error())
		return
	}
	route := "open"
	if body.Route != nil {
		if *body.Route != "open" && *body.Route != "free" {
			invalidRequest(w, "route must be open or free")
			return
		}
		route = *body.Route
	}
	headers, ok := configJSONColumn(w, body.Headers, "headers")
	if !ok {
		return
	}
	method := "POST"
	if body.Method != nil && *body.Method != "" {
		method = strings.ToUpper(*body.Method)
	}
	encrypted, err := h.encryptOptional(body.APIKey)
	if err != nil {
		internalError(w, "encrypt config key", err)
		return
	}
	timeoutMs := 15000
	if body.TimeoutMs != nil {
		timeoutMs = *body.TimeoutMs
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	config, err := db.CreateAPIConfig(db.NewAPIConfig{
		Name:             body.Name,
		Description:      body.Description,
		Method:           method,
		URL:              body.URL,
		Headers:          headers,
		RequestTemplate:  rawColumn(body.RequestTemplate),
		ResponseTemplate: rawColumn(body.ResponseTemplate),
		APIKey:           encrypted,
		TimeoutMs:        timeoutMs,
		StreamTimeoutMs:  intOr(body.StreamTimeoutMs, 0),
		StreamMaxBodyMb:  intOr(body.StreamMaxBodyMb, 0),
		Route:            route,
		Enabled:          enabled,
	})
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "create api config", err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, h.configDTO(*config, configStatsDTO{}))
}

func (h *AdminResources) apiConfigByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		config, err := db.GetAPIConfig(id)
		if err != nil {
			internalError(w, "get api config", err)
			return
		}
		if config == nil {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, h.configDTO(*config, configStatsDTO{}))
	case http.MethodPut:
		h.updateAPIConfig(w, r, id)
	case http.MethodDelete:
		removed, err := db.DeleteAPIConfig(id)
		if err != nil {
			internalError(w, "delete api config", err)
			return
		}
		if !removed {
			httperr.NotFound(w)
			return
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *AdminResources) updateAPIConfig(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Name             *string         `json:"name"`
		Description      *string         `json:"description"`
		Method           *string         `json:"method"`
		URL              *string         `json:"url"`
		Headers          json.RawMessage `json:"headers"`
		RequestTemplate  json.RawMessage `json:"requestTemplate"`
		ResponseTemplate json.RawMessage `json:"responseTemplate"`
		TimeoutMs        *int            `json:"timeoutMs"`
		StreamTimeoutMs  *int            `json:"streamTimeoutMs"`
		StreamMaxBodyMb  *int            `json:"streamMaxBodyMb"`
		Route            *string         `json:"route"`
		Enabled          *bool           `json:"enabled"`
	}
	fields, ok := decodeBody(w, r, &body)
	if !ok {
		return
	}
	if body.Name != nil && !util.IsValidConfigName(*body.Name) {
		invalidRequest(w, "wildcards must be a whole segment ('api/*', 'api/**')")
		return
	}
	if body.URL != nil {
		if _, err := util.ValidateUpstreamURL(*body.URL); err != nil {
			invalidRequest(w, err.Error())
			return
		}
	}
	if body.Route != nil && *body.Route != "open" && *body.Route != "free" {
		invalidRequest(w, "route must be open or free")
		return
	}
	method := body.Method
	if method != nil {
		upper := strings.ToUpper(*method)
		method = &upper
	}
	patch := db.APIConfigPatch{
		Name:            body.Name,
		Description:     body.Description,
		Method:          method,
		URL:             body.URL,
		TimeoutMs:       body.TimeoutMs,
		StreamTimeoutMs: body.StreamTimeoutMs,
		StreamMaxBodyMb: body.StreamMaxBodyMb,
		Route:           body.Route,
		Enabled:         body.Enabled,
	}
	if fieldPresent(fields, "headers") {
		headers, ok := configJSONColumn(w, body.Headers, "headers")
		if !ok {
			return
		}
		patch.Headers = headers
	}
	if fieldPresent(fields, "requestTemplate") {
		patch.RequestTemplate = rawColumn(body.RequestTemplate)
	}
	if fieldPresent(fields, "responseTemplate") {
		patch.ResponseTemplate = rawColumn(body.ResponseTemplate)
	}
	if set, value := nullableString(fields, "apiKey"); set {
		patch.APIKeySet = true
		encrypted, err := h.encryptOptional(value)
		if err != nil {
			internalError(w, "encrypt config key", err)
			return
		}
		patch.APIKey = encrypted
	}
	config, err := db.UpdateAPIConfig(id, patch)
	if err != nil {
		if isUniqueViolation(err) {
			conflict(w, err.Error(), "constraint_violation")
			return
		}
		internalError(w, "update api config", err)
		return
	}
	if config == nil {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, h.configDTO(*config, configStatsDTO{}))
}

// ------------------------------------------------------------------ stats

type modelStatDTO struct {
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
}

type providerStatDTO struct {
	Provider string `json:"provider"`
	Requests int    `json:"requests"`
	Errors   int    `json:"errors"`
}

type configStatRowDTO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Requests     int    `json:"requests"`
	Errors       int    `json:"errors"`
	AvgLatencyMs int64  `json:"avg_latency_ms"`
}

// stats serves GET /admin/stats: today's totals plus per-model and per-config
// breakdowns for the dashboard.
func (h *AdminResources) stats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	today, err := db.TodayStatsSince()
	if err != nil {
		internalError(w, "today stats", err)
		return
	}
	models, err := db.ModelStatsToday()
	if err != nil {
		internalError(w, "model stats", err)
		return
	}
	providers, err := db.ProviderStatsToday()
	if err != nil {
		internalError(w, "provider stats", err)
		return
	}
	configStats, err := db.APIConfigStatsAll()
	if err != nil {
		internalError(w, "api config stats", err)
		return
	}

	modelRows := make([]modelStatDTO, 0, len(models))
	for _, stat := range models {
		modelRows = append(modelRows, modelStatDTO{Model: stat.Model, Requests: stat.Requests, Tokens: stat.Tokens})
	}
	providerRows := make([]providerStatDTO, 0, len(providers))
	for _, stat := range providers {
		providerRows = append(providerRows, providerStatDTO{Provider: stat.Provider, Requests: stat.Requests, Errors: stat.Errors})
	}
	configRows := make([]configStatRowDTO, 0, len(configStats))
	for _, stat := range configStats {
		name := "#" + strconv.FormatInt(stat.APIConfigID, 10)
		config, err := db.GetAPIConfig(stat.APIConfigID)
		if err != nil {
			internalError(w, "load api config for stats", err)
			return
		}
		if config != nil {
			name = config.Name
		}
		configRows = append(configRows, configStatRowDTO{
			ID:           stat.APIConfigID,
			Name:         name,
			Requests:     stat.Requests,
			Errors:       stat.Errors,
			AvgLatencyMs: int64(math.Round(stat.AvgLatencyMs)),
		})
	}

	httperr.WriteJSON(w, http.StatusOK, map[string]any{
		"today": map[string]any{
			"requests":       today.Requests,
			"tokens":         today.Tokens,
			"errors":         today.Errors,
			"avg_latency_ms": int64(math.Round(today.AvgLatencyMs)),
		},
		"models":      modelRows,
		"providers":   providerRows,
		"api_configs": configRows,
	})
}

// ------------------------------------------------------------------ usage

type usageLogDTO struct {
	ID           int64   `json:"id"`
	RequestID    string  `json:"request_id"`
	APIKeyID     *int64  `json:"api_key_id"`
	Kind         string  `json:"kind"`
	Model        *string `json:"model"`
	Provider     *string `json:"provider"`
	APIConfigID  *int64  `json:"api_config_id"`
	TargetURL    *string `json:"target_url"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	LatencyMs    *int64  `json:"latency_ms"`
	Status       *int    `json:"status"`
	Stream       int     `json:"stream"`
	Error        *string `json:"error"`
	CreatedAt    int64   `json:"created_at"`
}

func toUsageLogDTO(entry db.UsageLogRow) usageLogDTO {
	stream := 0
	if entry.Stream {
		stream = 1
	}
	return usageLogDTO{
		ID:           entry.ID,
		RequestID:    entry.RequestID,
		APIKeyID:     entry.APIKeyID,
		Kind:         entry.Kind,
		Model:        entry.Model,
		Provider:     entry.Provider,
		APIConfigID:  entry.APIConfigID,
		TargetURL:    entry.TargetURL,
		InputTokens:  entry.InputTokens,
		OutputTokens: entry.OutputTokens,
		TotalTokens:  entry.TotalTokens,
		LatencyMs:    entry.LatencyMs,
		Status:       entry.Status,
		Stream:       stream,
		Error:        entry.Error,
		CreatedAt:    entry.CreatedAt,
	}
}

// usage serves GET /admin/usage with the same filters as the reference:
// model/provider/kind/status/since plus limit/offset paging.
func (h *AdminResources) usage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	query := r.URL.Query()
	filters := db.UsageFilters{
		Limit:    clampInt(query.Get("limit"), 100, 1000),
		Offset:   clampInt(query.Get("offset"), 0, 1_000_000),
		Model:    query.Get("model"),
		Provider: query.Get("provider"),
		Kind:     query.Get("kind"),
	}
	if raw := query.Get("status"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			filters.Status = &value
		}
	}
	if raw := query.Get("since"); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
			filters.Since = &value
		}
	}
	entries, err := db.ListUsageLogs(filters)
	if err != nil {
		internalError(w, "list usage logs", err)
		return
	}
	data := make([]usageLogDTO, 0, len(entries))
	for _, entry := range entries {
		data = append(data, toUsageLogDTO(entry))
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// ------------------------------------------------------------------- logs

type auditLogDTO struct {
	ID        int64   `json:"id"`
	Action    string  `json:"action"`
	Target    string  `json:"target"`
	SourceIP  *string `json:"source_ip"`
	CreatedAt int64   `json:"created_at"`
}

// logs serves GET /admin/logs: the newest audit entries.
func (h *AdminResources) logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	entries, err := db.ListAuditLogs(clampInt(r.URL.Query().Get("limit"), 100, 1000))
	if err != nil {
		internalError(w, "list audit logs", err)
		return
	}
	data := make([]auditLogDTO, 0, len(entries))
	for _, entry := range entries {
		data = append(data, auditLogDTO{
			ID:        entry.ID,
			Action:    entry.Action,
			Target:    entry.Target,
			SourceIP:  entry.SourceIP,
			CreatedAt: entry.CreatedAt,
		})
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// logsCleanup serves POST /admin/logs/cleanup: run the retention purge now.
func (h *AdminResources) logsCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	usage, audit, err := db.PurgeOldLogs(h.logRetentionDays)
	if err != nil {
		internalError(w, "purge old logs", err)
		return
	}
	_ = db.RecordAudit(
		"logs_cleanup",
		"usage:"+strconv.FormatInt(usage, 10)+",audit:"+strconv.FormatInt(audit, 10),
		clientIP(r),
	)
	httperr.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"deleted": map[string]int64{"usage": usage, "audit": audit},
	})
}

// ------------------------------------------------------------ model routes

type modelRouteDTO struct {
	ID            int64  `json:"id"`
	ModelName     string `json:"model_name"`
	ProviderID    int64  `json:"provider_id"`
	UpstreamModel string `json:"upstream_model"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	Enabled       int    `json:"enabled"`
}

func toModelRouteDTO(route db.ModelRouteRow) modelRouteDTO {
	return modelRouteDTO{
		ID:            route.ID,
		ModelName:     route.ModelName,
		ProviderID:    route.ProviderID,
		UpstreamModel: route.UpstreamModel,
		Priority:      route.Priority,
		Weight:        route.Weight,
		Enabled:       boolInt(route.Enabled),
	}
}

func (h *AdminResources) modelRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes, err := db.ListModelRoutes()
		if err != nil {
			internalError(w, "list model routes", err)
			return
		}
		data := make([]modelRouteDTO, 0, len(routes))
		for _, route := range routes {
			data = append(data, toModelRouteDTO(route))
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
	case http.MethodPost:
		h.createModelRoute(w, r)
	default:
		methodNotAllowed(w, "GET, POST")
	}
}

func (h *AdminResources) createModelRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ModelName     string `json:"modelName"`
		ProviderID    int64  `json:"providerId"`
		UpstreamModel string `json:"upstreamModel"`
		Priority      *int   `json:"priority"`
		Weight        *int   `json:"weight"`
		Enabled       *bool  `json:"enabled"`
	}
	if _, ok := decodeBody(w, r, &body); !ok {
		return
	}
	if body.ModelName == "" || body.UpstreamModel == "" || body.ProviderID <= 0 {
		invalidRequest(w, "modelName, providerId and upstreamModel are required")
		return
	}
	provider, err := db.GetProvider(body.ProviderID)
	if err != nil {
		internalError(w, "get provider", err)
		return
	}
	if provider == nil {
		invalidRequest(w, "Provider "+strconv.FormatInt(body.ProviderID, 10)+" does not exist")
		return
	}
	route, err := db.CreateModelRoute(db.NewModelRoute{
		ModelName:     body.ModelName,
		ProviderID:    body.ProviderID,
		UpstreamModel: body.UpstreamModel,
		Priority:      intOr(body.Priority, 100),
		Weight:        intOr(body.Weight, 100),
		Enabled:       body.Enabled == nil || *body.Enabled,
	})
	if err != nil {
		internalError(w, "create model route", err)
		return
	}
	httperr.WriteJSON(w, http.StatusCreated, toModelRouteDTO(*route))
}

func (h *AdminResources) modelRouteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, "DELETE")
		return
	}
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	removed, err := db.DeleteModelRoute(id)
	if err != nil {
		internalError(w, "delete model route", err)
		return
	}
	if !removed {
		httperr.NotFound(w)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ------------------------------------------------- api key/config relations

// apiKeyConfigs serves GET /admin/api-keys/:id/configs: which API configs the
// key may call (unrestricted when it has no allowed_apis list).
func (h *AdminResources) apiKeyConfigs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	key, err := db.GetAPIKey(id)
	if err != nil {
		internalError(w, "get api key", err)
		return
	}
	if key == nil {
		httperr.NotFound(w)
		return
	}
	allowed := parseStrings(key.AllowedAPIs)
	configs, err := db.ListAPIConfigs()
	if err != nil {
		internalError(w, "list api configs", err)
		return
	}
	type configRefDTO struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Route string `json:"route"`
	}
	data := make([]configRefDTO, 0, len(configs))
	for _, config := range configs {
		if len(allowed) > 0 && !containsString(allowed, config.Name) {
			continue
		}
		data = append(data, configRefDTO{ID: config.ID, Name: config.Name, Route: config.Route})
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// apiConfigStats serves GET /admin/api-configs/:id/stats.
func (h *AdminResources) apiConfigStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	config, err := db.GetAPIConfig(id)
	if err != nil {
		internalError(w, "get api config", err)
		return
	}
	if config == nil {
		httperr.NotFound(w)
		return
	}
	stats, err := db.APIConfigStatsFor(id)
	if err != nil {
		internalError(w, "api config stats", err)
		return
	}
	httperr.WriteJSON(w, http.StatusOK, configStatRowDTO{
		ID:           config.ID,
		Name:         config.Name,
		Requests:     stats.Requests,
		Errors:       stats.Errors,
		AvgLatencyMs: int64(math.Round(stats.AvgLatencyMs)),
	})
}

// apiConfigKeys serves GET /admin/api-configs/:id/keys: which client keys may
// call this config.
func (h *AdminResources) apiConfigKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET")
		return
	}
	id, ok := pathID(r)
	if !ok {
		invalidRequest(w, "Invalid id")
		return
	}
	config, err := db.GetAPIConfig(id)
	if err != nil {
		internalError(w, "get api config", err)
		return
	}
	if config == nil {
		httperr.NotFound(w)
		return
	}
	keys, err := db.ListAPIKeys()
	if err != nil {
		internalError(w, "list api keys", err)
		return
	}
	data := make([]apiKeyDTO, 0, len(keys))
	for _, key := range keys {
		if allowed := key.AllowedAPIs; allowed != nil && *allowed != "" {
			// An explicit list: the config must be named in it. A malformed list
			// matches nothing, mirroring the reference.
			if !containsString(parseStrings(allowed), config.Name) {
				continue
			}
		}
		data = append(data, toAPIKeyDTO(key))
	}
	httperr.WriteJSON(w, http.StatusOK, map[string]any{"data": data})
}

// clampInt parses an optional integer query parameter, falling back to
// fallback when it is missing/invalid and capping it at max.
func clampInt(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

// rawColumn converts a JSON template value into the stored text column; empty
// or null becomes NULL.
func rawColumn(raw json.RawMessage) *string {
	if isJSONNull(raw) {
		return nil
	}
	text := string(raw)
	return &text
}

// configJSONColumn validates an optional headers object and returns it as the
// stored JSON text column.
func configJSONColumn(w http.ResponseWriter, raw json.RawMessage, field string) (*string, bool) {
	if isJSONNull(raw) {
		return nil, true
	}
	var headerMap map[string]string
	if err := json.Unmarshal(raw, &headerMap); err != nil {
		invalidRequest(w, field+" must be an object of strings")
		return nil, false
	}
	text := string(raw)
	return &text, true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
