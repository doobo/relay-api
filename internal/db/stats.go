package db

import (
	"database/sql"
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
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	LatencyMs    *int64
	Status       *int
	Stream       bool
	Error        *string
	CreatedAt    int64
}

// UsageFilters narrows a usage-log listing. Zero values mean "no filter".
type UsageFilters struct {
	Limit    int
	Offset   int
	Model    string
	Provider string
	Kind     string
	Status   *int
	Since    *int64
}

const usageLogColumns = `id, request_id, api_key_id, kind, model, provider, api_config_id, target_url,
	input_tokens, output_tokens, total_tokens, latency_ms, status, stream, error, created_at`

func scanUsageLog(row interface{ Scan(...any) error }) (*UsageLogRow, error) {
	var (
		entry       UsageLogRow
		apiKeyID    sql.NullInt64
		model       sql.NullString
		provider    sql.NullString
		configID    sql.NullInt64
		targetURL   sql.NullString
		latency     sql.NullInt64
		status      sql.NullInt64
		stream      int
		errorDetail sql.NullString
	)
	if err := row.Scan(
		&entry.ID, &entry.RequestID, &apiKeyID, &entry.Kind, &model, &provider, &configID, &targetURL,
		&entry.InputTokens, &entry.OutputTokens, &entry.TotalTokens, &latency, &status, &stream, &errorDetail,
		&entry.CreatedAt,
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
	entry.Stream = stream != 0
	return &entry, nil
}

// ListUsageLogs returns usage rows newest-first, honoring the given filters.
func ListUsageLogs(filters UsageFilters) ([]UsageLogRow, error) {
	conditions := make([]string, 0, 5)
	params := make([]any, 0, 8)
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
	if filters.Status != nil {
		conditions = append(conditions, "status = ?")
		params = append(params, *filters.Status)
	}
	if filters.Since != nil {
		conditions = append(conditions, "created_at >= ?")
		params = append(params, *filters.Since)
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

	rows, err := Get().Query(
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
// the current day.
func TodayStatsSince() (TodayStats, error) {
	var stats TodayStats
	err := Get().QueryRow(
		`SELECT
			COUNT(*) AS requests,
			COALESCE(SUM(total_tokens), 0) AS tokens,
			COALESCE(SUM(CASE WHEN status >= 400 OR status IS NULL THEN 1 ELSE 0 END), 0) AS errors,
			COALESCE(AVG(latency_ms), 0) AS avg_latency_ms
		 FROM usage_logs
		 WHERE created_at >= ?`,
		StartOfDayMS(time.Now()),
	).Scan(&stats.Requests, &stats.Tokens, &stats.Errors, &stats.AvgLatencyMs)
	return stats, err
}

// ModelStat is one model's counters for the current day.
type ModelStat struct {
	Model    string
	Requests int
	Tokens   int
}

// ModelStatsToday returns today's per-model counters, busiest first.
func ModelStatsToday() ([]ModelStat, error) {
	rows, err := Get().Query(
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
func ProviderStatsToday() ([]ProviderStat, error) {
	rows, err := Get().Query(
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

// APIConfigStatsFor aggregates one config's all-time request counters.
func APIConfigStatsFor(configID int64) (APIConfigStats, error) {
	var stats APIConfigStats
	err := Get().QueryRow(
		`SELECT COUNT(*) AS requests,
			COALESCE(SUM(CASE WHEN status >= 400 OR status IS NULL THEN 1 ELSE 0 END), 0) AS errors,
			COALESCE(AVG(latency_ms), 0) AS avg_latency_ms
		 FROM usage_logs
		 WHERE kind = 'api' AND api_config_id = ?`,
		configID,
	).Scan(&stats.Requests, &stats.Errors, &stats.AvgLatencyMs)
	stats.APIConfigID = configID
	return stats, err
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
	rows, err := Get().Query(
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

// PurgeOldLogs deletes usage_logs and audit_logs older than retentionDays and
// reports how many rows each deletion removed.
func PurgeOldLogs(retentionDays int) (usage int64, audit int64, err error) {
	cutoff := time.Now().UnixMilli() - int64(retentionDays)*86_400_000
	usageResult, err := Get().Exec(`DELETE FROM usage_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	auditResult, err := Get().Exec(`DELETE FROM audit_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	usage, err = usageResult.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	audit, err = auditResult.RowsAffected()
	return usage, audit, err
}
