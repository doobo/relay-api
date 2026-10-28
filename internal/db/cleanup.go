package db

import (
	"log/slog"
	"sync"
	"time"
)

// StartLogCleanup runs the retention purge once at startup (so frequent
// restarts cannot skip the schedule forever) and then every day at hour:minute
// local time. It returns a stop function for graceful shutdown.
//
// retentionDays applies to usage_logs/audit_logs. The per-key daily usage
// rollup is pruned independently after rollupRetentionDays, because weekly and
// monthly quotas depend on it.
func StartLogCleanup(retentionDays, hour, minute int) func() {
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		runDailyCleanup(retentionDays)
		for {
			timer := time.NewTimer(nextCleanupDelay(time.Now(), hour, minute))
			select {
			case <-timer.C:
				runDailyCleanup(retentionDays)
			case <-stop:
				timer.Stop()
				return
			}
		}
	}()

	return func() { once.Do(func() { close(stop) }) }
}

// nextCleanupDelay returns how long to wait for the next hour:minute local
// time. An out-of-range hour/minute falls back to 03:00.
func nextCleanupDelay(now time.Time, hour, minute int) time.Duration {
	if hour < 0 || hour > 23 {
		hour = 3
	}
	if minute < 0 || minute > 59 {
		minute = 0
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// runDailyCleanup purges old logs and refreshes the quota window. Clearing the
// counters forces the next quota check per key to reload day/week/month totals
// from the rollup, which is exactly the "refresh the window daily" step: today
// starts from its persisted row and yesterday is already part of the
// week/month base.
func runDailyCleanup(retentionDays int) {
	usage, audit, err := PurgeOldLogs(retentionDays)
	if err != nil {
		slog.Error("log cleanup failed", "error", err)
		return
	}
	if err := PruneDailyUsage(rollupRetentionDays); err != nil {
		slog.Warn("daily usage rollup prune failed", "error", err)
	}
	refreshQuotaCounters()
	slog.Info("log cleanup",
		"usage_deleted", usage,
		"audit_deleted", audit,
		"retention_days", retentionDays,
		"rollup_retention_days", rollupRetentionDays,
	)
}

// refreshQuotaCounters drops the in-memory quota counters. The next request for
// each key reloads its day/week/month window from api_key_daily_usage.
func refreshQuotaCounters() {
	quotaMu.Lock()
	quotaCounters = make(map[int64]*quotaCounter)
	quotaMu.Unlock()
}
