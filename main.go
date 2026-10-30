// Command relay-api is the Go re-implementation of mini-api-gateway: an
// OpenAI-compatible API relay with an embedded admin UI.
//
// The defining difference from the reference implementation is the listener:
// Go serves HTTP/1.1 and cleartext HTTP/2 (h2c) on the same port via net/http's
// native Protocols support, so there is no separate H2C_PORT.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/ipacl"
	"relay-api/internal/logging"
	"relay-api/internal/middleware"
	"relay-api/internal/router"
	"relay-api/internal/routes"
	"relay-api/internal/util"
	"relay-api/web"
)

// version is the build version. Release builds override it with
// -ldflags "-X main.version=..."; local builds report "dev".
var version = "dev"

func main() {
	cfg := config.Load()
	logWriter := setupLogger(cfg.LogLevel)
	defer func() { _ = logWriter.Close() }()

	// Startup failures must flush the async log buffer before os.Exit, which
	// does not run deferred calls.
	fatal := func(message string, args ...any) {
		slog.Error(message, args...)
		_ = logWriter.Close()
		os.Exit(1)
	}

	// A /free tunnel holds a client socket and an upstream socket at once, so a
	// few thousand concurrent tunnels need several thousand descriptors. Raise
	// the soft limit to the hard limit here so the deployment does not have to
	// remember LimitNOFILE (MAX_OPEN_FILES tunes the target).
	before, after, err := util.RaiseFileLimit(uint64(cfg.MaxOpenFiles))
	switch {
	case err != nil:
		slog.Debug("open-file limit unchanged", "error", err)
	case after > before:
		slog.Info("raised open-file limit", "from", before, "to", after)
	}
	if after > 0 && after < 8192 {
		slog.Warn("open-file limit is low for the /free tunnel",
			"limit", after, "hint", "raise LimitNOFILE in the unit or MAX_OPEN_FILES")
	}

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		fatal("open database", "path", cfg.DatabasePath, "error", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Migrate(database); err != nil {
		fatal("migrate database", "error", err)
	}
	slog.Info("SQLite ready", "path", cfg.DatabasePath, "journal_mode", "WAL")

	if err := seedDefaultAdmin(cfg); err != nil {
		fatal("seed default admin", "error", err)
	}

	// Load the IP allow/deny lists once; the Settings handler swaps the
	// compiled rules in place after a save, so no restart is needed. Failing
	// here is deliberate: silently running with a malformed whitelist would
	// either block everyone or enforce nothing.
	ipSettings, err := db.GetIPAccessSettings()
	if err != nil {
		fatal("load IP access settings", "error", err)
	}
	ipRules, err := ipacl.Compile(ipSettings.Whitelist, ipSettings.Blacklist)
	if err != nil {
		fatal("compile IP access settings", "error", err)
	}
	ipAccess := ipacl.NewStore(ipRules)

	secrets, err := util.NewSecretBox()
	if err != nil {
		fatal("load secret encryption key", "error", err)
	}
	// Let the config cache decrypt stored keys once instead of on every /free
	// and /open request.
	db.SetAPIConfigSecretBox(secrets)

	limiter := middleware.NewRateLimiter()
	challenges := util.NewChallengeStore()
	authMiddleware := middleware.NewAdminAuthMiddleware(cfg.AdminToken)

	// Resource APIs: reads are open to any authenticated account, writes are
	// admin-only via RequireAdminRole.
	resourcesMux := http.NewServeMux()
	routes.NewAdminResources(secrets, cfg.LogRetentionDays).Register(resourcesMux)
	routes.NewSettingsRoutes(ipAccess).Register(resourcesMux)
	resourcesMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })

	adminMux := http.NewServeMux()
	routes.NewAdminAuthRoutes(cfg, limiter, challenges).Register(adminMux)
	adminMux.Handle("/admin/", middleware.RequireAdminRole(resourcesMux))
	adminMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })

	// OpenAI-compatible API: size limit + API-key auth in front of the routes.
	resolver := router.NewResolver(secrets)
	v1Mux := http.NewServeMux()
	routes.NewGateway(cfg, resolver).Register(v1Mux)
	v1Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	v1Handler := middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(limiter)(middleware.AISession(v1Mux)))

	// Non-AI JSON forwarder: /open/* keeps the same size limit and API-key auth.
	openMux := http.NewServeMux()
	routes.NewForwardRoutes(cfg, secrets, "open").Register(openMux, "/open")
	openMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	openHandler := middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(limiter)(openMux))

	// Public raw-streaming tunnel: /free/* has deliberately no size limit or
	// API-key auth in front of it - its bodies are piped unread and its per
	// config limits are enforced inside the handler while streaming.
	freeMux := http.NewServeMux()
	// The relay's copy chunk is process-wide (the pool is shared with the SSE
	// path), so it is installed before the handler serves anything.
	if size := routes.SetTunnelCopyBufferSize(cfg.TunnelCopyBufferKB * 1024); size != cfg.TunnelCopyBufferKB*1024 {
		slog.Warn("tunnel copy buffer clamped",
			"requested_kb", cfg.TunnelCopyBufferKB, "effective_kb", size/1024)
	}
	tunnelRoutes := routes.NewTunnelRoutes(secrets)
	tunnelRoutes.SetUsageSample(cfg.TunnelUsageSample)
	tunnelRoutes.Register(freeMux, "/free")
	freeMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.Handle("/v1/", v1Handler)
	mux.Handle("/open/", openHandler)
	mux.Handle("/free/", freeMux)
	// The admin subtree authenticates every path except login/challenge.
	mux.Handle("/admin/", authMiddleware.Handler(adminMux))
	// "/" is the lowest-priority pattern, so it only catches paths no other
	// route matched: the embedded UI answers those.
	mux.Handle("/", web.Handler())

	// net/http serves HTTP/1.1 and cleartext HTTP/2 (h2c) on one listener when
	// both protocols are enabled; this replaces the x/net/http2/h2c wrapper
	// and needs no separate H2C_PORT.
	//
	// XHTTP fans one client into many parallel streams, so the per-stream and
	// per-connection receive windows are widened above the 1 MiB default to
	// keep a high-BDP cross-region link from being throttled to window/RTT,
	// and more concurrent streams are allowed per connection (default 250;
	// H2C_MAX_CONCURRENT_STREAMS, default 2048). This limit is a
	// connection-level SETTINGS value: it applies to new connections only, so a
	// change needs a restart, and it is not per api_config. Streams beyond it
	// are refused (REFUSED_STREAM), which a compliant client retries on another
	// connection; each accepted stream may hold an upload buffer plus the
	// relay's copy chunk, so raising it far has a memory cost.
	//
	// ReadTimeout/WriteTimeout stay unset: both are total-duration limits and
	// SSE plus /free tunnels are long-lived. IdleTimeout only reaps a
	// connection between requests, and WriteByteTimeout only a write that has
	// stopped making progress, so neither cuts an active stream.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           middleware.RequestLog(middleware.IPAccess(ipAccess)(mux), cfg.AccessLogSkipPrefixes...),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Duration(cfg.ServerIdleTimeoutMs) * time.Millisecond,
		// Request headers are a few KiB in practice; 64 KiB leaves room for
		// large tokens/cookies without letting a peer make the server buffer
		// megabytes before authentication.
		MaxHeaderBytes: 64 << 10,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          cfg.H2CMaxConcurrentStreams,
			MaxReadFrameSize:              64 << 10,
			MaxReceiveBufferPerConnection: 2 << 20,
			MaxReceiveBufferPerStream:     1 << 20,
			WriteByteTimeout:              time.Duration(cfg.SendTimeoutMs) * time.Millisecond,
			SendPingTimeout:               30 * time.Second,
			PingTimeout:                   15 * time.Second,
		},
		Protocols: protocols,
	}

	// Automatic log retention: run once at startup (so frequent restarts cannot
	// skip the schedule) and then daily at LOG_CLEANUP_TIME.
	cleanupHour, cleanupMinute := cfg.CleanupHourMinute()
	stopCleanup := db.StartLogCleanup(cfg.LogRetentionDays, cleanupHour, cleanupMinute)
	defer stopCleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("Relay-Api listening", "version", version, "addr", srv.Addr, "protocols", "http/1.1+h2c")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server failed", "error", err)
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}
}

// seedDefaultAdmin creates the configured default account if no account with
// that username exists yet. Existing passwords are never overwritten.
func seedDefaultAdmin(cfg config.Config) error {
	hash, err := util.HashPassword(cfg.AdminDefaultPassword)
	if err != nil {
		return err
	}
	if err := db.EnsureAdminUser(cfg.AdminDefaultUsername, hash); err != nil {
		return err
	}
	slog.Info("admin user ready", "username", cfg.AdminDefaultUsername)
	return nil
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	httperr.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// setupLogger configures the default slog logger for the requested level. The
// text handler writes through a batching async writer, so a slow stdout cannot
// block the request goroutines that produce the log line.
func setupLogger(level config.LogLevel) *logging.AsyncWriter {
	var slogLevel slog.Level
	switch level {
	case config.LogDebug:
		slogLevel = slog.LevelDebug
	case config.LogWarn:
		slogLevel = slog.LevelWarn
	case config.LogError:
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	writer := logging.NewAsyncWriter(os.Stdout, logging.Options{})
	slog.SetDefault(slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slogLevel})))
	return writer
}
