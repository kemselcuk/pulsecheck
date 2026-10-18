package store

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"pulsecheck/internal/checker"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countRows returns the number of stored results for target.
func countRows(t *testing.T, s *Store, target string) int {
	t.Helper()

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM results WHERE target = ?`, target).Scan(&n); err != nil {
		t.Fatalf("count rows for %q: %v", target, err)
	}
	return n
}

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestDeleteOlderThan(t *testing.T) {
	s, _ := openStore(t, "delete.db")
	now := time.Now()

	seed := []checker.Result{
		{TargetName: "svc", StartedAt: now.Add(-25 * time.Hour), Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "svc", StartedAt: now.Add(-26 * time.Hour), Duration: time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 500},
		{TargetName: "svc", StartedAt: now.Add(-time.Minute), Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
	}
	for _, r := range seed {
		if err := s.RecordResult(r); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
	}

	deleted, err := s.deleteOlderThan(24 * time.Hour)
	if err != nil {
		t.Fatalf("deleteOlderThan: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	history, err := s.History("svc", 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("History returned %d rows, want 1 fresh row", len(history))
	}
	assertResult(t, seed[2], history[0])

	// Running the same cleanup again deletes nothing.
	if deleted, err := s.deleteOlderThan(24 * time.Hour); err != nil || deleted != 0 {
		t.Errorf("second deleteOlderThan = (%d, %v), want (0, nil)", deleted, err)
	}
}

// TestDeleteOlderThanNonPositiveWindowIsNoOp pins the misconfiguration guard: a
// non-positive retention window must never delete anything (review finding,
// decision D21).
func TestDeleteOlderThanNonPositiveWindowIsNoOp(t *testing.T) {
	s, _ := openStore(t, "noop-window.db")

	seed := checker.Result{TargetName: "svc", StartedAt: time.Now().Add(-time.Minute), Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200}
	if err := s.RecordResult(seed); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	for _, window := range []time.Duration{0, -time.Hour} {
		deleted, err := s.deleteOlderThan(window)
		if err != nil {
			t.Fatalf("deleteOlderThan(%s): %v", window, err)
		}
		if deleted != 0 {
			t.Errorf("deleteOlderThan(%s) deleted %d rows, want 0", window, deleted)
		}
	}
	if got := countRows(t, s, "svc"); got != 1 {
		t.Errorf("rows after non-positive-window cleanups = %d, want 1", got)
	}
}

// TestStartRetentionNonPositiveWindowDisabled pins that a non-positive window
// disables retention entirely (warn + immediate done) instead of wiping rows.
func TestStartRetentionNonPositiveWindowDisabled(t *testing.T) {
	s, _ := openStore(t, "disabled-window.db")

	seed := checker.Result{TargetName: "svc", StartedAt: time.Now().Add(-time.Minute), Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200}
	if err := s.RecordResult(seed); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := s.StartRetention(ctx, 20*time.Millisecond, 0, discardLogger())

	// Well past several would-be periods: the row must still be there.
	time.Sleep(200 * time.Millisecond)
	if got := countRows(t, s, "svc"); got != 1 {
		t.Errorf("rows after disabled retention = %d, want 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done was not closed for a disabled retention loop")
	}
	if got := countRows(t, s, "svc"); got != 1 {
		t.Errorf("rows after shutdown = %d, want 1", got)
	}
}

func TestStartRetention(t *testing.T) {
	s, _ := openStore(t, "retention.db")

	// Warm the pool so the goroutine baseline below is stable.
	if _, _, err := s.Latest("warmup"); err != nil {
		t.Fatalf("warmup read: %v", err)
	}
	baselineGoroutines := runtime.NumGoroutine()

	now := time.Now()
	if err := s.RecordResult(checker.Result{
		TargetName: "svc",
		StartedAt:  now.Add(-100 * time.Millisecond),
		Duration:   time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("seed fresh row: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := s.StartRetention(ctx, 50*time.Millisecond, time.Second, discardLogger())

	// Record a row that is already past the one-second window. The initial
	// cleanup may have run before this insert, so the periodic tick removes it.
	if err := s.RecordResult(checker.Result{
		TargetName: "aged",
		StartedAt:  now.Add(-2 * time.Second),
		Duration:   time.Millisecond,
		Outcome:    checker.OutcomeDown,
		StatusCode: 500,
	}); err != nil {
		t.Fatalf("seed aged row: %v", err)
	}

	waitFor(t, 3*time.Second, "aged row to be deleted", func() bool {
		return countRows(t, s, "aged") == 0
	})
	if got := countRows(t, s, "svc"); got != 1 {
		t.Errorf("fresh rows after cleanup = %d, want 1", got)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("done channel did not close after context cancellation")
	}

	// The loop must be fully stopped: a stale row inserted after cancellation
	// has to survive several former periods.
	if err := s.RecordResult(checker.Result{
		TargetName: "after-stop",
		StartedAt:  now.Add(-2 * time.Second),
		Duration:   time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("seed row after cancellation: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := countRows(t, s, "after-stop"); got != 1 {
		t.Errorf("rows after cancellation = %d, want 1 (cleanup kept running)", got)
	}

	// No goroutine from the retention loop may outlive the done signal.
	waitFor(t, 2*time.Second, "retention goroutine to exit", func() bool {
		return runtime.NumGoroutine() <= baselineGoroutines
	})
}
