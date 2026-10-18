// Command pulsecheck is the main entry point of the PulseCheck service.
//
// It wires the six internal packages — config, checker, store, api, metrics,
// web — together and owns the process lifecycle. A single signal-aware context
// is the backbone: SIGINT/SIGTERM cancels it, the checker stops because that
// context IS its context, and the HTTP server drains before the store closes.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pulsecheck/internal/api"
	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/metrics"
	"pulsecheck/internal/store"
	"pulsecheck/internal/web"
)

// version is the build version reported by /healthz, /metrics, and the startup
// log. It is a package-level var (not a const) so a release build can override
// it with -ldflags "-X main.version=<version>".
var version = "dev"

const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers; it protects the server from slowloris-style connections.
	readHeaderTimeout = 5 * time.Second

	// httpShutdownTimeout is how long in-flight HTTP requests get to finish
	// after a shutdown signal before the listener is torn down anyway.
	httpShutdownTimeout = 5 * time.Second

	// retentionPeriod is how often results older than the configured window are
	// pruned from the database.
	retentionPeriod = 10 * time.Minute

	// schedulerShutdownTimeout is the hard cap on waiting for the checker to
	// drain. Run is expected to return almost immediately because its context
	// is already cancelled; the cap only exists so a bug can never hang the
	// process forever — a timeout is reported as an honest failure (exit 1).
	schedulerShutdownTimeout = 10 * time.Second
)

func main() {
	os.Exit(run())
}

// newHandler builds the top-level route table.
//
// api.New and metrics.New register FULL patterns ("GET /api/status",
// "GET /metrics"), so mounting them needs no StripPrefix: each inner mux still
// sees the full path and matches it exactly as written. The api handler is
// mounted twice on purpose — under "/api/" for the JSON API and at "/healthz"
// for the liveness probe, which is a top-level path rather than an /api/ one.
// Both mounts share one handler, hence one process-start instant for the
// reported uptime. web.New serves the dashboard at "/" and the target detail
// pages under "/targets/".
func newHandler(cfg *config.Config, st *store.Store, version string, logger *slog.Logger) http.Handler {
	apiHandler := api.New(*cfg, st, version, logger)

	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/healthz", apiHandler)
	mux.Handle("/metrics", metrics.New(*cfg, st, version, logger))
	mux.Handle("/", web.New(*cfg, st))
	return mux
}

// run wires every component, serves until SIGINT/SIGTERM, and returns the
// process exit code. Keeping the wiring in run (instead of main) means the
// deferred cleanup below runs before os.Exit, and the whole startup and
// shutdown sequence can be read top to bottom.
//
// Wiring order (this is the part worth reading):
//
//  1. structured logger to stderr
//  2. config.Load            — fail fast, exit 1
//  3. store.Open             — fail fast, exit 1
//  4. signal context         — SIGINT/SIGTERM
//  5. Store.StartRetention   — background child of that context
//  6. checker.New + Run      — background child of that context
//  7. HTTP mux               — "/api/" → api.New, "/" → web.New
//  8. http.Server + goroutine
//  9. wait for signal, then drain HTTP, wait for the checker, close the store
func run() int {
	// 1. Structured logger to stderr, so stdout stays free for piping.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// 2. Configuration. The default points at the bundled demo config, so a
	// fresh `go run ./cmd/pulsecheck` works with no setup; point -config at a
	// custom file for real use.
	configPath := flag.String("config", "config.demo.json", "path to the JSON configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "err", err)
		return 1
	}

	// 3. Storage. Opened before the signal context so a bad database path
	// fails fast with a clear message. The deferred Close is a safety net for
	// the early-return paths; database/sql's Close is idempotent, so the
	// explicit close in the shutdown path below cannot double-close painfully.
	st, err := store.Open(cfg.Storage.DBPath)
	if err != nil {
		logger.Error("failed to open store", "db_path", cfg.Storage.DBPath, "err", err)
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("store close failed", "err", err)
		}
	}()

	// 4. One context owns the whole lifecycle: SIGINT/SIGTERM cancels it, the
	// checker stops because this is its context, and the wait loop exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 5. Retention runs as a background child of the same context and stops
	// with it.
	st.StartRetention(ctx, retentionPeriod, cfg.Storage.Retention, logger)

	// 6. Scheduler. A nil client lets checker.New use its own default; the
	// store is the Recorder. Run's contract is to return nil after a clean
	// shutdown, and it blocks until ctx is cancelled.
	sched := checker.New(*cfg, nil, st, logger)
	schedDone := make(chan error, 1)
	go func() { schedDone <- sched.Run(ctx) }()

	// 7. HTTP handler, built by newHandler so the route table can be tested
	// without binding a port.
	mux := newHandler(cfg, st, version, logger)

	// 8. HTTP server. ListenAndServe runs in a goroutine and reports over a
	// buffered channel, so it can never block — even after this function has
	// stopped reading it, the goroutine always completes and exits.
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	logger.Info("pulsecheck started",
		"version", version,
		"addr", cfg.Server.Addr,
		"targets", len(cfg.Targets),
		"workers", cfg.Defaults.Workers,
		"db_path", cfg.Storage.DBPath,
		"retention", cfg.Storage.Retention,
	)

	// 9. Wait for either a fatal server error (e.g. the address is taken) or a
	// shutdown signal.
	select {
	case err := <-srvErr:
		if err != nil {
			logger.Error("http server stopped", "err", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		logger.Info("shutdown: stopping checks")
	}

	// The signal already cancelled ctx, which IS the checker's context, so the
	// scheduler is winding down as we speak. Drain HTTP first: it has its own
	// deadline and must not be able to touch the store after we close it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown: http server", "err", err)
		return 1
	}
	logger.Info("shutdown: HTTP drained")

	// Wait for the checker, but never forever: a bug must not hang the process.
	select {
	case err := <-schedDone:
		if err != nil {
			// Run's contract is a nil error after a clean shutdown; a non-nil
			// value means something unexpected happened, so fail loudly.
			logger.Error("shutdown: checker stopped with an error", "err", err)
			return 1
		}
	case <-time.After(schedulerShutdownTimeout):
		logger.Error("shutdown: timed out waiting for checker", "timeout", schedulerShutdownTimeout)
		return 1
	}

	if err := st.Close(); err != nil {
		logger.Error("shutdown: store close", "err", err)
		return 1
	}
	logger.Info("shutdown complete")
	return 0
}
