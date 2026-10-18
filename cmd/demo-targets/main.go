// Command demo-targets runs a small local HTTP server with fixed endpoints so
// PulseCheck can be demonstrated and tested without reaching the internet.
//
// Routes:
//
//	GET /       -> 200, a short text index of the available routes
//	GET /ok     -> 200 "ok"
//	GET /broken -> 500 "broken"
//	GET /slow   -> 200 "slow" after a configurable delay (default 8s)
//
// The /slow handler waits on the request context, so it returns as soon as the
// client disconnects or the request is cancelled. The server shuts down
// gracefully on SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const shutdownTimeout = 5 * time.Second

func main() {
	addr := flag.String("addr", ":18081", "address the demo server listens on")
	slowDelay := flag.Duration("slow-delay", 8*time.Second, "how long GET /slow waits before responding")
	flag.Parse()

	if err := run(*addr, *slowDelay, os.Stderr); err != nil {
		slog.Error("demo-targets: server stopped", "err", err)
		os.Exit(1)
	}
}

// run serves the demo routes on addr until interrupted. It returns nil after a
// clean shutdown and an error if the listener fails.
func run(addr string, slowDelay time.Duration, logOutput io.Writer) error {
	logger := slog.New(slog.NewTextHandler(logOutput, nil))

	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(logger, newMux(slowDelay)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("demo-targets: listening", "addr", addr, "slow_delay", slowDelay)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("demo-targets: shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("demo-targets: shutdown: %w", err)
	}
	logger.Info("demo-targets: shutdown complete")

	return nil
}

// newMux builds the demo route table. It is separated from main so tests can
// exercise the handlers without binding a port.
func newMux(slowDelay time.Duration) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, `demo-targets

GET /ok     -> 200 "ok"
GET /broken -> 500 "broken"
GET /slow   -> 200 "slow" after %s
`, slowDelay)
	})

	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("/broken", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusInternalServerError, "broken")
	})

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(slowDelay)
		defer timer.Stop()

		select {
		case <-timer.C:
			writeText(w, http.StatusOK, "slow")
		case <-r.Context().Done():
			// The client went away (or cancelled); stop waiting immediately.
		}
	})

	return mux
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}

// statusRecorder remembers the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// logRequests logs method, path, status, and duration for every request.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}
