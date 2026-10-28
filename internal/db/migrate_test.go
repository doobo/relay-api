package db

import (
	"path/filepath"
	"testing"
)

// TestTunnelDefaultsBackfill covers the one-time upgrade of /free configs that
// were created when the defaults were "unlimited": they must get the bounded
// defaults once, and a deliberate 0 set afterwards must survive a restart.
func TestTunnelDefaultsBackfill(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "tunnel.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A config stored by the old binary: both limits are 0 (unlimited).
	if _, err := Get().Exec(
		`INSERT INTO api_configs (name, method, url, timeout_ms, stream_timeout_ms, stream_max_body_mb, route, enabled, created_at, updated_at)
		 VALUES ('legacy', 'POST', 'https://example.com', 15000, 0, 0, 'free', 1, 0, 0)`,
	); err != nil {
		t.Fatalf("insert legacy config: %v", err)
	}
	// Simulate a database that predates the migration.
	if _, err := Get().Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("reset user_version: %v", err)
	}

	if err := Migrate(Get()); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	read := func() (timeout, bodyMb int) {
		t.Helper()
		if err := Read().QueryRow(
			`SELECT stream_timeout_ms, stream_max_body_mb FROM api_configs WHERE name = 'legacy'`,
		).Scan(&timeout, &bodyMb); err != nil {
			t.Fatalf("read config: %v", err)
		}
		return timeout, bodyMb
	}

	timeout, bodyMb := read()
	if timeout != DefaultStreamTimeoutMs || bodyMb != DefaultStreamMaxBodyMb {
		t.Fatalf("limits = (%d, %d), want (%d, %d)",
			timeout, bodyMb, DefaultStreamTimeoutMs, DefaultStreamMaxBodyMb)
	}

	// Once the migration has run, an explicit 0 (unlimited) must not be
	// rewritten on the next start.
	if _, err := Get().Exec(
		`UPDATE api_configs SET stream_timeout_ms = 0, stream_max_body_mb = 0 WHERE name = 'legacy'`,
	); err != nil {
		t.Fatalf("reset limits: %v", err)
	}
	if err := Migrate(Get()); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	timeout, bodyMb = read()
	if timeout != 0 || bodyMb != 0 {
		t.Fatalf("explicit unlimited was overwritten: (%d, %d)", timeout, bodyMb)
	}
}
