package db

import (
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

// RecordUsage appends a usage_logs row.
func RecordUsage(record UsageRecord) error {
	total := record.TotalTokens
	if total == 0 {
		total = record.InputTokens + record.OutputTokens
	}
	_, err := Get().Exec(
		`INSERT INTO usage_logs
			(request_id, api_key_id, kind, model, provider, api_config_id, target_url,
			 input_tokens, output_tokens, total_tokens, latency_ms, status, stream, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.RequestID, nullInt(record.APIKeyID), record.Kind,
		nullString(record.Model), nullString(record.Provider), nullInt(record.APIConfigID), nullString(record.TargetURL),
		record.InputTokens, record.OutputTokens, total,
		nullInt(record.LatencyMs), nullStatus(record.Status), boolToInt(record.Stream), nullString(record.Error),
		time.Now().UnixMilli(),
	)
	return err
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
