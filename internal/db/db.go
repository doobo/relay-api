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
	mu         sync.RWMutex
	handle     *sql.DB
	readHandle *sql.DB
)

// readPoolSize is the number of concurrent read-only connections. SQLite WAL
// lets readers proceed while the single writer is committing, so dashboard and
// log queries stop queueing behind usage inserts. Eight connections absorb a
// handful of concurrent admin tabs without starving auth lookups; readers are
// cheap in WAL mode.
const readPoolSize = 8

// Open creates/opens the SQLite database at path, applies the single-process
// pragmas from the reference (WAL, NORMAL sync, 5s busy timeout, foreign keys)
// and returns the write pool.
//
// Writes go through one connection (SQLite allows one writer, so this removes
// SQLITE_BUSY races). Reads use a separate, larger read-only pool (Read) so
// admin/stats queries do not serialise behind the usage writer. The pragmas are
// set via the DSN so they apply to every connection either pool opens.
func Open(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	readDB, err := sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	readDB.SetMaxOpenConns(readPoolSize)
	readDB.SetMaxIdleConns(readPoolSize)
	if err := readDB.Ping(); err != nil {
		_ = readDB.Close()
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite reader: %w", err)
	}

	mu.Lock()
	handle = sqlDB
	readHandle = readDB
	mu.Unlock()
	// Drop anything cached against a previous handle: the config set, auth
	// keys, quota counters and any resolver's routing generation.
	invalidateAPIConfigCache()
	resetHotCaches()
	startUsageWriter()
	return sqlDB, nil
}

// dsn builds a modernc.org/sqlite DSN carrying the pragmas for every
// connection. The path is encoded as a file: URL so Windows drive letters and
// spaces survive. The read-only pool omits journal/foreign-key setup (the
// writer already did it) and adds query_only so a mistaken write can never take
// the single writer lock.
func dsn(absPath string, readOnly bool) string {
	pragmas := []string{
		"busy_timeout(5000)",
		"mmap_size(268435456)",
		"cache_size(-8000)",
		"temp_store(MEMORY)",
	}
	if readOnly {
		pragmas = append(pragmas, "query_only(1)")
	} else {
		pragmas = append(pragmas,
			"synchronous(NORMAL)",
			"foreign_keys(ON)",
			"journal_mode(WAL)",
			"wal_autocheckpoint(1000)",
		)
	}
	query := "?_pragma=" + strings.Join(pragmas, "&_pragma=")
	return "file:" + url.PathEscape(filepath.ToSlash(absPath)) + query
}

// Get returns the package-level write handle. It panics before Open, which can
// only happen if the program wired itself up incorrectly.
func Get() *sql.DB {
	mu.RLock()
	defer mu.RUnlock()
	if handle == nil {
		panic("db: Open was not called")
	}
	return handle
}

// Read returns the read-only pool for queries that never write (dashboard
// aggregates, usage/audit listings, cached lookups). It falls back to the write
// handle if the reader is unavailable.
func Read() *sql.DB {
	mu.RLock()
	reader := readHandle
	mu.RUnlock()
	if reader != nil {
		return reader
	}
	return Get()
}

// Close releases both connection pools.
func Close() error {
	// Drain any queued usage rows before the handle is released.
	stopUsageWriter()
	mu.Lock()
	writer, reader := handle, readHandle
	handle, readHandle = nil, nil
	mu.Unlock()
	if writer == nil && reader == nil {
		return nil
	}
	var err error
	if reader != nil {
		err = reader.Close()
	}
	if writer != nil {
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
	}
	resetHotCaches()
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
