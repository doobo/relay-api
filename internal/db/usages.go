package db

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// UsageRecord is one usage_logs row to insert. Nil pointers become NULL.
type UsageRecord struct {
	RequestID    string
	APIKeyID     *int64
	Kind         string // "ai" or "api"
	Model        *string
	Provider     *string
	APIConfigID  *int64
	TargetURL    *string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	LatencyMs    *int64
	Status       *int
	Stream       bool
	Error        *string
}

// usageInsertSQL appends one usage_logs row. The synchronous and asynchronous
// paths share it so both write an identical shape.
const usageInsertSQL = `INSERT INTO usage_logs
	(request_id, api_key_id, kind, model, provider, api_config_id, target_url,
	 input_tokens, output_tokens, total_tokens, latency_ms, status, stream, error, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// usageInsertArgs renders a record into the placeholder arguments, stamping it
// with the current time.
func usageInsertArgs(record UsageRecord) []any {
	total := record.TotalTokens
	if total == 0 {
		total = record.InputTokens + record.OutputTokens
	}
	return []any{
		record.RequestID, nullInt(record.APIKeyID), record.Kind,
		nullString(record.Model), nullString(record.Provider), nullInt(record.APIConfigID), nullString(record.TargetURL),
		record.InputTokens, record.OutputTokens, total,
		nullInt(record.LatencyMs), nullStatus(record.Status), boolToInt(record.Stream), nullString(record.Error),
		time.Now().UnixMilli(),
	}
}

// RecordUsage appends a usage_logs row synchronously. It stays the primitive
// for callers that must see the row before returning (admin tooling, tests);
// the high-frequency tunnel path uses RecordUsageAsync instead.
func RecordUsage(record UsageRecord) error {
	_, err := Get().Exec(usageInsertSQL, usageInsertArgs(record)...)
	return err
}

// Async usage recording. /free tunnels are high-frequency (XHTTP packet mode
// splits one client into many small requests) and every row is a disk write on
// the single SQLite connection, so a synchronous INSERT on the response path
// serialises the whole gateway and pushes back the first byte of the reply.
// Records are queued here and drained by one writer goroutine into batched
// transactions, keeping the request path free of the write.
const (
	usageQueueSize  = 1024
	usageBatchSize  = 200
	usageFlushEvery = 250 * time.Millisecond
)

// usageMessage is one queue item: either a record to store, or a barrier whose
// close signals that everything queued before it has been flushed.
type usageMessage struct {
	record  UsageRecord
	barrier chan struct{}
}

type usageWriter struct {
	queue   chan usageMessage
	stopped chan struct{}
	closed  bool // guarded by usageMu
}

var (
	usageMu     sync.Mutex
	usageActive *usageWriter
)

// startUsageWriter begins the background batch writer; stopUsageWriter drains
// and halts it. They bracket the database handle's lifetime (Open/Close).
func startUsageWriter() {
	writer := &usageWriter{
		queue:   make(chan usageMessage, usageQueueSize),
		stopped: make(chan struct{}),
	}
	usageMu.Lock()
	usageActive = writer
	usageMu.Unlock()
	go writer.run()
}

func stopUsageWriter() {
	usageMu.Lock()
	writer := usageActive
	usageActive = nil
	if writer != nil {
		writer.closed = true
	}
	usageMu.Unlock()
	if writer == nil {
		return
	}
	close(writer.queue)
	<-writer.stopped
}

// RecordUsageAsync queues a record for the batch writer, falling back to a
// synchronous insert when the writer is not running or its queue is full so a
// usage row is never silently dropped.
func RecordUsageAsync(record UsageRecord) {
	usageMu.Lock()
	writer := usageActive
	if writer == nil {
		usageMu.Unlock()
		_ = RecordUsage(record)
		return
	}
	// Send while holding the lock so stopUsageWriter cannot close the queue
	// between the nil check and the send.
	select {
	case writer.queue <- usageMessage{record: record}:
		usageMu.Unlock()
	default:
		usageMu.Unlock()
		_ = RecordUsage(record)
	}
}

// FlushUsage blocks until every record queued before the call has been written,
// so callers (graceful shutdown, tests) can observe them.
func FlushUsage(ctx context.Context) error {
	usageMu.Lock()
	writer := usageActive
	if writer == nil {
		usageMu.Unlock()
		return nil
	}
	barrier := make(chan struct{})
	select {
	case writer.queue <- usageMessage{barrier: barrier}:
		usageMu.Unlock()
	case <-ctx.Done():
		usageMu.Unlock()
		return ctx.Err()
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run drains the queue until it is closed, flushing a batch whenever it fills or
// the flush interval elapses. A barrier flushes and then releases its waiter, so
// FlushUsage observes everything queued ahead of it.
func (w *usageWriter) run() {
	defer close(w.stopped)
	batch := make([]UsageRecord, 0, usageBatchSize)
	ticker := time.NewTicker(usageFlushEvery)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		insertUsageBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case message, ok := <-w.queue:
			if !ok {
				flush()
				return
			}
			if message.barrier != nil {
				flush()
				close(message.barrier)
				continue
			}
			batch = append(batch, message.record)
			if len(batch) >= usageBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// insertUsageBatch writes a slice of records in one transaction; a failure is
// logged but not retried, because usage is best-effort telemetry and must never
// stall the writer.
func insertUsageBatch(records []UsageRecord) {
	err := Transaction(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(usageInsertSQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := range records {
			if _, err := stmt.Exec(usageInsertArgs(records[i])...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("record usage batch", "count", len(records), "error", err)
	}
}

func nullStatus(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// QuotaUsage is an API key's usage for the current day/week/month.
type QuotaUsage struct {
	DayRequests   int
	DayTokens     int
	WeekRequests  int
	WeekTokens    int
	MonthRequests int
	MonthTokens   int
}

// APIKeyQuotaUsage aggregates one key's usage across the three quota periods in
// a single indexed scan.
func APIKeyQuotaUsage(keyID int64) (QuotaUsage, error) {
	now := time.Now()
	day := startOfDayMs(now)
	week := startOfWeekMs(now)
	month := startOfMonthMs(now)
	var usage QuotaUsage
	err := Get().QueryRow(
		`SELECT
			COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN total_tokens ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN total_tokens ELSE 0 END), 0)
		 FROM usage_logs
		 WHERE api_key_id = ? AND created_at >= ?`,
		day, day, week, week, month, month, keyID, month,
	).Scan(
		&usage.DayRequests, &usage.DayTokens,
		&usage.WeekRequests, &usage.WeekTokens,
		&usage.MonthRequests, &usage.MonthTokens,
	)
	return usage, err
}

// StartOfDayMS returns the start of the local day, in epoch ms.
func StartOfDayMS(t time.Time) int64 { return startOfDayMs(t) }

// StartOfWeekMS returns the start of the local week (Monday 00:00), in epoch ms.
func StartOfWeekMS(t time.Time) int64 { return startOfWeekMs(t) }

// StartOfMonthMS returns the start of the local calendar month, in epoch ms.
func StartOfMonthMS(t time.Time) int64 { return startOfMonthMs(t) }

func startOfDayMs(t time.Time) int64 {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location()).UnixMilli()
}

func startOfWeekMs(t time.Time) int64 {
	year, month, day := t.Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, t.Location())
	offset := (int(start.Weekday()) + 6) % 7 // Monday = 0, Sunday = 6
	return start.AddDate(0, 0, -offset).UnixMilli()
}

func startOfMonthMs(t time.Time) int64 {
	year, month, _ := t.Date()
	return time.Date(year, month, 1, 0, 0, 0, 0, t.Location()).UnixMilli()
}

// GetAPIKeyByHash loads a key by its SHA-256 hash (nil when missing).
func GetAPIKeyByHash(hash string) (*APIKeyRow, error) {
	return scanAPIKey(Get().QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ?`, hash))
}

// lastUsedInterval throttles last_used_at writes: it is informational, and one
// write per key per minute is plenty.
const lastUsedInterval = time.Minute

var (
	lastUsedMu     sync.Mutex
	lastUsedWrites = map[int64]int64{}
)

// MarkAPIKeyUsed records last_used_at, at most once per key per minute.
func MarkAPIKeyUsed(id int64) {
	now := time.Now().UnixMilli()
	lastUsedMu.Lock()
	if last := lastUsedWrites[id]; now-last < lastUsedInterval.Milliseconds() {
		lastUsedMu.Unlock()
		return
	}
	lastUsedWrites[id] = now
	lastUsedMu.Unlock()
	_, _ = Get().Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, id)
}
