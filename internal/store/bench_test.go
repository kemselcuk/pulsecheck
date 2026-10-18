package store

import (
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
