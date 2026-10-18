package store

import (
	"testing"
	"time"

	"pulsecheck/internal/checker"
)

// TestStatusSnapshotMatchesPerTargetQueries is the equivalence pin for the
// batched snapshot (D40): for every target it compares the snapshot entry with
// the per-target primitives the dashboard used before (Latest, History, Stats,
// Uptime), field by field — counts, average, histogram buckets, summed
// duration, percentile estimates, both uptime windows, and the recent strip.
func TestStatusSnapshotMatchesPerTargetQueries(t *testing.T) {
	s, _ := openStore(t, "snapshot.db")
	now := time.Now()

	seed := []checker.Result{
		// alpha: a mixed 24 h history with a certificate on the newest row, and
		// one 8-day-old row that only the 7 d window can see.
		{TargetName: "alpha", StartedAt: now.Add(-1 * time.Minute), Duration: 12 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200, CertExpiresAt: now.Add(48 * time.Hour).UTC().Truncate(time.Millisecond)},
		{TargetName: "alpha", StartedAt: now.Add(-2 * time.Minute), Duration: 30 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 503},
		{TargetName: "alpha", StartedAt: now.Add(-3 * time.Minute), Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"},
		{TargetName: "alpha", StartedAt: now.Add(-8 * 24 * time.Hour), Duration: 5 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		// bravo: only skipped rows -> zero histogram, null uptimes.
		{TargetName: "bravo", StartedAt: now.Add(-1 * time.Minute), Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"},
		// charlie: intentionally no rows at all.
	}
	for _, r := range seed {
		if err := s.RecordResult(r); err != nil {
			t.Fatalf("RecordResult: %v", err)
		}
	}

	names := []string{"alpha", "bravo", "charlie"}
	snap, err := s.StatusSnapshot(names)
	if err != nil {
		t.Fatalf("StatusSnapshot: %v", err)
	}
	if len(snap) != len(names) {
		t.Fatalf("snapshot has %d entries, want %d", len(snap), len(names))
	}

	for _, name := range names {
		got := snap[name]

		last, ok, err := s.Latest(name)
		if err != nil {
			t.Fatalf("Latest(%s): %v", name, err)
		}
		if got.HasLatest != ok {
			t.Errorf("%s: HasLatest = %v, want %v", name, got.HasLatest, ok)
		}
		if ok && got.Latest != last {
			t.Errorf("%s: Latest = %+v, want %+v", name, got.Latest, last)
		}

		history, err := s.History(name, snapshotRecentLimit)
		if err != nil {
			t.Fatalf("History(%s): %v", name, err)
		}
		if len(got.Recent) != len(history) {
			t.Errorf("%s: Recent has %d outcomes, want %d", name, len(got.Recent), len(history))
		} else {
			for i, h := range history {
				if got.Recent[i] != h.Outcome {
					t.Errorf("%s: Recent[%d] = %q, want %q", name, i, got.Recent[i], h.Outcome)
				}
			}
		}

		wantStats, err := s.Stats(name, snapshotWindow24h)
		if err != nil {
			t.Fatalf("Stats(%s): %v", name, err)
		}
		assertSnapshotStatsEqual(t, name, wantStats, got.Stats)

		for _, window := range []struct {
			label string
			d     time.Duration
			got   *float64
		}{
			{"uptime_24h", snapshotWindow24h, got.Uptime24h},
			{"uptime_7d", snapshotWindow7d, got.Uptime7d},
		} {
			percent, hasData, err := s.Uptime(name, window.d)
			if err != nil {
				t.Fatalf("Uptime(%s, %s): %v", name, window.d, err)
			}
			if hasData != (window.got != nil) {
				t.Errorf("%s %s: hasData = %v, snapshot %v", name, window.label, hasData, window.got)
				continue
			}
			if hasData && *window.got != percent {
				t.Errorf("%s %s = %v, want %v", name, window.label, *window.got, percent)
			}
		}
	}
}

// assertSnapshotStatsEqual compares two Stats field by field; Stats carries a
// slice and pointers, so == cannot be used.
func assertSnapshotStatsEqual(t *testing.T, name string, want, got Stats) {
	t.Helper()

	if got.Up != want.Up || got.Down != want.Down || got.Timeout != want.Timeout ||
		got.Error != want.Error || got.Skipped != want.Skipped ||
		got.AvgDurationMs != want.AvgDurationMs || got.DurationSumMs != want.DurationSumMs {
		t.Errorf("%s: Stats = %+v, want %+v", name, got, want)
	}
	if len(got.DurationBuckets) != len(want.DurationBuckets) {
		t.Fatalf("%s: %d buckets, want %d", name, len(got.DurationBuckets), len(want.DurationBuckets))
	}
	for i := range want.DurationBuckets {
		if got.DurationBuckets[i] != want.DurationBuckets[i] {
			t.Errorf("%s: bucket[%d] = %+v, want %+v", name, i, got.DurationBuckets[i], want.DurationBuckets[i])
		}
	}
	if durPtr(got.P50DurationMs) != durPtr(want.P50DurationMs) {
		t.Errorf("%s: p50 = %v, want %v", name, durPtr(got.P50DurationMs), durPtr(want.P50DurationMs))
	}
	if durPtr(got.P95DurationMs) != durPtr(want.P95DurationMs) {
		t.Errorf("%s: p95 = %v, want %v", name, durPtr(got.P95DurationMs), durPtr(want.P95DurationMs))
	}
}

// durValue renders a nullable duration estimate as a comparable pair, so two
// estimates (and their nil-ness) can be compared with a single ==.
type durValue struct {
	value int64
	set   bool
}

// durPtr wraps a nullable duration estimate for comparison.
func durPtr(p *int64) durValue {
	if p == nil {
		return durValue{}
	}
	return durValue{value: *p, set: true}
}

// TestStatusSnapshotEmptyInput covers the degenerate inputs: no targets, and a
// name that has no rows (which must still arrive as the full zero snapshot).
func TestStatusSnapshotEmptyInput(t *testing.T) {
	s, _ := openStore(t, "snapshot-empty.db")

	empty, err := s.StatusSnapshot(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("StatusSnapshot(nil) = (%v, %v), want an empty map", empty, err)
	}

	snap, err := s.StatusSnapshot([]string{"ghost"})
	if err != nil {
		t.Fatalf("StatusSnapshot(ghost): %v", err)
	}
	got, ok := snap["ghost"]
	if !ok {
		t.Fatal("snapshot has no entry for the requested name")
	}
	if got.HasLatest || len(got.Recent) != 0 || got.Recent == nil {
		t.Errorf("ghost entry = %+v, want no latest and a non-nil empty recent", got)
	}
	if got.Stats.P50DurationMs != nil || got.Stats.P95DurationMs != nil || got.Stats.DurationSumMs != 0 {
		t.Errorf("ghost stats = %+v, want no estimates and no sum", got.Stats)
	}
	if len(got.Stats.DurationBuckets) != len(zeroDurationBuckets()) ||
		got.Stats.DurationBuckets[len(got.Stats.DurationBuckets)-1].Count != 0 {
		t.Errorf("ghost histogram = %+v, want the full zero histogram", got.Stats.DurationBuckets)
	}
	if got.Uptime24h != nil || got.Uptime7d != nil {
		t.Errorf("ghost uptimes = (%v, %v), want (nil, nil)", got.Uptime24h, got.Uptime7d)
	}
}
