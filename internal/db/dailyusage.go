package db

import (
	"database/sql"
	"log/slog"
	"time"
)

// Weekly and monthly quotas must not depend on usage_logs: that table is
// purged by retention, while a month-long quota window needs every day in the
// window. api_key_daily_usage is a small per-key, per-day rollup written in the
// same transaction as the log row, so it:
//
//   - survives log purges (and can be kept far longer: see rollupRetentionDays);
//   - lets quota checks read completed days from a tiny table instead of
//     aggregating millions of log rows;
//   - stays exact because it is incremented with every persisted record.
//
// The current day is also present here, but enforcement treats it as a seed for
// its in-memory counter and advances that counter per request, so quota checks
// never touch the database on the hot path.
const dailyUsageUpsert = `INSERT INTO api_key_daily_usage
	(day, api_key_id, requests, tokens)
 VALUES (?, ?, ?, ?)
 ON CONFLICT(day, api_key_id) DO UPDATE SET
	requests = api_key_daily_usage.requests + excluded.requests,
	tokens   = api_key_daily_usage.tokens   + excluded.tokens`

// dailyUsageBackfillDays bounds the first-run backfill. 40 days covers the
// longest monthly window plus a week that started in the previous month.
const dailyUsageBackfillDays = 40

// rollupRetentionDays bounds api_key_daily_usage. It is intentionally much
// longer than LOG_RETENTION_DAYS so weekly/monthly quotas remain accurate.
const rollupRetentionDays = 400

// dayKey returns the local calendar day as YYYYMMDD. Integer keys compare
// chronologically and avoid timezone arithmetic in SQL range scans.
func dayKey(t time.Time) int64 {
	year, month, day := t.Date()
	return int64(year)*10000 + int64(month)*100 + int64(day)
}

// dayKeyAt converts an epoch-millisecond timestamp to its local day key.
func dayKeyAt(ms int64) int64 { return dayKey(time.UnixMilli(ms)) }

// usageTotal mirrors the total_tokens value stored on usage_logs.
func usageTotal(record UsageRecord) int {
	total := record.TotalTokens
	if total == 0 {
		total = record.InputTokens + record.OutputTokens
	}
	return total
}

// upsertDailyUsage adds the batch's quota-relevant records to the daily
// rollup. Records without an API key (the /free tunnel) do not affect quotas
// and are skipped. createdAt is the timestamp stamped on the log rows, so the
// rollup day always agrees with the log's created_at.
func upsertDailyUsage(tx *sql.Tx, records []UsageRecord, createdAt int64) error {
	type delta struct {
		requests int
		tokens   int
	}
	day := dayKey(time.UnixMilli(createdAt))
	deltas := make(map[int64]*delta)
	for _, record := range records {
		if record.APIKeyID == nil {
			continue
		}
		entry := deltas[*record.APIKeyID]
		if entry == nil {
			entry = &delta{}
			deltas[*record.APIKeyID] = entry
		}
		entry.requests++
		entry.tokens += usageTotal(record)
	}
	if len(deltas) == 0 {
		return nil
	}

	stmt, err := tx.Prepare(dailyUsageUpsert)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for keyID, entry := range deltas {
		if _, err := stmt.Exec(day, keyID, entry.requests, entry.tokens); err != nil {
			return err
		}
	}
	return nil
}

// quotaUsageFromRollup loads a key's counters for the current periods:
// completed days come from api_key_daily_usage, and today is returned as the
// seed for the caller's in-memory counter. Week/month totals include today.
func quotaUsageFromRollup(keyID int64, now time.Time) (QuotaUsage, error) {
	today := dayKey(now)
	weekDay := dayKeyAt(startOfWeekMs(now))
	monthDay := dayKeyAt(startOfMonthMs(now))

	// A week can start in the previous month, so the scan must cover both
	// windows; the old query's month lower bound silently dropped those days.
	lower := monthDay
	if weekDay < lower {
		lower = weekDay
	}

	var (
		weekRequests, weekTokens   int
		monthRequests, monthTokens int
		todayRequests, todayTokens int
	)
	err := Read().QueryRow(
		`SELECT
			COALESCE(SUM(CASE WHEN day >= ? AND day < ? THEN requests ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? AND day < ? THEN tokens   ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? AND day < ? THEN requests ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? AND day < ? THEN tokens   ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day = ? THEN requests ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day = ? THEN tokens   ELSE 0 END), 0)
		 FROM api_key_daily_usage
		 WHERE api_key_id = ? AND day >= ?`,
		weekDay, today, weekDay, today,
		monthDay, today, monthDay, today,
		today, today,
		keyID, lower,
	).Scan(
		&weekRequests, &weekTokens,
		&monthRequests, &monthTokens,
		&todayRequests, &todayTokens,
	)
	if err != nil {
		return QuotaUsage{}, err
	}
	return QuotaUsage{
		DayRequests:   todayRequests,
		DayTokens:     todayTokens,
		WeekRequests:  weekRequests + todayRequests,
		WeekTokens:    weekTokens + todayTokens,
		MonthRequests: monthRequests + todayRequests,
		MonthTokens:   monthTokens + todayTokens,
	}, nil
}

// backfillDailyUsage seeds the rollup from existing logs the first time the
// table is introduced. Without it, an upgrade would start with empty weekly and
// monthly counters even though usage_logs still holds the current window.
func backfillDailyUsage() error {
	var hasRows int
	if err := Read().QueryRow(`SELECT EXISTS(SELECT 1 FROM api_key_daily_usage)`).Scan(&hasRows); err != nil {
		return err
	}
	if hasRows != 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -dailyUsageBackfillDays).UnixMilli()
	result, err := Get().Exec(
		`INSERT OR IGNORE INTO api_key_daily_usage (day, api_key_id, requests, tokens)
		 SELECT CAST(strftime('%Y%m%d', created_at / 1000, 'unixepoch', 'localtime') AS INTEGER),
		        api_key_id,
		        COUNT(*),
		        COALESCE(SUM(total_tokens), 0)
		 FROM usage_logs
		 WHERE api_key_id IS NOT NULL AND created_at >= ?
		 GROUP BY 1, 2`,
		cutoff,
	)
	if err != nil {
		return err
	}
	if inserted, err := result.RowsAffected(); err == nil && inserted > 0 {
		slog.Info("backfilled api_key_daily_usage from usage_logs",
			"days", dailyUsageBackfillDays,
			"rows", inserted,
		)
	}
	return nil
}

// PruneDailyUsage drops rollup rows older than retentionDays. It is kept much
// longer than logs because quota accounting depends on it.
func PruneDailyUsage(retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := dayKey(time.Now().AddDate(0, 0, -retentionDays))
	_, err := Get().Exec(`DELETE FROM api_key_daily_usage WHERE day < ?`, cutoff)
	return err
}
