package db

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// schema is the full baseline, transcribed from the reference gateway's
// src/db/migrate.ts. Column names, types and the epoch-millisecond timestamp
// convention are kept identical so an existing gateway.db can be reused as-is.
const schema = `
CREATE TABLE IF NOT EXISTS providers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key TEXT,
  /* Extra upstream headers as a JSON object; values may reference client
     headers with ${name} and the resolved identity with ${_ai_session}. */
  headers TEXT,
  enabled INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS models (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  provider_id INTEGER NOT NULL,
  upstream_model TEXT NOT NULL,
  enabled INTEGER DEFAULT 1,
  priority INTEGER DEFAULT 100,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(provider_id) REFERENCES providers(id)
);

CREATE TABLE IF NOT EXISTS model_routes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  model_name TEXT NOT NULL,
  provider_id INTEGER NOT NULL,
  upstream_model TEXT NOT NULL,
  priority INTEGER DEFAULT 100,
  weight INTEGER DEFAULT 100,
  enabled INTEGER DEFAULT 1
);

CREATE TABLE IF NOT EXISTS api_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  scope TEXT DEFAULT 'both',
  allowed_models TEXT,
  allowed_apis TEXT,
  rate_limit INTEGER DEFAULT 60,
  daily_request_limit INTEGER NOT NULL DEFAULT 0,
  daily_token_limit INTEGER NOT NULL DEFAULT 0,
  weekly_request_limit INTEGER NOT NULL DEFAULT 0,
  weekly_token_limit INTEGER NOT NULL DEFAULT 0,
  monthly_request_limit INTEGER NOT NULL DEFAULT 0,
  monthly_token_limit INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER DEFAULT 1,
  expires_at INTEGER,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER
);

CREATE TABLE IF NOT EXISTS api_configs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  description TEXT,
  method TEXT DEFAULT 'POST',
  url TEXT NOT NULL,
  headers TEXT,
  request_template TEXT,
  response_template TEXT,
  api_key TEXT,
  timeout_ms INTEGER DEFAULT 15000,
  /* Limits for the /free streaming tunnel only. 0 means unlimited. */
  stream_timeout_ms INTEGER NOT NULL DEFAULT 0,
  stream_max_body_mb INTEGER NOT NULL DEFAULT 0,
  /* Mount point: 'open' (API key required) or 'free' (public). */
  route TEXT DEFAULT 'open',
  enabled INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS usage_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id TEXT NOT NULL,
  api_key_id INTEGER,
  kind TEXT DEFAULT 'ai',
  model TEXT,
  provider TEXT,
  api_config_id INTEGER,
  /* Upstream URL an API-config request was forwarded to, without query. */
  target_url TEXT,
  input_tokens INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  total_tokens INTEGER DEFAULT 0,
  latency_ms INTEGER,
  status INTEGER,
  stream INTEGER DEFAULT 0,
  error TEXT,
  /* JSON snapshots for the usage-log detail view; secrets are redacted. */
  request_headers TEXT,
  upstream_headers TEXT,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  source_ip TEXT,
  created_at INTEGER NOT NULL
);

/* Per-key, per-local-day usage rollup. Quota accounting reads this table
   instead of usage_logs, so purging logs cannot shrink weekly/monthly usage.
   day is YYYYMMDD; tokens include the same total_tokens the log row stored. */
CREATE TABLE IF NOT EXISTS api_key_daily_usage (
  day INTEGER NOT NULL,
  api_key_id INTEGER NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0,
  tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, api_key_id)
);

CREATE TABLE IF NOT EXISTS admin_users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  enabled INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_login_at INTEGER
);

CREATE TABLE IF NOT EXISTS admin_sessions (
  token_hash TEXT PRIMARY KEY,
  admin_user_id INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  FOREIGN KEY(admin_user_id) REFERENCES admin_users(id)
);

CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_logs(model);
CREATE INDEX IF NOT EXISTS idx_usage_config ON usage_logs(api_config_id);
CREATE INDEX IF NOT EXISTS idx_usage_key_created ON usage_logs(api_key_id, created_at);
-- Per-key day-range scans for the weekly/monthly quota windows.
CREATE INDEX IF NOT EXISTS idx_key_daily_key ON api_key_daily_usage(api_key_id, day);
-- Covers the all-time per-config aggregate (configs page): every selected
-- column lives in the index, so SQLite never touches the table rows.
CREATE INDEX IF NOT EXISTS idx_usage_config_cover ON usage_logs(kind, api_config_id, status, latency_ms);
`

// Migrate creates the schema and applies the incremental column additions that
// existing databases from the reference gateway need. SQLite has no
// ADD COLUMN IF NOT EXISTS, so ensureColumn checks PRAGMA table_info first.
func Migrate(db *sql.DB) error {
	if err := Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}

		// Per-key quota columns: 0 means "no limit" for every period.
		for _, column := range []string{
			"daily_request_limit",
			"daily_token_limit",
			"weekly_request_limit",
			"weekly_token_limit",
			"monthly_request_limit",
			"monthly_token_limit",
		} {
			if err := ensureColumn(tx, "api_keys", column, "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
		}

		// Added with provider custom headers.
		if err := ensureColumn(tx, "providers", "headers", "TEXT"); err != nil {
			return err
		}
		// Added with the forwarding usage column.
		if err := ensureColumn(tx, "usage_logs", "target_url", "TEXT"); err != nil {
			return err
		}
		// Added with request-header snapshots in the usage details view.
		if err := ensureColumn(tx, "usage_logs", "request_headers", "TEXT"); err != nil {
			return err
		}
		if err := ensureColumn(tx, "usage_logs", "upstream_headers", "TEXT"); err != nil {
			return err
		}
		// Added with the /free mount: pre-existing configs were all /open.
		if err := ensureColumn(tx, "api_configs", "route", "TEXT DEFAULT 'open'"); err != nil {
			return err
		}
		// Added with the /free streaming tunnel.
		if err := ensureColumn(tx, "api_configs", "stream_timeout_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := ensureColumn(tx, "api_configs", "stream_max_body_mb", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		// Added with account roles; pre-existing accounts were full admins.
		return ensureColumn(tx, "admin_users", "role", "TEXT NOT NULL DEFAULT 'admin'")
	}); err != nil {
		return err
	}

	// Seed api_key_daily_usage from existing logs on the first start after the
	// rollup was introduced, so quotas keep working without waiting a day.
	if err := backfillDailyUsage(); err != nil {
		slog.Warn("backfill daily key usage", "error", err)
	}

	// Seed the in-memory dashboard counters only after the schema exists: Open
	// runs before Migrate on a fresh database. A failed seed is not fatal, the
	// stats endpoints fall back to SQL.
	if err := initLiveStats(); err != nil {
		slog.Warn("seed live usage stats", "error", err)
	}
	return nil
}

// ensureColumn adds a column when it is missing.
func ensureColumn(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	type tableInfo struct {
		CID       int
		Name      string
		Type      string
		NotNull   int
		DfltValue sql.NullString
		PK        int
	}
	present := false
	for rows.Next() {
		var info tableInfo
		if err := rows.Scan(&info.CID, &info.Name, &info.Type, &info.NotNull, &info.DfltValue, &info.PK); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s columns: %w", table, err)
		}
		if info.Name == column {
			present = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}
