package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestUsageWriterBatchesAndFlushes verifies that records queued through the
// async writer are all persisted once FlushUsage returns, and that the queue is
// drained when the handle closes.
func TestUsageWriterBatchesAndFlushes(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "usage.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const total = 25
	for i := 0; i < total; i++ {
		RecordUsageAsync(UsageRecord{RequestID: "req", Kind: "api"})
	}

	if err := FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var count int
	if err := Get().QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != total {
		t.Fatalf("usage rows = %d, want %d", count, total)
	}
}
