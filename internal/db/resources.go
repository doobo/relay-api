package db

import (
	"database/sql"
	"errors"
	"time"
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
	ID        int64
	Name      string
	Type      string
	BaseURL   string
	APIKey    *string
	Enabled   bool
	CreatedAt int64
	UpdatedAt int64
}

const providerColumns = `id, name, type, base_url, api_key, enabled, created_at, updated_at`

func scanProvider(row interface{ Scan(...any) error }) (*ProviderRow, error) {
	var (
		provider ProviderRow
		apiKey   sql.NullString
		enabled  int
	)
	if err := row.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &apiKey, &enabled, &provider.CreatedAt, &provider.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if apiKey.Valid {
		provider.APIKey = &apiKey.String
	}
	provider.Enabled = enabled != 0
	return &provider, nil
}

// ListProviders returns all providers ordered by id.
func ListProviders() ([]ProviderRow, error) {
	rows, err := Get().Query(`SELECT ` + providerColumns + ` FROM providers ORDER BY id`)
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
	return scanProvider(Get().QueryRow(`SELECT `+providerColumns+` FROM providers WHERE id = ?`, id))
}

// CreateProvider inserts a provider.
func CreateProvider(name, kind, baseURL string, apiKey *string, enabled bool) (*ProviderRow, error) {
	now := time.Now().UnixMilli()
	result, err := Get().Exec(
		`INSERT INTO providers (name, type, base_url, api_key, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, kind, baseURL, nullString(apiKey), boolToInt(enabled), now, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return GetProvider(id)
}

// ProviderPatch carries the fields an update may set. A nil pointer keeps the
// current value; APIKeySet (with a possibly nil APIKey) replaces it.
type ProviderPatch struct {
	Name      *string
	Type      *string
	BaseURL   *string
	Enabled   *bool
	APIKeySet bool
	APIKey    *string
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
	_, err = Get().Exec(
		`UPDATE providers SET name = ?, type = ?, base_url = ?, api_key = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		name, kind, baseURL, nullString(apiKey), boolToInt(enabled), time.Now().UnixMilli(), id,
	)
	if err != nil {
		return nil, err
	}
	return GetProvider(id)
}

// DeleteProvider removes a provider. Reports whether a row was removed.
func DeleteProvider(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// ListModelNamesByProvider lists model aliases (from models and model_routes)
// that still point at a provider, used to explain why it cannot be deleted.
func ListModelNamesByProvider(providerID int64) ([]string, error) {
	rows, err := Get().Query(
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
	rows, err := Get().Query(`SELECT ` + modelColumns + ` FROM models ORDER BY priority, name`)
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
	return scanModel(Get().QueryRow(`SELECT `+modelColumns+` FROM models WHERE id = ?`, id))
}

// FindModelByName finds a duplicate alias, ignoring excludeID (0 = none).
func FindModelByName(name string, excludeID int64) (*ModelRow, error) {
	if excludeID > 0 {
		return scanModel(Get().QueryRow(`SELECT `+modelColumns+` FROM models WHERE name = ? AND id != ?`, name, excludeID))
	}
	return scanModel(Get().QueryRow(`SELECT `+modelColumns+` FROM models WHERE name = ?`, name))
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
	_, err = Get().Exec(
		`UPDATE models SET name = ?, provider_id = ?, upstream_model = ?, enabled = ?, priority = ? WHERE id = ?`,
		name, providerID, upstreamModel, boolToInt(enabled), priority, id,
	)
	if err != nil {
		return nil, err
	}
	return GetModel(id)
}

// DeleteModel removes an alias. Reports whether a row was removed.
func DeleteModel(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
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
	return &key, nil
}

// ListAPIKeys returns every key ordered by id.
func ListAPIKeys() ([]APIKeyRow, error) {
	rows, err := Get().Query(`SELECT ` + apiKeyColumns + ` FROM api_keys ORDER BY id`)
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
	return scanAPIKey(Get().QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id))
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
	_, err = Get().Exec(
		`UPDATE api_keys SET name = ?, scope = ?, rate_limit = ?, enabled = ?,
			daily_request_limit = ?, daily_token_limit = ?, weekly_request_limit = ?, weekly_token_limit = ?,
			monthly_request_limit = ?, monthly_token_limit = ? WHERE id = ?`,
		pickString(patch.Name, existing.Name),
		pickString(patch.Scope, existing.Scope),
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
	return GetAPIKey(id)
}

// DeleteAPIKey removes a key. Reports whether a row was removed.
func DeleteAPIKey(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// ---------------------------------------------------------------- api configs

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
	rows, err := Get().Query(`SELECT ` + apiConfigColumns + ` FROM api_configs ORDER BY name`)
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
	return scanAPIConfig(Get().QueryRow(`SELECT `+apiConfigColumns+` FROM api_configs WHERE id = ?`, id))
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
	return affected > 0, err
}

// APIConfigStats is per-config request aggregation across all time.
type APIConfigStats struct {
	APIConfigID  int64
	Requests     int
	Errors       int
	AvgLatencyMs float64
}

// APIConfigStatsAll aggregates usage_logs per config (kind = api).
func APIConfigStatsAll() ([]APIConfigStats, error) {
	rows, err := Get().Query(
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
