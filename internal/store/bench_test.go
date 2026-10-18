package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"pulsecheck/internal/checker"
)

// benchStore opens a fresh SQLite database in the benchmark's temporary
// directory, so each benchmark starts from an empty schema and no benchmark
// can observe rows written by another.
func benchStore(b *testing.B) *Store {
	b.Helper()

	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// benchResult builds a deterministic up result for row i. StartedAt advances
// with i so ordering by (started_at DESC, id DESC) is stable and History's
// newest-first window is well defined.
func benchResult(i int) checker.Result {
	return checker.Result{
		TargetName: "bench",
		URL:        "https://example.com",
		StartedAt:  time.UnixMilli(int64(i) * int64(time.Second/time.Millisecond)).UTC(),
		Duration:   time.Duration(i%100) * time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}
}

// BenchmarkRecord measures one insert through RecordResult, the same call the
// scheduler's recorder path uses (Record delegates to it).
func BenchmarkRecord(b *testing.B) {
	s := benchStore(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.RecordResult(benchResult(i)); err != nil {
			b.Fatalf("RecordResult: %v", err)
		}
	}
}

// BenchmarkHistory measures a 50-row history query against a table seeded with
// 1000 rows once, before the timer starts. That is the shape the web UI asks
// for repeatedly, so it is the read path most worth watching.
func BenchmarkHistory(b *testing.B) {
	s := benchStore(b)
	for i := 0; i < 1000; i++ {
		if err := s.RecordResult(benchResult(i)); err != nil {
			b.Fatalf("seed RecordResult: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results, err := s.History("bench", 50)
		if err != nil {
			b.Fatalf("History: %v", err)
		}
		if len(results) != 50 {
			b.Fatalf("History returned %d rows, want 50", len(results))
		}
	}
}

// BenchmarkLatest measures the single-row lookup that backs the current status
// view.
func BenchmarkLatest(b *testing.B) {
	s := benchStore(b)
	if err := s.RecordResult(benchResult(0)); err != nil {
		b.Fatalf("seed RecordResult: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := s.Latest("bench"); err != nil || !ok {
			b.Fatalf("Latest: ok=%v err=%v", ok, err)
		}
	}
}

// BenchmarkStatusReadPaths compares the two ways a full dashboard poll can
// read the store: the per-target path /api/status used before (Latest, a
// 30-row History, Stats, and both uptime windows per target — five to six
// queries each) against the batched StatusSnapshot (five grouped queries in
// total, D40). The store is seeded once with 200 targets × 2880 rows — 24 h at
// the 30 s default interval, ~576k rows — before either timer starts.
func BenchmarkStatusReadPaths(b *testing.B) {
	s := benchStore(b)
	const (
		targets = 200
		rows    = 2880
	)
	names := seedStatusScale(b, s, targets, rows)

	b.Run("per-target", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := readPerTarget(s, names); err != nil {
				b.Fatalf("per-target read: %v", err)
			}
		}
	})

	b.Run("snapshot", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := s.StatusSnapshot(names); err != nil {
				b.Fatalf("StatusSnapshot: %v", err)
			}
		}
	})
}

// readPerTarget replicates the per-target read path /api/status used before
// the snapshot, so the benchmark compares like with like.
func readPerTarget(s *Store, names []string) error {
	for _, name := range names {
		if _, _, err := s.Latest(name); err != nil {
			return err
		}
		if _, err := s.History(name, snapshotRecentLimit); err != nil {
			return err
		}
		if _, err := s.Stats(name, snapshotWindow24h); err != nil {
			return err
		}
		if _, _, err := s.Uptime(name, snapshotWindow24h); err != nil {
			return err
		}
		if _, _, err := s.Uptime(name, snapshotWindow7d); err != nil {
			return err
		}
	}
	return nil
}

// seedStatusScale inserts targets × rows deterministic results spanning the
// last 24 hours in one transaction — the direct-insert fast path is fine here
// because the benchmark measures reads — and returns the target names.
func seedStatusScale(b *testing.B, s *Store, targets, rows int) []string {
	b.Helper()

	tx, err := s.db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO results
		(target, started_at, outcome, status_code, duration_ms, error, cert_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}

	base := time.Now().Add(-snapshotWindow24h)
	names := make([]string, 0, targets)
	for ti := 0; ti < targets; ti++ {
		name := fmt.Sprintf("scale-%03d", ti)
		names = append(names, name)
		for ri := 0; ri < rows; ri++ {
			startedAt := base.Add(time.Duration(ri) * 30 * time.Second).UnixMilli()
			outcome, status := "up", 200
			switch {
			case ri%200 == 199:
				outcome, status = "down", 503
			case ri%500 == 499:
				outcome, status = "skipped", 0
			}
			duration := int64(5 + (ri*7)%120)
			if outcome == "skipped" {
				duration = 0
			}
			if _, err := stmt.Exec(name, startedAt, outcome, status, duration, ""); err != nil {
				b.Fatalf("insert: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return names
}
