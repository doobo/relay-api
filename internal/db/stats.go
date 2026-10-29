package db

import (
	"database/sql"
	"runtime"
	"strings"
	"time"
)

// ------------------------------------------------------------- usage logs

// UsageLogRow is one usage_logs row as returned to the admin UI. Column names
// and nullability match the reference implementation's raw row shape.
type UsageLogRow struct {
	ID           int64
	RequestID    string
	APIKeyID     *int64
	Kind         string
	Model        *string
	Provider     *string
	APIConfigID  *int64
	TargetURL    *string
	ClientIP     *string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	LatencyMs    *int64
	Status       *int
	Stream       bool
	Error        *string
	// RequestHeaders/UpstreamHeaders are JSON snapshots, nil for older rows.
	RequestHeaders  *string
	UpstreamHeaders *string
	CreatedAt       int64
}

// UsageFilters narrows a usage-log listing. Zero values mean "no filter".
type UsageFilters struct {
	Limit    int
	Offset   int
	Model    string
	Provider string
	Kind     string
	// ClientIP matches the caller address by substring, so a partial IPv4 or
	// IPv6 prefix narrows the log instead of requiring the exact address.
	ClientIP string
	Status   *int
	Since    *int64
	// APIKeyID narrows to one client key (nil = every key).
	APIKeyID *int64
}

const usageLogColumns = `id, request_id, api_key_id, kind, model, provider, api_config_id, target_url, client_ip,
	input_tokens, output_tokens, total_tokens, latency_ms, status, stream, error,
	request_headers, upstream_headers, created_at`

func scanUsageLog(row interface{ Scan(...any) error }) (*UsageLogRow, error) {
	var (
		entry           UsageLogRow
		apiKeyID        sql.NullInt64
		model           sql.NullString
		provider        sql.NullString
		configID        sql.NullInt64
		targetURL       sql.NullString
		clientIP        sql.NullString
		latency         sql.NullInt64
		status          sql.NullInt64
		stream          int
		errorDetail     sql.NullString
		requestHeaders  sql.NullString
		upstreamHeaders sql.NullString
	)
	if err := row.Scan(
		&entry.ID, &entry.RequestID, &apiKeyID, &entry.Kind, &model, &provider, &configID, &targetURL, &clientIP,
		&entry.InputTokens, &entry.OutputTokens, &entry.TotalTokens, &latency, &status, &stream, &errorDetail,
		&requestHeaders, &upstreamHeaders, &entry.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if apiKeyID.Valid {
		entry.APIKeyID = &apiKeyID.Int64
	}
	if model.Valid {
		entry.Model = &model.String
	}
	if provider.Valid {
		entry.Provider = &provider.String
	}
	if configID.Valid {
		entry.APIConfigID = &configID.Int64
	}
	if targetURL.Valid {
		entry.TargetURL = &targetURL.String
	}
	if clientIP.Valid {
		entry.ClientIP = &clientIP.String
	}
	if latency.Valid {
		entry.LatencyMs = &latency.Int64
	}
	if status.Valid {
		value := int(status.Int64)
		entry.Status = &value
	}
	if errorDetail.Valid {
		entry.Error = &errorDetail.String
	}
	if requestHeaders.Valid {
		entry.RequestHeaders = &requestHeaders.String
	}
	if upstreamHeaders.Valid {
		entry.UpstreamHeaders = &upstreamHeaders.String
	}
	entry.Stream = stream != 0
	return &entry, nil
}

// GetUsageLog loads one usage row (nil when missing).
func GetUsageLog(id int64) (*UsageLogRow, error) {
	return scanUsageLog(Read().QueryRow(`SELECT `+usageLogColumns+` FROM usage_logs WHERE id = ?`, id))
}

// ListUsageLogs returns usage rows newest-first, honoring the given filters.
func ListUsageLogs(filters UsageFilters) ([]UsageLogRow, error) {
	conditions := make([]string, 0, 7)
	params := make([]any, 0, 10)
	if filters.Model != "" {
		conditions = append(conditions, "model = ?")
		params = append(params, filters.Model)
	}
	if filters.Provider != "" {
		// The admin UI shows this column as "provider" for AI requests and as
		// the upstream URL for forwards, so one filter covers both: names match
		// exactly, URLs by substring.
		conditions = append(conditions, "(provider = ? OR target_url LIKE ?)")
		params = append(params, filters.Provider, "%"+filters.Provider+"%")
	}
	if filters.Kind != "" {
		conditions = append(conditions, "kind = ?")
		params = append(params, filters.Kind)
	}
	if filters.ClientIP != "" {
		conditions = append(conditions, "client_ip LIKE ?")
		params = append(params, "%"+filters.ClientIP+"%")
	}
	if filters.Status != nil {
		conditions = append(conditions, "status = ?")
		params = append(params, *filters.Status)
	}
	if filters.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		params = append(params, *filters.Since)
	}
	if filters.APIKeyID != nil {
		conditions = append(conditions, "api_key_id = ?")
		params = append(params, *filters.APIKeyID)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	limit := filters.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := filters.Offset
	if offset < 0 {
		offset = 0
	}
	params = append(params, limit, offset)

	rows, err := Read().Query(
		`SELECT `+usageLogColumns+` FROM usage_logs `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		params...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []UsageLogRow
	for rows.Next() {
		entry, err := scanUsageLog(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, rows.Err()
}

// ------------------------------------------------------------ today stats

// TodayStats aggregates today's usage counters for the dashboard.
type TodayStats struct {
	Requests     int
	Tokens       int
	Errors       int
	AvgLatencyMs float64
}

// TodayStatsSince aggregates usage rows created at/after the local start of
// the current day. The counters are maintained in memory by the usage writer;
// SQLite is only scanned to seed them at startup.
func TodayStatsSince() (TodayStats, error) {
	if stats, ok := liveTodayStats(); ok {
		return stats, nil
	}
	aggregate, err := queryTodayAggregate()
	if err != nil {
		return TodayStats{}, err
	}
	return aggregate.stats(), nil
}

// todayAggregate is the raw counter set needed to seed the live stats. The
// average is derived from sum/count so it can continue incrementally.
type todayAggregate struct {
	Requests     int
	Tokens       int
	Errors       int
	LatencySum   int64
	LatencyCount int64
}

// stats converts the raw aggregate into the dashboard shape.
func (a todayAggregate) stats() TodayStats {
	stats := TodayStats{Requests: a.Requests, Tokens: a.Tokens, Errors: a.Errors}
	if a.LatencyCount > 0 {
		stats.AvgLatencyMs = float64(a.LatencySum) / float64(a.LatencyCount)
	}
	return stats
}

// queryTodayAggregate is the SQLite source of truth used to seed the live
// counters at startup, and the fallback when they are unavailable.
func queryTodayAggregate() (todayAggregate, error) {
	var aggregate todayAggregate
	err := Read().QueryRow(
		`SELECT
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(CASE WHEN status >= 400 OR status IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(latency_ms), 0),
			COUNT(latency_ms)
		 FROM usage_logs
		 WHERE created_at >= ?`,
		StartOfDayMS(time.Now()),
	).Scan(
		&aggregate.Requests,
		&aggregate.Tokens,
		&aggregate.Errors,
		&aggregate.LatencySum,
		&aggregate.LatencyCount,
	)
	return aggregate, err
}

// ModelStat is one model's counters for the current day.
type ModelStat struct {
	Model    string
	Requests int
	Tokens   int
}

// ModelStatsToday returns today's per-model counters, busiest first. Served
// from the live counters; SQLite is only the startup seed.
func ModelStatsToday() ([]ModelStat, error) {
	if stats, ok := liveModelStatsToday(); ok {
		return stats, nil
	}
	return modelStatsTodaySQL()
}

// modelStatsTodaySQL is the SQLite fallback for ModelStatsToday.
func modelStatsTodaySQL() ([]ModelStat, error) {
	rows, err := Read().Query(
		`SELECT model, COUNT(*) AS requests, COALESCE(SUM(total_tokens), 0) AS tokens
		 FROM usage_logs
		 WHERE kind = 'ai' AND model IS NOT NULL AND created_at >= ?
		 GROUP BY model
		 ORDER BY requests DESC
		 LIMIT 20`,
		StartOfDayMS(time.Now()),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []ModelStat
	for rows.Next() {
		var stat ModelStat
		if err := rows.Scan(&stat.Model, &stat.Requests, &stat.Tokens); err != nil {
			return nil, err
		}
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}

// ProviderStat is one provider's counters for the current day.
type ProviderStat struct {
	Provider string
	Requests int
	Errors   int
}

// ProviderStatsToday returns today's per-provider counters, busiest first.
// Served from the live counters; SQLite is only the startup seed.
func ProviderStatsToday() ([]ProviderStat, error) {
	if stats, ok := liveProviderStatsToday(); ok {
		return stats, nil
	}
	return providerStatsTodaySQL()
}

// providerStatsTodaySQL is the SQLite fallback for ProviderStatsToday.
func providerStatsTodaySQL() ([]ProviderStat, error) {
	rows, err := Read().Query(
		`SELECT provider, COUNT(*) AS requests,
			COALESCE(SUM(CASE WHEN status >= 400 OR status IS NULL THEN 1 ELSE 0 END), 0) AS errors
		 FROM usage_logs
		 WHERE kind = 'ai' AND provider IS NOT NULL AND created_at >= ?
		 GROUP BY provider
		 ORDER BY requests DESC
		 LIMIT 20`,
		StartOfDayMS(time.Now()),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []ProviderStat
	for rows.Next() {
		var stat ProviderStat
		if err := rows.Scan(&stat.Provider, &stat.Requests, &stat.Errors); err != nil {
			return nil, err
		}
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}

// APIConfigStatsFor returns one config's aggregate. It reuses the grouped,
// briefly cached APIConfigStatsAll result instead of scanning usage_logs again
// for a single id.
func APIConfigStatsFor(configID int64) (APIConfigStats, error) {
	stats, err := APIConfigStatsAll()
	if err != nil {
		return APIConfigStats{APIConfigID: configID}, err
	}
	for _, stat := range stats {
		if stat.APIConfigID == configID {
			return stat, nil
		}
	}
	return APIConfigStats{APIConfigID: configID}, nil
}

// -------------------------------------------------------------- audit logs

// AuditLogRow is one audit_logs row.
type AuditLogRow struct {
	ID        int64
	Action    string
	Target    string
	SourceIP  *string
	CreatedAt int64
}

// ListAuditLogs returns the newest audit entries.
func ListAuditLogs(limit int) ([]AuditLogRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := Read().Query(
		`SELECT id, action, target, source_ip, created_at
		 FROM audit_logs ORDER BY created_at DESC, id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []AuditLogRow
	for rows.Next() {
		var (
			entry    AuditLogRow
			sourceIP sql.NullString
		)
		if err := rows.Scan(&entry.ID, &entry.Action, &entry.Target, &sourceIP, &entry.CreatedAt); err != nil {
			return nil, err
		}
		if sourceIP.Valid {
			entry.SourceIP = &sourceIP.String
		}
		logs = append(logs, entry)
	}
	return logs, rows.Err()
}

// purgeBatchSize bounds one DELETE so the first cleanup of a years-old table
// does not hold the single writer for minutes.
const purgeBatchSize = 10_000

// PurgeOldLogs deletes usage_logs and audit_logs older than retentionDays and
// reports how many rows each deletion removed. retentionDays <= 0 disables the
// purge: 0 means "keep forever", not "delete everything".
func PurgeOldLogs(retentionDays int) (usage int64, audit int64, err error) {
	if retentionDays <= 0 {
		return 0, 0, nil
	}
	cutoff := time.Now().UnixMilli() - int64(retentionDays)*86_400_000

	// Delete usage rows in bounded batches: each batch is its own transaction,
	// so the writer keeps serving between them and the WAL does not balloon.
	for {
		result, err := Get().Exec(
			`DELETE FROM usage_logs WHERE id IN (
				SELECT id FROM usage_logs WHERE created_at < ? LIMIT ?
			)`,
			cutoff, purgeBatchSize,
		)
		if err != nil {
			return usage, 0, err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return usage, 0, err
		}
		usage += deleted
		if deleted < purgeBatchSize {
			break
		}
		runtime.Gosched()
	}

	auditResult, err := Get().Exec(`DELETE FROM audit_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return usage, 0, err
	}
	audit, err = auditResult.RowsAffected()
	return usage, audit, err
}
