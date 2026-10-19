package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
)

// TestSoak is the long-running integration soak (D45). It is skipped unless
// SOAK is set, because it deliberately runs for minutes:
//
//	SOAK=1 go test -race -count=1 -run TestSoak -v ./internal/store/       # 3 minutes
//	SOAK=1 SOAK_DURATION=15s go test -race -count=1 -run TestSoak ./...    # short run
//
// It runs the REAL stack — scheduler, SQLite store, retention and live
// reloads — against local HTTP servers (fast, slow, flaky) and asserts, over
// the whole run: the row count stays bounded by the retention window (the
// store never grows without limit), goroutines return to their baseline after
// shutdown, and everything is -race clean.
//
// SOAK_RETENTION overrides the retention window (default 2s) so the pruning
// assertion can be mutation-checked: running with SOAK_RETENTION=24h must
// FAIL with the bound message, because nothing is pruned. The bound itself is
// tuned to the DEFAULT 2s window and the ~60-100ms check intervals below.
func TestSoak(t *testing.T) {
	if os.Getenv("SOAK") == "" {
		t.Skip("soak test: set SOAK=1 to run it (SOAK_DURATION default 3m; SOAK_RETENTION default 2s)")
	}

	duration := envDuration(t, "SOAK_DURATION", 3*time.Minute)
	retention := envDuration(t, "SOAK_RETENTION", 2*time.Second)
	pruneEvery := retention / 4
	if pruneEvery < 50*time.Millisecond {
		pruneEvery = 50 * time.Millisecond
	}
	if pruneEvery > time.Second {
		pruneEvery = time.Second
	}

	st, err := Open(t.TempDir() + "/soak.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Three servers with distinct behaviours: always-OK, slow (in-flight
	// skips), and flaky (a mixed outcome stream).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okSrv.Close)

	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slowSrv.Close)

	var flaky atomic.Int64
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if flaky.Add(1)%3 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(flakySrv.Close)

	client := okSrv.Client()
	t.Cleanup(client.CloseIdleConnections)

	// targets builds the rotating config: the three stable targets (the flaky
	// one's interval alternates, exercising the update path) plus an optional
	// rotating extra target (add/remove paths).
	targets := func(extra string, flakyInterval time.Duration) []config.Target {
		ts := []config.Target{
			{Name: "fast", URL: okSrv.URL, Interval: 60 * time.Millisecond, Timeout: time.Second},
			{Name: "slow", URL: slowSrv.URL, Interval: 60 * time.Millisecond, Timeout: time.Second},
			{Name: "flaky", URL: flakySrv.URL, Interval: flakyInterval, Timeout: time.Second},
		}
		if extra != "" {
			ts = append(ts, config.Target{Name: extra, URL: okSrv.URL, Interval: 100 * time.Millisecond, Timeout: time.Second})
		}
		return ts
	}

	cfg := config.Config{
		Targets:  targets("", 60*time.Millisecond),
		Defaults: config.DefaultsConfig{Workers: 4},
	}

	// Baseline before the stack under test starts: the servers and the client
	// exist; the retention goroutine, the scheduler workers and every target
	// loop must be gone again after shutdown.
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	retentionDone := st.StartRetention(ctx, pruneEvery, retention, logger)

	sched := checker.New(cfg, client, st, logger)
	schedDone := make(chan error, 1)
	go func() { schedDone <- sched.Run(ctx) }()

	// Rows arrive at roughly (3 targets / 60ms + 1 extra / 100ms) ≈ 60/s; the
	// 2s design window holds ≈ 120; the bound leaves a 3x margin. Without
	// pruning, a 15s run already exceeds it by far.
	maxAllowed := int64(400)
	var maxRows int64

	rowCount := func() int64 {
		var n int64
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM results`).Scan(&n); err != nil {
			t.Errorf("count rows: %v", err)
		}
		return n
	}

	sample := time.NewTicker(250 * time.Millisecond)
	defer sample.Stop()
	rotate := time.NewTicker(2 * time.Second)
	defer rotate.Stop()

	deadline := time.After(duration)
	rotation := 0
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-sample.C:
			if n := rowCount(); n > maxRows {
				maxRows = n
			}
		case <-rotate.C:
			rotation++
			extra := fmt.Sprintf("extra-%d", rotation)
			flakyInterval := 60 * time.Millisecond
			if rotation%2 == 0 {
				flakyInterval = 80 * time.Millisecond
			}
			sched.Reload(config.Config{
				Targets:  targets(extra, flakyInterval),
				Defaults: config.DefaultsConfig{Workers: 4},
			})
		}
	}

	if n := rowCount(); n > maxRows {
		maxRows = n
	}

	// Shut the whole stack down: the scheduler first, then the retention
	// cleaner; then close idle connections so their goroutines drain.
	cancel()
	select {
	case err := <-schedDone:
		if err != nil {
			t.Fatalf("scheduler Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop within 5s")
	}
	select {
	case <-retentionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("retention cleaner did not stop within 5s")
	}
	client.CloseIdleConnections()

	// Goroutines must return to the baseline (poll: connection teardown is
	// immediate but asynchronous).
	settled := false
	deadlineG := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadlineG) {
		after = runtime.NumGoroutine()
		if after <= baseline {
			settled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !settled {
		t.Errorf("goroutines after shutdown = %d, baseline = %d (leak?)", after, baseline)
	}

	if maxRows > maxAllowed {
		t.Errorf("row count peaked at %d, bound is %d: the retention window (%v) is not pruning (SOAK_RETENTION is for mutation checks only)", maxRows, maxAllowed, retention)
	}
	t.Logf("soak done: duration=%v retention=%v maxRows=%d goroutines baseline=%d after=%d", duration, retention, maxRows, baseline, after)
}

// envDuration reads a duration from the environment, falling back to def.
func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()

	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("invalid %s=%q", name, raw)
	}
	return d
}
