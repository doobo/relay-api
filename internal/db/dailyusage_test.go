package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openDailyUsageTestDB(t *testing.T) {
	t.Helper()
	if _, err := Open(filepath.Join(t.TempDir(), "daily.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = Close() })
	if err := Migrate(Get()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestQuotaUsageSurvivesLogPurge is the behaviour the rollup exists for:
// deleting usage_logs (retention) must not shrink weekly/monthly quota usage.
func TestQuotaUsageSurvivesLogPurge(t *testing.T) {
	openDailyUsageTestDB(t)

	key, err := CreateAPIKey(NewAPIKey{
		Name: "quota", Prefix: "sk-q", KeyHash: "hash-q", Scope: "both",
		DailyRequestLimit: 10, MonthlyRequestLimit: 100,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyID := key.ID
	status := 200
	if err := RecordUsage(UsageRecord{
		RequestID: "r1", APIKeyID: &keyID, Kind: "ai", InputTokens: 3, OutputTokens: 4, Status: &status,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	assertCounts := func(label string) {
		t.Helper()
		forgetQuotaCounter(keyID)
		usage, err := APIKeyQuotaUsageCached(keyID)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if usage.DayRequests != 1 || usage.DayTokens != 7 ||
			usage.WeekRequests != 1 || usage.MonthRequests != 1 {
			t.Fatalf("%s: usage = %+v, want 1 request / 7 tokens in all periods", label, usage)
		}
	}
	assertCounts("before purge")

	if _, err := Get().Exec(`DELETE FROM usage_logs`); err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	assertCounts("after log purge")
}

// TestQuotaWindowsFromDailyRollup checks the day/week/month boundaries against
// rows written directly into the rollup, including a week that began in the
// previous month.
func TestQuotaWindowsFromDailyRollup(t *testing.T) {
	openDailyUsageTestDB(t)

	now := time.Now()
	today := dayKey(now)
	weekStart := dayKeyAt(startOfWeekMs(now))
	monthStart := dayKeyAt(startOfMonthMs(now))

	type counts struct{ requests, tokens int }
	entries := map[int64]counts{today: {1, 10}}
	if weekStart < today {
		if _, exists := entries[weekStart]; !exists {
			entries[weekStart] = counts{2, 20}
		}
	}
	if monthStart < today {
		if _, exists := entries[monthStart]; !exists {
			entries[monthStart] = counts{4, 40}
		}
	}
	for day, entry := range entries {
		if _, err := Get().Exec(
			`INSERT INTO api_key_daily_usage (day, api_key_id, requests, tokens) VALUES (?, ?, ?, ?)`,
			day, int64(99), entry.requests, entry.tokens,
		); err != nil {
			t.Fatalf("insert rollup: %v", err)
		}
	}

	var wantWeek, wantMonth counts
	for day, entry := range entries {
		if day >= weekStart {
			wantWeek.requests += entry.requests
			wantWeek.tokens += entry.tokens
		}
		if day >= monthStart {
			wantMonth.requests += entry.requests
			wantMonth.tokens += entry.tokens
		}
	}

	usage, err := APIKeyQuotaUsage(99)
	if err != nil {
		t.Fatalf("quota usage: %v", err)
	}
	if usage.DayRequests != 1 || usage.DayTokens != 10 {
		t.Fatalf("day = %+v, want today's 1/10", usage)
	}
	if usage.WeekRequests != wantWeek.requests || usage.WeekTokens != wantWeek.tokens {
		t.Fatalf("week = %d/%d, want %d/%d", usage.WeekRequests, usage.WeekTokens, wantWeek.requests, wantWeek.tokens)
	}
	if usage.MonthRequests != wantMonth.requests || usage.MonthTokens != wantMonth.tokens {
		t.Fatalf("month = %d/%d, want %d/%d", usage.MonthRequests, usage.MonthTokens, wantMonth.requests, wantMonth.tokens)
	}
}

func TestUsageWriterUpdatesDailyRollup(t *testing.T) {
	openDailyUsageTestDB(t)

	key, err := CreateAPIKey(NewAPIKey{Name: "k", Prefix: "sk-k", KeyHash: "hash-k", Scope: "both"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyID := key.ID
	RecordUsageAsync(UsageRecord{RequestID: "a1", APIKeyID: &keyID, Kind: "ai", InputTokens: 2, OutputTokens: 5})
	if err := FlushUsage(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var requests, tokens int
	if err := Get().QueryRow(
		`SELECT requests, tokens FROM api_key_daily_usage WHERE day = ? AND api_key_id = ?`,
		dayKey(time.Now()), keyID,
	).Scan(&requests, &tokens); err != nil {
		t.Fatalf("query rollup: %v", err)
	}
	if requests != 1 || tokens != 7 {
		t.Fatalf("rollup = %d/%d, want 1/7", requests, tokens)
	}
}

func TestBackfillDailyUsageFromLogs(t *testing.T) {
	openDailyUsageTestDB(t)

	createdAt := time.Now().Add(-time.Hour).UnixMilli()
	if _, err := Get().Exec(
		`INSERT INTO usage_logs (request_id, api_key_id, kind, total_tokens, created_at) VALUES (?, ?, 'ai', ?, ?)`,
		"old-row", int64(7), 12, createdAt,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	if err := backfillDailyUsage(); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var requests, tokens int
	if err := Get().QueryRow(
		`SELECT requests, tokens FROM api_key_daily_usage WHERE day = ? AND api_key_id = 7`,
		dayKey(time.UnixMilli(createdAt)),
	).Scan(&requests, &tokens); err != nil {
		t.Fatalf("query rollup: %v", err)
	}
	if requests != 1 || tokens != 12 {
		t.Fatalf("rollup = %d/%d, want 1/12", requests, tokens)
	}

	// A second backfill must not double count.
	if err := backfillDailyUsage(); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if err := Get().QueryRow(
		`SELECT requests FROM api_key_daily_usage WHERE day = ? AND api_key_id = 7`,
		dayKey(time.UnixMilli(createdAt)),
	).Scan(&requests); err != nil {
		t.Fatalf("query rollup again: %v", err)
	}
	if requests != 1 {
		t.Fatalf("requests after second backfill = %d, want 1", requests)
	}
}

func TestPurgeOldLogsZeroKeepsEverything(t *testing.T) {
	openDailyUsageTestDB(t)

	old := time.Now().AddDate(0, 0, -10).UnixMilli()
	if _, err := Get().Exec(
		`INSERT INTO usage_logs (request_id, kind, created_at) VALUES ('old', 'ai', ?)`, old,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	usage, audit, err := PurgeOldLogs(0)
	if err != nil {
		t.Fatalf("purge 0: %v", err)
	}
	if usage != 0 || audit != 0 {
		t.Fatalf("purge 0 deleted %d/%d rows, want none", usage, audit)
	}
	var count int
	if err := Get().QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("usage rows = %d, want 1", count)
	}

	usage, _, err = PurgeOldLogs(3)
	if err != nil {
		t.Fatalf("purge 3: %v", err)
	}
	if usage != 1 {
		t.Fatalf("purge 3 deleted %d rows, want 1", usage)
	}
}

func TestNextCleanupDelay(t *testing.T) {
	now := time.Date(2026, 1, 2, 2, 0, 0, 0, time.Local)
	if delay := nextCleanupDelay(now, 3, 0); delay != time.Hour {
		t.Fatalf("before the schedule: delay = %v, want 1h", delay)
	}
	now = time.Date(2026, 1, 2, 4, 0, 0, 0, time.Local)
	if delay := nextCleanupDelay(now, 3, 0); delay != 23*time.Hour {
		t.Fatalf("after the schedule: delay = %v, want 23h", delay)
	}
	if delay := nextCleanupDelay(now, 99, 99); delay != 23*time.Hour {
		t.Fatalf("invalid schedule fallback: delay = %v, want 23h", delay)
	}
}
