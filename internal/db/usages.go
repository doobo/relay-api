package db

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
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
	// RequestHeaders/UpstreamHeaders are JSON snapshots for the detail view.
	// Callers that still hold the raw header maps should set HeaderSnapshots
	// instead so sorting and encoding happen in the batch writer.
	RequestHeaders  *string
	UpstreamHeaders *string
	// HeaderSnapshots lazily builds the two JSON snapshots. The usage writer
	// invokes it off the request path, and a dropped queue entry skips the
	// encoding entirely.
	HeaderSnapshots func() (request, upstream *string)
}

// usageInsertSQL appends one usage_logs row. The synchronous and asynchronous
// paths share it so both write an identical shape.
const usageInsertSQL = `INSERT INTO usage_logs
	(request_id, api_key_id, kind, model, provider, api_config_id, target_url,
	 input_tokens, output_tokens, total_tokens, latency_ms, status, stream, error,
	 request_headers, upstream_headers, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// usageInsertArgs renders a record into the placeholder arguments, stamping it
// with the batch's createdAt so the log row and its daily rollup agree.
func usageInsertArgs(record UsageRecord, createdAt int64) []any {
	total := usageTotal(record)
	requestHeaders, upstreamHeaders := record.RequestHeaders, record.UpstreamHeaders
	if record.HeaderSnapshots != nil {
		requestHeaders, upstreamHeaders = record.HeaderSnapshots()
	}
	return []any{
		record.RequestID, nullInt(record.APIKeyID), record.Kind,
		nullString(record.Model), nullString(record.Provider), nullInt(record.APIConfigID), nullString(record.TargetURL),
		record.InputTokens, record.OutputTokens, total,
		nullInt(record.LatencyMs), nullStatus(record.Status), boolToInt(record.Stream), nullString(record.Error),
		nullString(requestHeaders), nullString(upstreamHeaders),
		createdAt,
	}
}

// RecordUsage appends a usage_logs row synchronously. It stays the primitive
// for callers that must see the row before returning (admin tooling, tests);
// the high-frequency tunnel path uses RecordUsageAsync instead.
func RecordUsage(record UsageRecord) error {
	noteUsage(record)
	return insertUsage(record)
}

func insertUsage(record UsageRecord) error {
	now := time.Now()
	createdAt := now.UnixMilli()
	if err := Transaction(func(tx *sql.Tx) error {
		if _, err := tx.Exec(usageInsertSQL, usageInsertArgs(record, createdAt)...); err != nil {
			return err
		}
		// The rollup shares the log row's transaction: a quota read can never
		// observe usage that was logged but not yet counted (or vice versa).
		return upsertDailyUsage(tx, []UsageRecord{record}, createdAt)
	}); err != nil {
		return err
	}
	liveStats.note(record, now)
	return nil
}

// noteUsage advances the in-memory quota counters for a record. It runs before
// the insert is queued so quota checks never have to wait for the batch
// writer, and it counts even a record that is later dropped: enforcement errs
// on the side of the quota.
func noteUsage(record UsageRecord) {
	if record.APIKeyID == nil {
		return
	}
	total := usageTotal(record)
	addAPIKeyQuotaUsage(*record.APIKeyID, total)
}

// Async usage recording. /free tunnels are high-frequency (XHTTP packet mode
// splits one client into many small requests) and every row is a disk write on
// the single SQLite connection, so a synchronous INSERT on the response path
// serialises the whole gateway and pushes back the first byte of the reply.
// Records are queued here and drained by one writer goroutine into batched
// transactions, keeping the request path free of the write.
const (
	// A deep queue absorbs XHTTP packet-mode bursts (many small requests at
	// once) without pushing a synchronous INSERT back onto the relay path.
	usageQueueSize  = 8192
	usageBatchSize  = 500
	usageFlushEvery = 250 * time.Millisecond
)

// droppedUsage counts records discarded because the queue was full. Usage is
// best-effort telemetry: blocking the relay on an INSERT is worse than losing
// a row, and the warning below (rate-limited) makes the condition visible.
var (
	droppedUsage      atomic.Uint64
	droppedUsageLogAt atomic.Int64
)

const droppedUsageLogInterval = 10 * time.Second

func noteDroppedUsage() {
	total := droppedUsage.Add(1)
	now := time.Now().UnixMilli()
	last := droppedUsageLogAt.Load()
	if now-last < droppedUsageLogInterval.Milliseconds() {
		return
	}
	if droppedUsageLogAt.CompareAndSwap(last, now) {
		slog.Warn("usage queue full; dropping telemetry", "dropped_total", total)
	}
}

// usageMessage is one queue item: either a record to store, or a barrier whose
// close signals that everything queued before it has been flushed.
type usageMessage struct {
	record   UsageRecord
	markUsed *int64
	barrier  chan struct{}
}

type usageWriter struct {
	queue   chan usageMessage
	stop    chan struct{}
	stopped chan struct{}
}

// usageActive is read on every request. Storing it in an atomic pointer keeps
// the relay path free of the global mutex that used to guard it; only startup
// and shutdown (which are serialised by the database handle) swap it.
var usageActive atomic.Pointer[usageWriter]

// startUsageWriter begins the background batch writer; stopUsageWriter drains
// and halts it. They bracket the database handle's lifetime (Open/Close).
func startUsageWriter() {
	writer := &usageWriter{
		queue:   make(chan usageMessage, usageQueueSize),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if previous := usageActive.Swap(writer); previous != nil {
		close(previous.stop)
		<-previous.stopped
	}
	go writer.run()
}

func stopUsageWriter() {
	writer := usageActive.Swap(nil)
	if writer == nil {
		return
	}
	close(writer.stop)
	<-writer.stopped
}

// RecordUsageAsync queues a record for the batch writer. When the writer is not
// running (tests, shutdown) it falls back to a synchronous insert; when the
// queue is full the record is dropped rather than blocking the request on the
// single SQLite connection.
func RecordUsageAsync(record UsageRecord) {
	noteUsage(record)
	writer := usageActive.Load()
	if writer == nil {
		_ = insertUsage(record)
		return
	}
	// The queue is never closed, so a concurrent stop cannot make this send
	// panic; the writer drains what is already queued before it exits.
	select {
	case writer.queue <- usageMessage{record: record}:
	default:
		noteDroppedUsage()
	}
}

// FlushUsage blocks until every record queued before the call has been written,
// so callers (graceful shutdown, tests) can observe them.
func FlushUsage(ctx context.Context) error {
	writer := usageActive.Load()
	if writer == nil {
		return nil
	}
	barrier := make(chan struct{})
	select {
	case writer.queue <- usageMessage{barrier: barrier}:
	case <-writer.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier:
		return nil
	case <-writer.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run drains the queue until it is stopped, flushing a batch whenever it fills
// or the flush interval elapses. A barrier flushes and then releases its waiter,
// so FlushUsage observes everything queued ahead of it.
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
	handle := func(message usageMessage) {
		if message.markUsed != nil {
			if _, err := Get().Exec(
				`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
				time.Now().UnixMilli(), *message.markUsed,
			); err != nil {
				slog.Warn("mark api key used", "id", *message.markUsed, "error", err)
			}
			return
		}
		if message.barrier != nil {
			flush()
			close(message.barrier)
			return
		}
		batch = append(batch, message.record)
		if len(batch) >= usageBatchSize {
			flush()
		}
	}
	for {
		select {
		case message := <-w.queue:
			handle(message)
		case <-ticker.C:
			flush()
		case <-w.stop:
			// Best-effort drain: everything already queued is persisted before
			// the handle is released. The queue is never closed, so producers
			// racing with shutdown cannot panic.
			for {
				select {
				case message := <-w.queue:
					handle(message)
				default:
					flush()
					return
				}
			}
		}
	}
}

// insertUsageBatch writes a slice of records in one transaction; a failure is
// logged but not retried, because usage is best-effort telemetry and must never
// stall the writer.
func insertUsageBatch(records []UsageRecord) {
	now := time.Now()
	createdAt := now.UnixMilli()
	// Build arguments (including lazy header snapshots) before opening the
	// transaction so JSON encoding never extends SQLite's write lock.
	args := make([][]any, len(records))
	for index := range records {
		args[index] = usageInsertArgs(records[index], createdAt)
	}
	err := Transaction(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(usageInsertSQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, recordArgs := range args {
			if _, err := stmt.Exec(recordArgs...); err != nil {
				return err
			}
		}
		return upsertDailyUsage(tx, records, createdAt)
	})
	if err != nil {
		slog.Error("record usage batch", "count", len(records), "error", err)
		return
	}
	for _, record := range records {
		liveStats.note(record, now)
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

// APIKeyQuotaUsage returns a key's current day/week/month counters. It reads
// the per-key daily rollup, not usage_logs, so log retention never shrinks
// quota usage. Day* is today's persisted usage, which the cached enforcement
// path uses as the seed for its in-memory today counter.
func APIKeyQuotaUsage(keyID int64) (QuotaUsage, error) {
	return quotaUsageFromRollup(keyID, time.Now())
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

// GetAPIKeyByHash loads a key by its SHA-256 hash (nil when missing). Lookups
// are served from an in-memory cache, refreshed on any api_keys write.
func GetAPIKeyByHash(hash string) (*APIKeyRow, error) {
	if key, ok := cachedAPIKey(hash); ok {
		return &key, nil
	}
	key, err := scanAPIKey(Read().QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ?`, hash))
	if err != nil || key == nil {
		return key, err
	}
	storeAPIKey(hash, *key)
	rememberQuotaTracking(*key)
	return key, nil
}

// lastUsedInterval throttles last_used_at writes: it is informational, and one
// write per key per minute is plenty.
const lastUsedInterval = time.Minute

// lastUsedWrites maps a key id to an atomic timestamp. A sync.Map plus a
// per-key atomic keeps the per-request "did we already write this minute?"
// check off a single global mutex.
var lastUsedWrites sync.Map // map[int64]*atomic.Int64

// MarkAPIKeyUsed records last_used_at, at most once per key per minute.
func MarkAPIKeyUsed(id int64) {
	now := time.Now().UnixMilli()
	value, ok := lastUsedWrites.Load(id)
	if !ok {
		value, _ = lastUsedWrites.LoadOrStore(id, &atomic.Int64{})
	}
	last := value.(*atomic.Int64)
	previous := last.Load()
	if now-previous < lastUsedInterval.Milliseconds() {
		return
	}
	if !last.CompareAndSwap(previous, now) {
		return
	}
	// last_used_at is informational; hand the UPDATE to the batch writer so a
	// request never waits on the single SQLite connection for it.
	if enqueueMarkUsed(id) {
		return
	}
	_, _ = Get().Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, id)
}

// enqueueMarkUsed hands a last_used_at update to the usage writer. It reports
// false when the writer is not running or its queue is full, in which case the
// caller writes synchronously.
func enqueueMarkUsed(id int64) bool {
	writer := usageActive.Load()
	if writer == nil {
		return false
	}
	select {
	case writer.queue <- usageMessage{markUsed: &id}:
		return true
	default:
		return false
	}
}
