// Package config loads the relay-api runtime configuration from the
// environment. It mirrors the reference gateway's src/config/config.ts: the
// same variable names, defaults and clamping behaviour, minus H2C_PORT, which
// exists in the reference only because Bun cannot serve HTTP/1.1 and h2c on one
// listener. Go does both on PORT, so there is exactly one listening port.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// ForwardPrefix is the mount point of the API-key-protected JSON forwarder.
	ForwardPrefix = "/open"
	// FreeForwardPrefix is the public raw-streaming tunnel mount point.
	FreeForwardPrefix = "/free"
	// MethodNone is the stored api_configs.method value that pins no upstream
	// method: the client's own verb is forwarded unchanged.
	MethodNone = "NONE"
)

// ForwardRoute is the mount a non-AI config is registered on.
type ForwardRoute string

const (
	RouteOpen ForwardRoute = "open"
	RouteFree ForwardRoute = "free"
)

// ForwardRoutes lists every forwarding mount point.
var ForwardRoutes = []ForwardRoute{RouteOpen, RouteFree}

// PrefixForRoute returns the mount path a config with this route is served
// from.
func PrefixForRoute(route ForwardRoute) string {
	if route == RouteFree {
		return FreeForwardPrefix
	}
	return ForwardPrefix
}

// NormalizeRoute maps a stored route value to a known route. Missing or
// unexpected values (rows written before the column existed) count as "open".
func NormalizeRoute(route string) ForwardRoute {
	if route == string(RouteFree) {
		return RouteFree
	}
	return RouteOpen
}

// LogLevel is one of the four accepted slog severities.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Config is the fully resolved application configuration.
type Config struct {
	Port int

	DatabasePath string

	AdminToken           string
	AdminDefaultUsername string
	AdminDefaultPassword string
	AdminSessionTTLMs    int64
	// Failed logins per minute per client IP before that IP is throttled.
	AdminLoginRateLimit int

	LogLevel LogLevel

	// MaxOpenFiles is the soft RLIMIT_NOFILE the process raises itself to at
	// startup (Linux/macOS). Each active /free tunnel holds a client and an
	// upstream socket, so a few thousand tunnels need several thousand fds.
	MaxOpenFiles int

	// RequestSizeLimitBytes caps bodies on /v1/* and /open/*.
	RequestSizeLimitBytes int64
	// RequestTimeoutMs is the upstream timeout for non-streaming chat calls and
	// the default for /open/* forwards.
	RequestTimeoutMs int
	// StreamIdleTimeoutMs closes an SSE stream that produced no chunk for this
	// long.
	StreamIdleTimeoutMs int

	// ServerIdleTimeoutMs is how long a keep-alive connection may sit between
	// requests before it is closed. It only applies between requests, never to
	// an in-flight SSE stream or /free tunnel, so it can be set even though
	// those are long-lived.
	ServerIdleTimeoutMs int
	// SendTimeoutMs bounds how long a client write may make no progress before
	// the connection is closed. HTTP/2 enforces it per DATA frame
	// (HTTP2Config.WriteByteTimeout); HTTP/1.1 arms it per tunnel write.
	SendTimeoutMs int

	// UpstreamMaxRetries is the number of extra attempts per route after a
	// retryable upstream failure (network error, timeout, 408, 429, 5xx).
	// 0 disables retries and keeps the old one-shot failover behaviour.
	UpstreamMaxRetries int
	// UpstreamRetryBaseMs is the first backoff delay; it doubles per attempt.
	UpstreamRetryBaseMs int
	// UpstreamRetryMaxMs caps the backoff delay.
	UpstreamRetryMaxMs int
	// UpstreamHeaderTimeoutMs bounds the wait for response headers on
	// streaming AI calls. It stops an upstream that never answers from holding
	// the stream open until the client disconnects. 0 disables it.
	UpstreamHeaderTimeoutMs int
	// UpstreamBreakerThreshold is the number of consecutive failed requests
	// after which a provider is skipped for the cooldown. 0 disables the
	// circuit breaker.
	UpstreamBreakerThreshold int
	// UpstreamBreakerCooldownMs is how long an open breaker rejects requests
	// before admitting a single probe.
	UpstreamBreakerCooldownMs int

	// AccessLogSkipPrefixes are request path prefixes whose access-log line is
	// suppressed (the tunnel is high-frequency by design).
	AccessLogSkipPrefixes []string
	// TunnelUsageSample records one in N successful /free requests; 1 records
	// every request.
	TunnelUsageSample int
	// TunnelCopyBufferKB is the /free relay's copy chunk size in KiB. Each
	// concurrent tunnel holds one chunk for the whole response, so raising it
	// trades memory for fewer syscalls; see the README for the arithmetic.
	TunnelCopyBufferKB int
	// H2CMaxConcurrentStreams is the per-connection HTTP/2 stream limit
	// advertised to clients. XHTTP fans one client into many streams; the value
	// is connection-level SETTINGS, so it applies to new connections only and
	// needs a restart.
	H2CMaxConcurrentStreams int
	// UsageHeaderSnapshots stores request/upstream header JSON on every usage
	// row. Disabling it saves CPU and keeps usage_logs small.
	UsageHeaderSnapshots bool

	// LogRetentionDays is how many days of usage/audit logs to keep; 0 disables
	// the cleanup. Quotas do not depend on these logs (see api_key_daily_usage),
	// so this can be short even when monthly quotas are in use.
	LogRetentionDays int
	// LogCleanupTime is the local "HH:MM" time the daily cleanup runs.
	LogCleanupTime string
}

// Load reads the configuration from the process environment, applying the same
// defaults as the reference implementation.
func Load() Config {
	databasePath := getenv("DATABASE_PATH", "./data/gateway.db")
	ensureParentDir(databasePath)

	return Config{
		Port:                      intEnv("PORT", 5630),
		DatabasePath:              databasePath,
		AdminToken:                os.Getenv("ADMIN_TOKEN"),
		AdminDefaultUsername:      getenv("ADMIN_DEFAULT_USERNAME", "admin"),
		AdminDefaultPassword:      getenv("ADMIN_DEFAULT_PASSWORD", "admin123"),
		AdminSessionTTLMs:         int64(intEnv("ADMIN_SESSION_TTL_HOURS", 24)) * 3_600_000,
		AdminLoginRateLimit:       intEnv("ADMIN_LOGIN_RATE_LIMIT", 10),
		LogLevel:                  parseLogLevel(os.Getenv("LOG_LEVEL")),
		MaxOpenFiles:              intEnv("MAX_OPEN_FILES", 65535),
		RequestSizeLimitBytes:     int64(intEnv("REQUEST_SIZE_LIMIT_MB", 10)) * 1024 * 1024,
		RequestTimeoutMs:          intEnv("REQUEST_TIMEOUT_MS", 120_000),
		StreamIdleTimeoutMs:       intEnv("STREAM_IDLE_TIMEOUT_MS", 60_000),
		ServerIdleTimeoutMs:       intEnv("SERVER_IDLE_TIMEOUT_MS", 75_000),
		SendTimeoutMs:             intEnv("SEND_TIMEOUT_MS", 60_000),
		UpstreamMaxRetries:        nonNegativeIntEnv("UPSTREAM_MAX_RETRIES", 1),
		UpstreamRetryBaseMs:       intEnv("UPSTREAM_RETRY_BASE_MS", 250),
		UpstreamRetryMaxMs:        intEnv("UPSTREAM_RETRY_MAX_MS", 4_000),
		UpstreamHeaderTimeoutMs:   intEnv("UPSTREAM_HEADER_TIMEOUT_MS", 30_000),
		UpstreamBreakerThreshold:  nonNegativeIntEnv("UPSTREAM_BREAKER_THRESHOLD", 5),
		UpstreamBreakerCooldownMs: intEnv("UPSTREAM_BREAKER_COOLDOWN_MS", 30_000),
		AccessLogSkipPrefixes:     splitPrefixes(getenv("ACCESS_LOG_SKIP_PREFIXES", "/free")),
		TunnelUsageSample:         intEnv("TUNNEL_USAGE_SAMPLE", 10),
		TunnelCopyBufferKB:        intEnv("TUNNEL_COPY_BUFFER_KB", 64),
		H2CMaxConcurrentStreams:   intEnv("H2C_MAX_CONCURRENT_STREAMS", 2048),
		UsageHeaderSnapshots:      boolEnv("USAGE_HEADER_SNAPSHOTS", true),
		LogRetentionDays:          retentionDays(),
		LogCleanupTime:            parseCleanupTime(os.Getenv("LOG_CLEANUP_TIME")),
	}
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// intEnv parses a positive integer, falling back on missing/invalid input. This
// matches the reference intEnv, where non-positive values mean "unset".
func intEnv(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// nonNegativeIntEnv parses an integer that may legitimately be zero (0 disables
// a feature), falling back on missing/invalid input.
func nonNegativeIntEnv(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// splitPrefixes parses a comma-separated path-prefix list, dropping blanks.
func splitPrefixes(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// boolEnv parses a boolean environment variable, accepting the usual spellings
// and falling back on blank/invalid input.
func boolEnv(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseLogLevel(raw string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return LogDebug
	case "warn":
		return LogWarn
	case "error":
		return LogError
	default:
		return LogInfo
	}
}

// retentionDays allows 0 (cleanup disabled), unlike intEnv.
func retentionDays() int {
	raw := strings.TrimSpace(os.Getenv("LOG_RETENTION_DAYS"))
	if raw == "" {
		return 7
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 7
	}
	return n
}

// parseCleanupTime accepts "HH:MM" local time; anything else falls back to
// 03:00.
func parseCleanupTime(raw string) string {
	if isClockTime(raw) {
		return raw
	}
	return "03:00"
}

// CleanupHourMinute returns the validated cleanup time as hour/minute. The
// value is normalised by Load, so this always yields a valid local time.
func (c Config) CleanupHourMinute() (int, int) {
	if !isClockTime(c.LogCleanupTime) {
		return 3, 0
	}
	hour, _ := strconv.Atoi(c.LogCleanupTime[0:2])
	minute, _ := strconv.Atoi(c.LogCleanupTime[3:5])
	return hour, minute
}

func isClockTime(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	hour, okHour := twoDigits(value[0:2])
	minute, okMinute := twoDigits(value[3:5])
	return okHour && okMinute && hour <= 23 && minute <= 59
}

func twoDigits(value string) (int, bool) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > 99 || len(value) != 2 {
		return 0, false
	}
	return n, true
}

// ensureParentDir creates the database's parent directory, matching the
// reference config loader. Failures are ignored so SQLite can report the real
// problem.
func ensureParentDir(databasePath string) {
	dir := filepath.Dir(databasePath)
	if dir == "" || dir == "." {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
}
