// Package db owns the SQLite connection and schema migration, mirroring the
// reference gateway's src/db/{db,migrate}.ts on top of database/sql and the
// pure-Go modernc.org/sqlite driver (no cgo, so the binary cross-compiles).
package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

var (
	mu     sync.RWMutex
	handle *sql.DB
)

// Open creates/opens the SQLite database at path, applies the single-process
// pragmas from the reference (WAL, NORMAL sync, 5s busy timeout, foreign keys)
// and returns the pool.
//
// The pool is capped at one connection: the gateway is single-process and
// SQLite allows one writer, so this removes SQLITE_BUSY races instead of
// fighting them. The pragmas are set via the DSN as well, so they apply to any
// connection the pool opens.
func Open(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	mu.Lock()
	handle = sqlDB
	mu.Unlock()
	return sqlDB, nil
}

// dsn builds a modernc.org/sqlite DSN carrying the pragmas for every
// connection. The path is encoded as a file: URL so Windows drive letters and
// spaces survive.
func dsn(absPath string) string {
	pragmas := []string{
		"busy_timeout(5000)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"journal_mode(WAL)",
	}
	query := "?_pragma=" + strings.Join(pragmas, "&_pragma=")
	return "file:" + url.PathEscape(filepath.ToSlash(absPath)) + query
}

// Get returns the package-level handle. It panics before Open, which can only
// happen if the program wired itself up incorrectly.
func Get() *sql.DB {
	mu.RLock()
	defer mu.RUnlock()
	if handle == nil {
		panic("db: Open was not called")
	}
	return handle
}

// Close releases the connection pool.
func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if handle == nil {
		return nil
	}
	err := handle.Close()
	handle = nil
	return err
}

// Transaction runs fn inside a transaction, rolling back on error or panic.
func Transaction(fn func(tx *sql.Tx) error) error {
	tx, err := Get().Begin()
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
