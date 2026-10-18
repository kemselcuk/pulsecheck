package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pulsecheck/internal/checker"
)

// openStore opens a database under the test's temporary directory. The nested
// data/ segment also exercises parent directory creation.
func openStore(t *testing.T, name string) (*Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "data", name)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// assertResult compares a result read back from the store with the original.
// StartedAt is compared at millisecond precision because that is the schema's
// resolution; URL is intentionally not persisted.
func assertResult(t *testing.T, want, got checker.Result) {
	t.Helper()

	if got.TargetName != want.TargetName {
		t.Errorf("TargetName = %q, want %q", got.TargetName, want.TargetName)
	}
	if got.StartedAt.UnixMilli() != want.StartedAt.UnixMilli() {
		t.Errorf("StartedAt = %v (%d ms), want %v (%d ms)",
			got.StartedAt, got.StartedAt.UnixMilli(), want.StartedAt, want.StartedAt.UnixMilli())
	}
	if got.Duration != want.Duration.Truncate(time.Millisecond) {
		t.Errorf("Duration = %v, want %v", got.Duration, want.Duration.Truncate(time.Millisecond))
	}
	if got.Outcome != want.Outcome {
		t.Errorf("Outcome = %q, want %q", got.Outcome, want.Outcome)
	}
	if got.StatusCode != want.StatusCode {
		t.Errorf("StatusCode = %d, want %d", got.StatusCode, want.StatusCode)
	}
	if got.Err != want.Err {
		t.Errorf("Err = %q, want %q", got.Err, want.Err)
	}
}

func TestOpenCreatesSchemaIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "nested", "idempotent.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// WAL is a persistent database property, so it must survive reopening.
	var journalMode string
	if err := first.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q", journalMode, "wal")
	}

	want := checker.Result{
		TargetName: "api",
		URL:        "https://example.com/health",
		StartedAt:  time.Now().Add(-time.Minute).Truncate(time.Millisecond),
		Duration:   42 * time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}
	if err := first.RecordResult(want); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	got, ok, err := second.Latest("api")
	if err != nil {
		t.Fatalf("Latest after reopen: %v", err)
	}
	if !ok {
		t.Fatal("Latest after reopen: no result, want the row written before closing")
	}
	assertResult(t, want, got)
}

func TestRecordLatestAndHistory(t *testing.T) {
	s, _ := openStore(t, "roundtrip.db")
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	cases := []checker.Result{
		{TargetName: "svc", StartedAt: base, Duration: 10 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "svc", StartedAt: base.Add(time.Second), Duration: 20 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 503},
		{TargetName: "svc", StartedAt: base.Add(2 * time.Second), Duration: 30 * time.Millisecond, Outcome: checker.OutcomeTimeout, StatusCode: 0, Err: "context deadline exceeded"},
		{TargetName: "svc", StartedAt: base.Add(3 * time.Second), Duration: 40 * time.Millisecond, Outcome: checker.OutcomeError, StatusCode: 0, Err: "connection refused"},
		{TargetName: "svc", StartedAt: base.Add(4 * time.Second), Duration: 0, Outcome: checker.OutcomeSkipped, StatusCode: 0, Err: "previous check still in flight"},
	}

	// Record one at a time and check Latest, covering every outcome.
	for _, want := range cases {
		if err := s.RecordResult(want); err != nil {
			t.Fatalf("RecordResult(%s): %v", want.Outcome, err)
		}
		got, ok, err := s.Latest("svc")
		if err != nil {
			t.Fatalf("Latest(%s): %v", want.Outcome, err)
		}
		if !ok {
			t.Fatalf("Latest(%s): not found", want.Outcome)
		}
		assertResult(t, want, got)
	}

	// Unknown target is a miss, not an error.
	if _, ok, err := s.Latest("missing"); err != nil || ok {
		t.Fatalf("Latest(missing) = (%v, %v), want (false, nil)", ok, err)
	}

	all, err := s.History("svc", 10)
	if err != nil {
		t.Fatalf("History(10): %v", err)
	}
	if len(all) != len(cases) {
		t.Fatalf("History(10) returned %d rows, want %d", len(all), len(cases))
	}
	for i := range cases {
		want := cases[len(cases)-1-i] // newest first
		assertResult(t, want, all[i])
	}

	limit, err := s.History("svc", 2)
	if err != nil {
		t.Fatalf("History(2): %v", err)
	}
	if len(limit) != 2 {
		t.Fatalf("History(2) returned %d rows, want 2", len(limit))
	}
	assertResult(t, cases[4], limit[0])
	assertResult(t, cases[3], limit[1])

	for _, n := range []int{0, -1} {
		empty, err := s.History("svc", n)
		if err != nil {
			t.Errorf("History(%d): unexpected error: %v", n, err)
			continue
		}
		if empty == nil {
			t.Errorf("History(%d) = nil, want empty slice", n)
		}
		if len(empty) != 0 {
			t.Errorf("History(%d) returned %d rows, want 0", n, len(empty))
		}
	}
}

func TestStats(t *testing.T) {
	s, _ := openStore(t, "stats.db")
	now := time.Now()

	seed := []checker.Result{
		{TargetName: "svc", StartedAt: now.Add(-1 * time.Hour), Duration: 100 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "svc", StartedAt: now.Add(-2 * time.Hour), Duration: 300 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "svc", StartedAt: now.Add(-3 * time.Hour), Duration: 200 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 500},
		{TargetName: "svc", StartedAt: now.Add(-4 * time.Hour), Duration: 0, Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"},
		// Outside the window: must be ignored.
		{TargetName: "svc", StartedAt: now.Add(-25 * time.Hour), Duration: 900 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		// Different target: must be ignored too.
		{TargetName: "other", StartedAt: now.Add(-1 * time.Hour), Duration: 500 * time.Millisecond, Outcome: checker.OutcomeError, Err: "boom"},
	}
	for _, r := range seed {
		if err := s.RecordResult(r); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
	}

	got, err := s.Stats("svc", 24*time.Hour)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := Stats{Up: 2, Down: 1, Skipped: 1, AvgDurationMs: 150}
	if got != want {
		t.Errorf("Stats = %+v, want %+v", got, want)
	}

	empty, err := s.Stats("missing", 24*time.Hour)
	if err != nil {
		t.Fatalf("Stats(missing): %v", err)
	}
	if empty != (Stats{}) {
		t.Errorf("Stats(missing) = %+v, want zero Stats", empty)
	}

	nonPositive, err := s.Stats("svc", 0)
	if err != nil {
		t.Fatalf("Stats(0): %v", err)
	}
	if nonPositive != (Stats{}) {
		t.Errorf("Stats(0) = %+v, want zero Stats", nonPositive)
	}
}

func TestConcurrentRecord(t *testing.T) {
	s, _ := openStore(t, "concurrent.db")

	const (
		goroutines = 8
		perWorker  = 50
	)
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				r := checker.Result{
					TargetName: "conc",
					StartedAt:  base.Add(time.Duration(g*perWorker+i) * time.Millisecond),
					Duration:   time.Duration(i) * time.Millisecond,
					Outcome:    checker.OutcomeUp,
					StatusCode: 200,
				}
				s.Record(r)
			}
		}(g)
	}
	wg.Wait()

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM results`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != goroutines*perWorker {
		t.Errorf("row count = %d, want %d", count, goroutines*perWorker)
	}
}

// TestRecordUsesProvidedStartedAt guards the decision that Record stores the
// caller's timestamp instead of time.Now; retention tests depend on it.
func TestRecordUsesProvidedStartedAt(t *testing.T) {
	s, _ := openStore(t, "started_at.db")
	want := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)

	s.Record(checker.Result{
		TargetName: "aged",
		StartedAt:  want,
		Duration:   time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	})

	var stored int64
	if err := s.db.QueryRow(`SELECT started_at FROM results WHERE target = 'aged'`).Scan(&stored); err != nil {
		t.Fatalf("read started_at: %v", err)
	}
	if stored != want.UnixMilli() {
		t.Errorf("started_at = %d, want caller-provided %d", stored, want.UnixMilli())
	}
}
