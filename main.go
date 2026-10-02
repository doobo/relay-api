// Command relay-api is the Go re-implementation of mini-api-gateway: an
// OpenAI-compatible API relay with an embedded admin UI.
//
// The defining difference from the reference implementation is the listener:
// Go serves HTTP/1.1 and cleartext HTTP/2 (h2c) on the same port via
// golang.org/x/net/http2/h2c, so there is no separate H2C_PORT.
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

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
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
	setupLogger(cfg.LogLevel)

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		slog.Error("open database", "path", cfg.DatabasePath, "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	if err := db.Migrate(database); err != nil {
		slog.Error("migrate database", "error", err)
		os.Exit(1)
	}
	slog.Info("SQLite ready", "path", cfg.DatabasePath, "journal_mode", "WAL")

	if err := seedDefaultAdmin(cfg); err != nil {
		slog.Error("seed default admin", "error", err)
		os.Exit(1)
	}

	secrets, err := util.NewSecretBox()
	if err != nil {
		slog.Error("load secret encryption key", "error", err)
		os.Exit(1)
	}

	limiter := middleware.NewRateLimiter()
	challenges := util.NewChallengeStore()
	authMiddleware := middleware.NewAdminAuthMiddleware(cfg.AdminToken)

	// Resource APIs: reads are open to any authenticated account, writes are
	// admin-only via RequireAdminRole.
	resourcesMux := http.NewServeMux()
	routes.NewAdminResources(secrets, cfg.LogRetentionDays).Register(resourcesMux)
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
	v1Handler := middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(limiter)(v1Mux))

	// Non-AI JSON forwarder: /open/* keeps the same size limit and API-key auth.
	openMux := http.NewServeMux()
	routes.NewForwardRoutes(cfg, secrets, "open").Register(openMux, "/open")
	openMux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { httperr.NotFound(w) })
	openHandler := middleware.BodyParser(cfg.RequestSizeLimitBytes)(middleware.APIKeyAuth(limiter)(openMux))

	// Public raw-streaming tunnel: /free/* has deliberately no size limit or
	// API-key auth in front of it - its bodies are piped unread and its per
	// config limits are enforced inside the handler while streaming.
	freeMux := http.NewServeMux()
	routes.NewTunnelRoutes(secrets).Register(freeMux, "/free")
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

	// h2c.NewHandler sniffs each accepted connection: a cleartext HTTP/2
	// preface (PRI * HTTP/2.0) or an "Upgrade: h2c" request is served as
	// HTTP/2, everything else falls through to the standard HTTP/1.1 server.
	// One port covers both, replacing the reference's H2C_PORT.
	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           h2c.NewHandler(middleware.RequestLog(mux), h2s),
		ReadHeaderTimeout: 10 * time.Second,
		// Deliberately no WriteTimeout/ReadTimeout: SSE chat streams and the
		// /free tunnel are long-lived. Idle control lives in the handlers
		// (STREAM_IDLE_TIMEOUT_MS), not in the server.
	}

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
			slog.Error("server failed", "error", err)
			os.Exit(1)
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

// setupLogger configures the default slog logger for the requested level.
func setupLogger(level config.LogLevel) {
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
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel})))
}
