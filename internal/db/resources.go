package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"relay-api/internal/util"
)

// ------------------------------------------------------------------ helpers

func nullString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// ----------------------------------------------------------------- providers

// ProviderRow is one upstream provider.
type ProviderRow struct {
	ID      int64
	Name    string
	Type    string
	BaseURL string
	APIKey  *string
	// Headers is a JSON object of extra upstream headers.
	Headers *string
	// ReasoningReplay opts this provider into the reasoning round-trip (see
	// providers/reasoning.go). Off by default.
	ReasoningReplay bool
	Enabled         bool
	CreatedAt       int64
	UpdatedAt       int64
}

const providerColumns = `id, name, type, base_url, api_key, headers, reasoning_replay, enabled, created_at, updated_at`

func scanProvider(row interface{ Scan(...any) error }) (*ProviderRow, error) {
	var (
		provider ProviderRow
		apiKey   sql.NullString
		headers  sql.NullString
		replay   int
		enabled  int
	)
	if err := row.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &apiKey, &headers, &replay, &enabled, &provider.CreatedAt, &provider.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if apiKey.Valid {
		provider.APIKey = &apiKey.String
	}
	if headers.Valid {
		provider.Headers = &headers.String
	}
	provider.ReasoningReplay = replay != 0
	provider.Enabled = enabled != 0
	return &provider, nil
}

// ListProviders returns all providers ordered by id.
func ListProviders() ([]ProviderRow, error) {
	rows, err := Read().Query(`SELECT ` + providerColumns + ` FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var providers []ProviderRow
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, *provider)
	}
	return providers, rows.Err()
}

// GetProvider loads one provider (nil when missing).
func GetProvider(id int64) (*ProviderRow, error) {
	return scanProvider(Read().QueryRow(`SELECT `+providerColumns+` FROM providers WHERE id = ?`, id))
}

// CreateProvider inserts a provider. Headers is a JSON object of extra upstream
// headers (nil/empty for none). reasoningReplay opts into the reasoning
// round-trip, which is off by default.
func CreateProvider(name, kind, baseURL string, apiKey, headers *string, reasoningReplay, enabled bool) (*ProviderRow, error) {
	now := time.Now().UnixMilli()
	result, err := Get().Exec(
		`INSERT INTO providers (name, type, base_url, api_key, headers, reasoning_replay, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, kind, baseURL, nullString(apiKey), nullString(headers), boolToInt(reasoningReplay), boolToInt(enabled), now, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	invalidateAIResourceCache()
	return GetProvider(id)
}

// ProviderPatch carries the fields an update may set. A nil pointer keeps the
// current value; APIKeySet (with a possibly nil APIKey) replaces it.
type ProviderPatch struct {
	Name    *string
	Type    *string
	BaseURL *string
	Enabled *bool
	// ReasoningReplay toggles the opt-in reasoning round-trip.
	ReasoningReplay *bool
	APIKeySet       bool
	APIKey          *string
	// HeadersSet replaces the current headers (with possibly nil) when true.
	HeadersSet bool
	Headers    *string
}

// UpdateProvider applies a patch and returns the updated row.
func UpdateProvider(id int64, patch ProviderPatch) (*ProviderRow, error) {
	existing, err := GetProvider(id)
	if err != nil || existing == nil {
		return existing, err
	}
	name, kind, baseURL := existing.Name, existing.Type, existing.BaseURL
	if patch.Name != nil {
		name = *patch.Name
	}
	if patch.Type != nil {
		kind = *patch.Type
	}
	if patch.BaseURL != nil {
		baseURL = *patch.BaseURL
	}
	enabled := existing.Enabled
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	apiKey := existing.APIKey
	if patch.APIKeySet {
		apiKey = patch.APIKey
	}
	headers := existing.Headers
	if patch.HeadersSet {
		headers = patch.Headers
	}
	reasoningReplay := existing.ReasoningReplay
	if patch.ReasoningReplay != nil {
		reasoningReplay = *patch.ReasoningReplay
	}
	_, err = Get().Exec(
		`UPDATE providers SET name = ?, type = ?, base_url = ?, api_key = ?, headers = ?, reasoning_replay = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		name, kind, baseURL, nullString(apiKey), nullString(headers), boolToInt(reasoningReplay), boolToInt(enabled), time.Now().UnixMilli(), id,
	)
	if err != nil {
		return nil, err
	}
	invalidateAIResourceCache()
	return GetProvider(id)
}

// DeleteProvider removes a provider. Reports whether a row was removed.
func DeleteProvider(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if affected > 0 {
		invalidateAIResourceCache()
	}
	return affected > 0, err
}

// ListModelNamesByProvider lists model aliases (from models and model_routes)
// that still point at a provider, used to explain why it cannot be deleted.
func ListModelNamesByProvider(providerID int64) ([]string, error) {
	rows, err := Read().Query(
		`SELECT name AS model_name FROM models WHERE provider_id = ?
		 UNION
		 SELECT model_name FROM model_routes WHERE provider_id = ?
		 ORDER BY model_name`,
		providerID, providerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// -------------------------------------------------------------------- models

// ModelRow is one model alias.
type ModelRow struct {
	ID            int64
	Name          string
	ProviderID    int64
	UpstreamModel string
	Enabled       bool
	Priority      int
	CreatedAt     int64
}

const modelColumns = `id, name, provider_id, upstream_model, enabled, priority, created_at`

func scanModel(row interface{ Scan(...any) error }) (*ModelRow, error) {
	var (
		model   ModelRow
		enabled int
	)
	if err := row.Scan(&model.ID, &model.Name, &model.ProviderID, &model.UpstreamModel, &enabled, &model.Priority, &model.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	model.Enabled = enabled != 0
	return &model, nil
}

// ListModels returns aliases ordered by priority then name.
func ListModels() ([]ModelRow, error) {
	rows, err := Read().Query(`SELECT ` + modelColumns + ` FROM models ORDER BY priority, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []ModelRow
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		models = append(models, *model)
	}
	return models, rows.Err()
}

// GetModel loads one alias (nil when missing).
func GetModel(id int64) (*ModelRow, error) {
	return scanModel(Read().QueryRow(`SELECT `+modelColumns+` FROM models WHERE id = ?`, id))
}

// FindModelByName finds a duplicate alias, ignoring excludeID (0 = none).
func FindModelByName(name string, excludeID int64) (*ModelRow, error) {
	if excludeID > 0 {
		return scanModel(Read().QueryRow(`SELECT `+modelColumns+` FROM models WHERE name = ? AND id != ?`, name, excludeID))
	}
	return scanModel(Read().QueryRow(`SELECT `+modelColumns+` FROM models WHERE name = ?`, name))
}

// CreateModel inserts an alias.
func CreateModel(name string, providerID int64, upstreamModel string, enabled bool, priority int) (*ModelRow, error) {
	result, err := Get().Exec(
		`INSERT INTO models (name, provider_id, upstream_model, enabled, priority, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, providerID, upstreamModel, boolToInt(enabled), priority, time.Now().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	invalidateAIResourceCache()
	return GetModel(id)
}

// ModelPatch carries optional model fields; nil keeps the current value.
type ModelPatch struct {
	Name          *string
	ProviderID    *int64
	UpstreamModel *string
	Enabled       *bool
	Priority      *int
}

// UpdateModel applies a patch and returns the updated row.
func UpdateModel(id int64, patch ModelPatch) (*ModelRow, error) {
	existing, err := GetModel(id)
	if err != nil || existing == nil {
		return existing, err
	}
	name, providerID, upstreamModel, enabled, priority := existing.Name, existing.ProviderID, existing.UpstreamModel, existing.Enabled, existing.Priority
	if patch.Name != nil {
		name = *patch.Name
	}
	if patch.ProviderID != nil {
		providerID = *patch.ProviderID
	}
	if patch.UpstreamModel != nil {
		upstreamModel = *patch.UpstreamModel
	}
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	if patch.Priority != nil {
		priority = *patch.Priority
	}
	err = Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`UPDATE models SET name = ?, provider_id = ?, upstream_model = ?, enabled = ?, priority = ? WHERE id = ?`,
			name, providerID, upstreamModel, boolToInt(enabled), priority, id,
		); err != nil {
			return err
		}
		// Keep failover routes attached across a rename. Without this the old
		// name would keep routing (an orphan model_routes row no longer matches
		// any models row) while the renamed alias silently lost its routes.
		if name != existing.Name {
			if _, err := tx.Exec(`UPDATE model_routes SET model_name = ? WHERE model_name = ?`, name, existing.Name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	invalidateAIResourceCache()
	return GetModel(id)
}

// DeleteModel removes an alias together with its failover routes, so no
// orphaned model_routes row can keep the name resolvable. Reports whether a row
// was removed.
func DeleteModel(id int64) (bool, error) {
	existing, err := GetModel(id)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}
	affected := int64(0)
	err = Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM model_routes WHERE model_name = ?`, existing.Name); err != nil {
			return err
		}
		result, err := tx.Exec(`DELETE FROM models WHERE id = ?`, id)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return false, err
	}
	if affected > 0 {
		invalidateAIResourceCache()
	}
	return affected > 0, nil
}

// ------------------------------------------------------------------ api keys

// APIKeyRow is one client API key (stored hashed).
type APIKeyRow struct {
	ID                  int64
	Name                string
	Prefix              string
	KeyHash             string
	Scope               string
	AllowedModels       *string
	AllowedAPIs         *string
	RateLimit           int
	DailyRequestLimit   int
	DailyTokenLimit     int
	WeeklyRequestLimit  int
	WeeklyTokenLimit    int
	MonthlyRequestLimit int
	MonthlyTokenLimit   int
	Enabled             bool
	ExpiresAt           *int64
	CreatedAt           int64
	LastUsedAt          *int64

	// Parsed permission lists, filled when the row is scanned. They keep the
	// auth/forward hot paths from decoding the same JSON arrays per request.
	allowedModels []string
	allowedAPIs   []string
	allowedParsed bool
}

// AllowedModelList returns the key's model whitelist, parsed once when the row
// was loaded (with an on-demand fallback for directly constructed rows).
func (k APIKeyRow) AllowedModelList() []string {
	if k.allowedParsed {
		return k.allowedModels
	}
	return parseStringList(k.AllowedModels)
}

// AllowedAPIList returns the key's API-config whitelist.
func (k APIKeyRow) AllowedAPIList() []string {
	if k.allowedParsed {
		return k.allowedAPIs
	}
	return parseStringList(k.AllowedAPIs)
}

// parseStringList decodes a stored JSON array of strings (nil when absent or
// invalid).
func parseStringList(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(*raw), &values); err != nil {
		return nil
	}
	return values
}

// HasQuotaLimits reports whether the key has any daily/weekly/monthly request
// or token quota configured. Keys without one skip quota-counter maintenance
// on the relay path.
func (k APIKeyRow) HasQuotaLimits() bool {
	return k.DailyRequestLimit > 0 || k.DailyTokenLimit > 0 ||
		k.WeeklyRequestLimit > 0 || k.WeeklyTokenLimit > 0 ||
		k.MonthlyRequestLimit > 0 || k.MonthlyTokenLimit > 0
}

const apiKeyColumns = `id, name, prefix, key_hash, scope, allowed_models, allowed_apis, rate_limit,
	daily_request_limit, daily_token_limit, weekly_request_limit, weekly_token_limit,
	monthly_request_limit, monthly_token_limit, enabled, expires_at, created_at, last_used_at`

func scanAPIKey(row interface{ Scan(...any) error }) (*APIKeyRow, error) {
	var (
		key       APIKeyRow
		allowedM  sql.NullString
		allowedA  sql.NullString
		enabled   int
		expiresAt sql.NullInt64
		lastUsed  sql.NullInt64
	)
	if err := row.Scan(
		&key.ID, &key.Name, &key.Prefix, &key.KeyHash, &key.Scope, &allowedM, &allowedA, &key.RateLimit,
		&key.DailyRequestLimit, &key.DailyTokenLimit, &key.WeeklyRequestLimit, &key.WeeklyTokenLimit,
		&key.MonthlyRequestLimit, &key.MonthlyTokenLimit, &enabled, &expiresAt, &key.CreatedAt, &lastUsed,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if allowedM.Valid {
		key.AllowedModels = &allowedM.String
	}
	if allowedA.Valid {
		key.AllowedAPIs = &allowedA.String
	}
	if expiresAt.Valid {
		key.ExpiresAt = &expiresAt.Int64
	}
	if lastUsed.Valid {
		key.LastUsedAt = &lastUsed.Int64
	}
	key.Enabled = enabled != 0
	key.allowedModels = parseStringList(key.AllowedModels)
	key.allowedAPIs = parseStringList(key.AllowedAPIs)
	key.allowedParsed = true
	return &key, nil
}

// ListAPIKeys returns every key ordered by id.
func ListAPIKeys() ([]APIKeyRow, error) {
	rows, err := Read().Query(`SELECT ` + apiKeyColumns + ` FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKeyRow
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, *key)
	}
	return keys, rows.Err()
}

// GetAPIKey loads one key (nil when missing).
func GetAPIKey(id int64) (*APIKeyRow, error) {
	return scanAPIKey(Read().QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id))
}

// NewAPIKey is the insert payload for an API key.
type NewAPIKey struct {
	Name                string
	Prefix              string
	KeyHash             string
	Scope               string
	AllowedModels       *string
	AllowedAPIs         *string
	RateLimit           int
	ExpiresAt           *int64
	DailyRequestLimit   int
	DailyTokenLimit     int
	WeeklyRequestLimit  int
	WeeklyTokenLimit    int
	MonthlyRequestLimit int
	MonthlyTokenLimit   int
}

// CreateAPIKey inserts a key row.
func CreateAPIKey(input NewAPIKey) (*APIKeyRow, error) {
	result, err := Get().Exec(
		`INSERT INTO api_keys (name, prefix, key_hash, scope, allowed_models, allowed_apis, rate_limit,
			daily_request_limit, daily_token_limit, weekly_request_limit, weekly_token_limit,
			monthly_request_limit, monthly_token_limit, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.Name, input.Prefix, input.KeyHash, input.Scope, nullString(input.AllowedModels), nullString(input.AllowedAPIs),
		input.RateLimit, input.DailyRequestLimit, input.DailyTokenLimit, input.WeeklyRequestLimit, input.WeeklyTokenLimit,
		input.MonthlyRequestLimit, input.MonthlyTokenLimit, nullInt(input.ExpiresAt), time.Now().UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	invalidateAPIKeyCache()
	return GetAPIKey(id)
}

func nullInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// APIKeyPatch carries optional key fields; nil keeps the current value.
type APIKeyPatch struct {
	Name                *string
	Scope               *string
	AllowedModels       *string
	RateLimit           *int
	Enabled             *bool
	DailyRequestLimit   *int
	DailyTokenLimit     *int
	WeeklyRequestLimit  *int
	WeeklyTokenLimit    *int
	MonthlyRequestLimit *int
	MonthlyTokenLimit   *int
}

// UpdateAPIKey applies a patch and returns the updated row.
func UpdateAPIKey(id int64, patch APIKeyPatch) (*APIKeyRow, error) {
	existing, err := GetAPIKey(id)
	if err != nil || existing == nil {
		return existing, err
	}
	pickString := func(p *string, current string) string {
		if p != nil {
			return *p
		}
		return current
	}
	pickInt := func(p *int, current int) int {
		if p != nil {
			return *p
		}
		return current
	}
	enabled := existing.Enabled
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	// An empty string clears the restriction (NULL = every model allowed).
	allowedModels := existing.AllowedModels
	if patch.AllowedModels != nil {
		if *patch.AllowedModels == "" {
			allowedModels = nil
		} else {
			allowedModels = patch.AllowedModels
		}
	}
	_, err = Get().Exec(
		`UPDATE api_keys SET name = ?, scope = ?, allowed_models = ?, rate_limit = ?, enabled = ?,
			daily_request_limit = ?, daily_token_limit = ?, weekly_request_limit = ?, weekly_token_limit = ?,
			monthly_request_limit = ?, monthly_token_limit = ? WHERE id = ?`,
		pickString(patch.Name, existing.Name),
		pickString(patch.Scope, existing.Scope),
		nullString(allowedModels),
		pickInt(patch.RateLimit, existing.RateLimit),
		boolToInt(enabled),
		pickInt(patch.DailyRequestLimit, existing.DailyRequestLimit),
		pickInt(patch.DailyTokenLimit, existing.DailyTokenLimit),
		pickInt(patch.WeeklyRequestLimit, existing.WeeklyRequestLimit),
		pickInt(patch.WeeklyTokenLimit, existing.WeeklyTokenLimit),
		pickInt(patch.MonthlyRequestLimit, existing.MonthlyRequestLimit),
		pickInt(patch.MonthlyTokenLimit, existing.MonthlyTokenLimit),
		id,
	)
	if err != nil {
		return nil, err
	}
	invalidateAPIKeyCache()
	return GetAPIKey(id)
}

// DeleteAPIKey removes a key. Reports whether a row was removed.
func DeleteAPIKey(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if affected > 0 {
		invalidateAPIKeyCache()
		forgetQuotaCounter(id)
	}
	return affected > 0, err
}

// ---------------------------------------------------------------- api configs

// Defaults for a /free tunnel's limits. 0 keeps its "unlimited" meaning when it
// is sent explicitly; these only apply when a value is left out.
const (
	// DefaultStreamTimeoutMs is the tunnel's inactivity limit (5 minutes):
	// the wait for response headers, and then the gap between body chunks.
	// It is not a total-duration cap, so an active long-lived stream is never
	// cut just for being old, while one whose peer vanished without a FIN is
	// still reaped instead of pinning its goroutine and buffer forever.
	DefaultStreamTimeoutMs = 300_000
	// DefaultStreamMaxBodyMb caps a tunnel upload. The tunnel streams bodies
	// through untouched, so an unbounded one would also raise the h2c
	// connection flow-control buffers.
	DefaultStreamMaxBodyMb = 8
)

// APIConfigRow is one non-AI forwarding config.
type APIConfigRow struct {
	ID               int64
	Name             string
	Description      *string
	Method           string
	URL              string
	Headers          *string
	RequestTemplate  *string
	ResponseTemplate *string
	APIKey           *string
	TimeoutMs        int
	StreamTimeoutMs  int
	StreamMaxBodyMb  int
	Route            string
	Enabled          bool
	CreatedAt        int64
	UpdatedAt        int64

	// Compiled template forms, populated when the enabled-config cache loads.
	// Nil means the raw field was absent/invalid (or this row came from a path
	// that does not compile); the handlers fall back to parsing it per request.
	ParsedHeaders          map[string]*util.Template `json:"-"`
	ParsedRequestTemplate  *util.Template            `json:"-"`
	ParsedResponseTemplate *util.Template            `json:"-"`
}

const apiConfigColumns = `id, name, description, method, url, headers, request_template, response_template,
	api_key, timeout_ms, stream_timeout_ms, stream_max_body_mb, route, enabled, created_at, updated_at`

func scanAPIConfig(row interface{ Scan(...any) error }) (*APIConfigRow, error) {
	var (
		config      APIConfigRow
		description sql.NullString
		headers     sql.NullString
		requestTpl  sql.NullString
		responseTpl sql.NullString
		apiKey      sql.NullString
		route       sql.NullString
		enabled     int
	)
	if err := row.Scan(
		&config.ID, &config.Name, &description, &config.Method, &config.URL, &headers, &requestTpl, &responseTpl,
		&apiKey, &config.TimeoutMs, &config.StreamTimeoutMs, &config.StreamMaxBodyMb, &route, &enabled,
		&config.CreatedAt, &config.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if description.Valid {
		config.Description = &description.String
	}
	if headers.Valid {
		config.Headers = &headers.String
	}
	if requestTpl.Valid {
		config.RequestTemplate = &requestTpl.String
	}
	if responseTpl.Valid {
		config.ResponseTemplate = &responseTpl.String
	}
	if apiKey.Valid {
		config.APIKey = &apiKey.String
	}
	config.Route = route.String
	if config.Route == "" {
		config.Route = "open"
	}
	config.Enabled = enabled != 0
	return &config, nil
}

// ListAPIConfigs returns every config ordered by name.
func ListAPIConfigs() ([]APIConfigRow, error) {
	rows, err := Read().Query(`SELECT ` + apiConfigColumns + ` FROM api_configs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []APIConfigRow
	for rows.Next() {
		config, err := scanAPIConfig(rows)
		if err != nil {
			return nil, err
		}
		configs = append(configs, *config)
	}
	return configs, rows.Err()
}

// GetAPIConfig loads one config (nil when missing).
func GetAPIConfig(id int64) (*APIConfigRow, error) {
	return scanAPIConfig(Read().QueryRow(`SELECT `+apiConfigColumns+` FROM api_configs WHERE id = ?`, id))
}

// NewAPIConfig is the insert payload for a config.
type NewAPIConfig struct {
	Name             string
	Description      *string
	Method           string
	URL              string
	Headers          *string
	RequestTemplate  *string
	ResponseTemplate *string
	APIKey           *string
	TimeoutMs        int
	StreamTimeoutMs  int
	StreamMaxBodyMb  int
	Route            string
	Enabled          bool
}

// CreateAPIConfig inserts a config.
func CreateAPIConfig(input NewAPIConfig) (*APIConfigRow, error) {
	now := time.Now().UnixMilli()
	result, err := Get().Exec(
		`INSERT INTO api_configs (name, description, method, url, headers, request_template, response_template,
			api_key, timeout_ms, stream_timeout_ms, stream_max_body_mb, route, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.Name, nullString(input.Description), input.Method, input.URL, nullString(input.Headers),
		nullString(input.RequestTemplate), nullString(input.ResponseTemplate), nullString(input.APIKey),
		input.TimeoutMs, input.StreamTimeoutMs, input.StreamMaxBodyMb, input.Route, boolToInt(input.Enabled), now, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	invalidateAPIConfigCache()
	return GetAPIConfig(id)
}

// APIConfigPatch carries optional config fields; nil keeps the current value.
type APIConfigPatch struct {
	Name             *string
	Description      *string
	Method           *string
	URL              *string
	Headers          *string
	RequestTemplate  *string
	ResponseTemplate *string
	TimeoutMs        *int
	StreamTimeoutMs  *int
	StreamMaxBodyMb  *int
	Route            *string
	Enabled          *bool
	APIKeySet        bool
	APIKey           *string
}

// UpdateAPIConfig applies a patch and returns the updated row.
func UpdateAPIConfig(id int64, patch APIConfigPatch) (*APIConfigRow, error) {
	existing, err := GetAPIConfig(id)
	if err != nil || existing == nil {
		return existing, err
	}
	setString := func(p *string, current *string) *string {
		if p != nil {
			return p
		}
		return current
	}
	setInt := func(p *int, current int) int {
		if p != nil {
			return *p
		}
		return current
	}
	enabled := existing.Enabled
	if patch.Enabled != nil {
		enabled = *patch.Enabled
	}
	apiKey := existing.APIKey
	if patch.APIKeySet {
		apiKey = patch.APIKey
	}
	_, err = Get().Exec(
		`UPDATE api_configs SET name = ?, description = ?, method = ?, url = ?, headers = ?,
			request_template = ?, response_template = ?, api_key = ?, timeout_ms = ?,
			stream_timeout_ms = ?, stream_max_body_mb = ?, route = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		firstString(patch.Name, existing.Name),
		nullString(setString(patch.Description, existing.Description)),
		firstString(patch.Method, existing.Method),
		firstString(patch.URL, existing.URL),
		nullString(setString(patch.Headers, existing.Headers)),
		nullString(setString(patch.RequestTemplate, existing.RequestTemplate)),
		nullString(setString(patch.ResponseTemplate, existing.ResponseTemplate)),
		nullString(apiKey),
		setInt(patch.TimeoutMs, existing.TimeoutMs),
		setInt(patch.StreamTimeoutMs, existing.StreamTimeoutMs),
		setInt(patch.StreamMaxBodyMb, existing.StreamMaxBodyMb),
		firstString(patch.Route, existing.Route),
		boolToInt(enabled),
		time.Now().UnixMilli(),
		id,
	)
	if err != nil {
		return nil, err
	}
	invalidateAPIConfigCache()
	return GetAPIConfig(id)
}

func firstString(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

// DeleteAPIConfig removes a config. Reports whether a row was removed.
func DeleteAPIConfig(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM api_configs WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	invalidateAPIConfigCache()
	return affected > 0, err
}

// APIConfigStats is per-config request aggregation across all time.
type APIConfigStats struct {
	APIConfigID  int64
	Requests     int
	Errors       int
	AvgLatencyMs float64
}

// Config stats are all-time aggregates over every kind='api' row. The grouped
// scan costs grow with the whole retention window (millions of rows), while the
// numbers only change as requests arrive, so the result is cached briefly. Both
// the configs list and the optional /admin/stats breakdown then stop re-running
// the scan on every page load.
const configStatsCacheTTL = 60 * time.Second

var (
	configStatsMu      sync.Mutex
	configStatsCache   []APIConfigStats
	configStatsCacheAt time.Time
)

// resetConfigStatsCache drops the cached aggregates when the database handle
// changes, so a fresh handle never serves the previous handle's numbers.
func resetConfigStatsCache() {
	configStatsMu.Lock()
	configStatsCache = nil
	configStatsCacheAt = time.Time{}
	configStatsMu.Unlock()
}

// APIConfigStatsAll aggregates usage_logs per config (kind = api), cached for a
// short interval. The mutex also acts as single-flight: concurrent callers wait
// for the one running scan and then read its cached result.
func APIConfigStatsAll() ([]APIConfigStats, error) {
	configStatsMu.Lock()
	defer configStatsMu.Unlock()
	if !configStatsCacheAt.IsZero() && time.Since(configStatsCacheAt) < configStatsCacheTTL {
		return append([]APIConfigStats(nil), configStatsCache...), nil
	}
	stats, err := apiConfigStatsAllSQL()
	if err != nil {
		return nil, err
	}
	configStatsCache = stats
	configStatsCacheAt = time.Now()
	return append([]APIConfigStats(nil), stats...), nil
}

// apiConfigStatsAllSQL is the uncached grouped scan behind APIConfigStatsAll.
// idx_usage_config_cover makes it cover the selected columns.
func apiConfigStatsAllSQL() ([]APIConfigStats, error) {
	rows, err := Read().Query(
		`SELECT api_config_id, COUNT(*) AS requests,
			COALESCE(SUM(CASE WHEN status >= 400 OR status IS NULL THEN 1 ELSE 0 END), 0) AS errors,
			COALESCE(AVG(latency_ms), 0) AS avg_latency_ms
		 FROM usage_logs
		 WHERE kind = 'api' AND api_config_id IS NOT NULL
		 GROUP BY api_config_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []APIConfigStats
	for rows.Next() {
		var row APIConfigStats
		if err := rows.Scan(&row.APIConfigID, &row.Requests, &row.Errors, &row.AvgLatencyMs); err != nil {
			return nil, err
		}
		stats = append(stats, row)
	}
	return stats, rows.Err()
}
