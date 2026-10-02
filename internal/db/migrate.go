package db

import (
	"database/sql"
	"fmt"
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
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT NOT NULL,
  target TEXT NOT NULL,
  source_ip TEXT,
  created_at INTEGER NOT NULL
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
`

// Migrate creates the schema and applies the incremental column additions that
// existing databases from the reference gateway need. SQLite has no
// ADD COLUMN IF NOT EXISTS, so ensureColumn checks PRAGMA table_info first.
func Migrate(db *sql.DB) error {
	return Transaction(func(tx *sql.Tx) error {
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

		// Added with the forwarding usage column.
		if err := ensureColumn(tx, "usage_logs", "target_url", "TEXT"); err != nil {
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
	})
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
