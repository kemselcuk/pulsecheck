package store

import (
	"testing"
	"time"

	"pulsecheck/internal/checker"
)

// seedOutcomes records n results with the given outcome, all one minute old, so
// they fall inside both the 24h and 7d windows.
func seedOutcomes(t *testing.T, s *Store, target string, outcome checker.Outcome, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		if err := s.RecordResult(checker.Result{
			TargetName: target,
			StartedAt:  time.Now().Add(-time.Minute),
			Duration:   time.Millisecond,
			Outcome:    outcome,
			StatusCode: 200,
		}); err != nil {
			t.Fatalf("seed %d x %s for %q: %v", n, outcome, target, err)
		}
	}
}

// TestUptime pins the arithmetic against hand-computed mixes, including the
// decision that skipped rows are excluded from the denominator (D30d): a round
// where no check ran is not an outage.
func TestUptime(t *testing.T) {
	s, _ := openStore(t, "uptime.db")

	// mixed: 7 up of 10 completed -> 70.0%. Five skipped rows must change
	// nothing; counting them would give 7/15 = 46.7%.
	seedOutcomes(t, s, "mixed", checker.OutcomeUp, 7)
	seedOutcomes(t, s, "mixed", checker.OutcomeDown, 2)
	seedOutcomes(t, s, "mixed", checker.OutcomeTimeout, 1)
	seedOutcomes(t, s, "mixed", checker.OutcomeSkipped, 5)

	// rounds-up: 2/3 = 66.666...% -> 66.7%.
	seedOutcomes(t, s, "rounds-up", checker.OutcomeUp, 2)
	seedOutcomes(t, s, "rounds-up", checker.OutcomeDown, 1)

	// rounds-down: 1/3 = 33.333...% -> 33.3%.
	seedOutcomes(t, s, "rounds-down", checker.OutcomeUp, 1)
	seedOutcomes(t, s, "rounds-down", checker.OutcomeDown, 2)

	// errors count as failures: 3/4 = 75.0%.
	seedOutcomes(t, s, "with-errors", checker.OutcomeUp, 3)
	seedOutcomes(t, s, "with-errors", checker.OutcomeError, 1)

	// skipped-only: no completed check ever ran -> no data, not 0%.
	seedOutcomes(t, s, "skipped-only", checker.OutcomeSkipped, 3)

	// all-down: a measured outage is a real 0.0% with data.
	seedOutcomes(t, s, "all-down", checker.OutcomeDown, 2)

	// aging: a row outside the 24h window must be ignored, so 1/1 = 100.0%.
	if err := s.RecordResult(checker.Result{
		TargetName: "aging", StartedAt: time.Now().Add(-time.Hour), Duration: time.Millisecond,
		Outcome: checker.OutcomeUp, StatusCode: 200,
	}); err != nil {
		t.Fatalf("seed aging row: %v", err)
	}
	if err := s.RecordResult(checker.Result{
		TargetName: "aging", StartedAt: time.Now().Add(-25 * time.Hour), Duration: time.Millisecond,
		Outcome: checker.OutcomeDown, StatusCode: 500,
	}); err != nil {
		t.Fatalf("seed aging old row: %v", err)
	}

	cases := []struct {
		target   string
		want     float64
		wantData bool
	}{
		{"mixed", 70.0, true},
		{"rounds-up", 66.7, true},
		{"rounds-down", 33.3, true},
		{"with-errors", 75.0, true},
		{"skipped-only", 0, false},
		{"all-down", 0.0, true},
		{"aging", 100.0, true},
		{"never-seen", 0, false},
	}

	for _, tc := range cases {
		got, hasData, err := s.Uptime(tc.target, 24*time.Hour)
		if err != nil {
			t.Errorf("Uptime(%q, 24h): %v", tc.target, err)
			continue
		}
		if hasData != tc.wantData {
			t.Errorf("Uptime(%q) hasData = %v, want %v", tc.target, hasData, tc.wantData)
		}
		if got != tc.want {
			t.Errorf("Uptime(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}

	// A non-positive window is an empty interval: no data, no error.
	for _, window := range []time.Duration{0, -time.Hour} {
		got, hasData, err := s.Uptime("mixed", window)
		if err != nil {
			t.Errorf("Uptime(mixed, %s): %v", window, err)
			continue
		}
		if hasData || got != 0 {
			t.Errorf("Uptime(mixed, %s) = (%v, %v), want (0, false)", window, got, hasData)
		}
	}
}

// TestOutcomeCounts pins the per-outcome counts and the window boundary.
func TestOutcomeCounts(t *testing.T) {
	s, _ := openStore(t, "counts.db")

	seedOutcomes(t, s, "svc", checker.OutcomeUp, 3)
	seedOutcomes(t, s, "svc", checker.OutcomeDown, 1)
	seedOutcomes(t, s, "svc", checker.OutcomeSkipped, 2)
	// Different target and an out-of-window row: both must be invisible.
	seedOutcomes(t, s, "other", checker.OutcomeUp, 4)
	if err := s.RecordResult(checker.Result{
		TargetName: "svc", StartedAt: time.Now().Add(-25 * time.Hour), Duration: time.Millisecond,
		Outcome: checker.OutcomeTimeout,
	}); err != nil {
		t.Fatalf("seed out-of-window row: %v", err)
	}

	got, err := s.OutcomeCounts("svc", 24*time.Hour)
	if err != nil {
		t.Fatalf("OutcomeCounts: %v", err)
	}
	want := map[checker.Outcome]int{
		checker.OutcomeUp:      3,
		checker.OutcomeDown:    1,
		checker.OutcomeSkipped: 2,
	}
	if len(got) != len(want) {
		t.Fatalf("OutcomeCounts = %v, want %v", got, want)
	}
	for outcome, n := range want {
		if got[outcome] != n {
			t.Errorf("OutcomeCounts[%q] = %d, want %d", outcome, got[outcome], n)
		}
	}

	empty, err := s.OutcomeCounts("missing", 24*time.Hour)
	if err != nil {
		t.Fatalf("OutcomeCounts(missing): %v", err)
	}
	if empty == nil {
		t.Fatal("OutcomeCounts(missing) = nil, want an empty map")
	}
	if len(empty) != 0 {
		t.Errorf("OutcomeCounts(missing) = %v, want empty", empty)
	}

	nonPositive, err := s.OutcomeCounts("svc", 0)
	if err != nil {
		t.Fatalf("OutcomeCounts(svc, 0): %v", err)
	}
	if nonPositive == nil || len(nonPositive) != 0 {
		t.Errorf("OutcomeCounts(svc, 0) = %v, want empty", nonPositive)
	}
}

// TestLastWriteTime covers the empty database (false, no fabricated time) and
// the newest timestamp across targets.
func TestLastWriteTime(t *testing.T) {
	s, _ := openStore(t, "lastwrite.db")

	got, ok, err := s.LastWriteTime()
	if err != nil {
		t.Fatalf("LastWriteTime on empty: %v", err)
	}
	if ok {
		t.Errorf("LastWriteTime on empty = %v (ok), want ok=false", got)
	}
	if !got.IsZero() {
		t.Errorf("LastWriteTime on empty = %v, want the zero time", got)
	}

	newest := time.Now().Add(-2 * time.Minute).Truncate(time.Millisecond)
	older := newest.Add(-time.Hour)
	oldest := newest.Add(-2 * time.Hour)
	for _, r := range []checker.Result{
		{TargetName: "a", StartedAt: older, Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "b", StartedAt: newest, Duration: time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 500},
		{TargetName: "a", StartedAt: oldest, Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
	} {
		if err := s.RecordResult(r); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
	}

	got, ok, err = s.LastWriteTime()
	if err != nil {
		t.Fatalf("LastWriteTime: %v", err)
	}
	if !ok {
		t.Fatal("LastWriteTime = ok=false, want true")
	}
	if !got.Equal(newest) {
		t.Errorf("LastWriteTime = %v, want %v", got, newest)
	}
	if got.Location() != time.UTC {
		t.Errorf("LastWriteTime location = %v, want UTC", got.Location())
	}
}
