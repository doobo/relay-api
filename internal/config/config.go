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

	// RequestSizeLimitBytes caps bodies on /v1/* and /open/*.
	RequestSizeLimitBytes int64
	// RequestTimeoutMs is the upstream timeout for non-streaming chat calls and
	// the default for /open/* forwards.
	RequestTimeoutMs int
	// StreamIdleTimeoutMs closes an SSE stream that produced no chunk for this
	// long.
	StreamIdleTimeoutMs int

	// LogRetentionDays is how many days of usage/audit logs to keep; 0 disables
	// the cleanup job.
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
		Port:                  intEnv("PORT", 5630),
		DatabasePath:          databasePath,
		AdminToken:            os.Getenv("ADMIN_TOKEN"),
		AdminDefaultUsername:  getenv("ADMIN_DEFAULT_USERNAME", "admin"),
		AdminDefaultPassword:  getenv("ADMIN_DEFAULT_PASSWORD", "admin123"),
		AdminSessionTTLMs:     int64(intEnv("ADMIN_SESSION_TTL_HOURS", 24)) * 3_600_000,
		AdminLoginRateLimit:   intEnv("ADMIN_LOGIN_RATE_LIMIT", 10),
		LogLevel:              parseLogLevel(os.Getenv("LOG_LEVEL")),
		RequestSizeLimitBytes: int64(intEnv("REQUEST_SIZE_LIMIT_MB", 10)) * 1024 * 1024,
		RequestTimeoutMs:      intEnv("REQUEST_TIMEOUT_MS", 120_000),
		StreamIdleTimeoutMs:   intEnv("STREAM_IDLE_TIMEOUT_MS", 60_000),
		LogRetentionDays:      retentionDays(),
		LogCleanupTime:        parseCleanupTime(os.Getenv("LOG_CLEANUP_TIME")),
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
