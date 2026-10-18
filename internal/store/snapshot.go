package store

import (
	"fmt"
	"math"
	"time"

	"pulsecheck/internal/checker"
)

// The snapshot's fixed shape mirrors the dashboard and the metrics scrape: a
// 24 h aggregate for the statistics and the first uptime window, a 7-day
// second uptime window, and the newest 30 outcomes per target.
const (
	snapshotRecentLimit = 30
	snapshotWindow24h   = 24 * time.Hour
	snapshotWindow7d    = 7 * 24 * time.Hour
)

// TargetSnapshot is everything the dashboard and the scrape know about one
// target, computed by the batched pass in StatusSnapshot (D40).
type TargetSnapshot struct {
	// Latest is the newest stored result; valid when HasLatest is true.
	Latest    checker.Result
	HasLatest bool
	// Recent holds the newest outcomes first, at most snapshotRecentLimit of
	// them. Always non-nil, so the JSON "recent" field stays an array.
	Recent []checker.Outcome
	// Stats is the 24 h aggregate, exactly as Stats computes it per target.
	Stats Stats
	// Uptime24h and Uptime7d are the rounded percentages, or nil when the
	// window holds no completed check (Uptime's hasData; skipped excluded).
	Uptime24h *float64
	Uptime7d  *float64
}

// StatusSnapshot computes the per-target snapshot for the given target names.
//
// Its shape was chosen by MEASUREMENT (D40): at 200 targets × 24 h of 30 s rows
// (~576k rows) the obvious "batch everything" rewrite — one full scan per
// concern in a handful of statements — measured the SLOWEST (~1.8 s per poll),
// because a grouped scan over the full history costs roughly an order of
// magnitude more per row than a plain index-range scan, and every concern still
// visited every retained row. The design that won does three things per poll:
//
//   - a bounded index seek per target for the newest result (a single batched
//     statement with correlated subqueries measured slower: it forced a
//     full-history scan);
//   - ONE merged index-range scan per target for every window aggregate
//     (grouped by outcome × bucket, with the 24 h counts and duration sums as
//     conditional aggregates over the same 7-day pass — the previous per-target
//     path visited the same rows four times for counts, buckets and two
//     uptimes separately);
//   - a LIMIT-30 index walk per target for the recent strip (History), which
//     never scans the full history the way a window function over the whole
//     (target IN ...) set did in the measured-bad variant.
//
// At the benchmark scale the snapshot polls in ~0.65 s against ~1.0-1.2 s for
// the original path (two samples each, BenchmarkStatusReadPaths); a probe of
// the aggregate portion alone measured ~17k allocations versus ~122k for the
// previous split reads. Every requested name gets an entry — an unknown
// or empty target yields the zero snapshot (an empty recent strip, a full zero
// histogram, nil uptimes) — so callers can assemble their payload without
// existence checks. The equivalence with the per-target primitives (Latest,
// History, Stats, Uptime) is pinned by TestStatusSnapshotMatchesPerTargetQueries.
func (s *Store) StatusSnapshot(targets []string) (map[string]TargetSnapshot, error) {
	out := make(map[string]TargetSnapshot, len(targets))
	if len(targets) == 0 {
		return out, nil
	}
	for _, name := range targets {
		out[name] = TargetSnapshot{Recent: []checker.Outcome{}, Stats: zeroStats()}
	}

	now := time.Now().UnixMilli()
	cutoff24h := now - snapshotWindow24h.Milliseconds()
	cutoff7d := now - snapshotWindow7d.Milliseconds()

	// 1) Newest result per target: a bounded index seek (ORDER BY
	// (started_at, id) DESC LIMIT 1 stops at the first row).
	for _, name := range targets {
		last, ok, err := s.Latest(name)
		if err != nil {
			return nil, fmt.Errorf("store: snapshot latest for %q: %w", name, err)
		}
		if !ok {
			continue
		}
		entry := out[name]
		entry.Latest = last
		entry.HasLatest = true
		out[name] = entry
	}

	// 2) Recent outcome strips: a bounded index walk per target. History stops
	// after 30 rows, so this never scans the full history.
	for _, name := range targets {
		history, err := s.History(name, snapshotRecentLimit)
		if err != nil {
			return nil, fmt.Errorf("store: snapshot recent for %q: %w", name, err)
		}
		if len(history) == 0 {
			continue
		}
		entry := out[name]
		for _, res := range history {
			entry.Recent = append(entry.Recent, res.Outcome)
		}
		out[name] = entry
	}

	// 3) The merged per-target aggregate: one index-range scan per target that
	// produces the 24 h outcome counts, the 7 d outcome counts, the 24 h
	// duration buckets and sums, and the all-rows 24 h sum/count the average
	// needs (the average deliberately includes the 0 ms skipped rows, matching
	// Stats' historical semantics). The cutoffs are inlined as integer literals
	// — internal instants, no injection risk — so the only bound arguments are
	// the target name and the 7 d cutoff.
	counts24 := make(map[string]map[checker.Outcome]int, len(targets))
	counts7d := make(map[string]map[checker.Outcome]int, len(targets))
	sum24 := make(map[string]int64, len(targets))
	total24 := make(map[string]int, len(targets))
	bucketCounts := make(map[string]map[int64]int, len(targets))
	bucketSums := make(map[string]map[int64]int64, len(targets))

	query := fmt.Sprintf(snapshotTargetAggregateQuery, durationCaseSQL(durationBucketBoundsMs), cutoff24h, cutoff24h)
	for _, name := range targets {
		if err := s.forEachRow(
			query,
			[]any{name, cutoff7d},
			func(rows rowScanner) error {
				var (
					outcome string
					upper   int64
					count7  int
					count24 int
					sum24v  int64
				)
				if err := rows.Scan(&outcome, &upper, &count7, &count24, &sum24v); err != nil {
					return err
				}
				if counts7d[name] == nil {
					counts7d[name] = make(map[checker.Outcome]int)
					counts24[name] = make(map[checker.Outcome]int)
					bucketCounts[name] = make(map[int64]int)
					bucketSums[name] = make(map[int64]int64)
				}
				counts7d[name][checker.Outcome(outcome)] += count7
				counts24[name][checker.Outcome(outcome)] += count24
				sum24[name] += sum24v
				total24[name] += count24
				// Buckets describe COMPLETED checks only: a skipped row never
				// measured a duration (D30d).
				if checker.Outcome(outcome) != checker.OutcomeSkipped {
					bucketCounts[name][upper] += count24
					bucketSums[name][upper] += sum24v
				}
				return nil
			},
		); err != nil {
			return nil, fmt.Errorf("store: snapshot aggregate for %q: %w", name, err)
		}
	}

	// Fold the gathered rows into per-target Stats and uptimes. The zero
	// snapshot seeded above supplies the full zero histogram, so every entry
	// keeps the same shape whether or not it has data.
	for name, entry := range out {
		c24 := counts24[name]
		entry.Stats.Up = c24[checker.OutcomeUp]
		entry.Stats.Down = c24[checker.OutcomeDown]
		entry.Stats.Timeout = c24[checker.OutcomeTimeout]
		entry.Stats.Error = c24[checker.OutcomeError]
		entry.Stats.Skipped = c24[checker.OutcomeSkipped]
		if n := total24[name]; n > 0 {
			entry.Stats.AvgDurationMs = int64(math.Round(float64(sum24[name]) / float64(n)))
		}

		cumulative := 0
		for i := range entry.Stats.DurationBuckets {
			upper := entry.Stats.DurationBuckets[i].UpperBoundMs
			cumulative += bucketCounts[name][upper]
			entry.Stats.DurationSumMs += bucketSums[name][upper]
			entry.Stats.DurationBuckets[i].Count = cumulative
		}
		if p50, ok := durationPercentile(entry.Stats.DurationBuckets, 0.50); ok {
			entry.Stats.P50DurationMs = &p50
		}
		if p95, ok := durationPercentile(entry.Stats.DurationBuckets, 0.95); ok {
			entry.Stats.P95DurationMs = &p95
		}

		if percent, ok := uptimePercent(c24); ok {
			entry.Uptime24h = &percent
		}
		if percent, ok := uptimePercent(counts7d[name]); ok {
			entry.Uptime7d = &percent
		}
		out[name] = entry
	}

	return out, nil
}

// snapshotTargetAggregateQuery is the merged per-target aggregate: grouped by
// outcome × bucket it returns the 7-day count, the 24-hour count, and the
// 24-hour duration sum. The %s verb is the bucket CASE expression and the two
// %d verbs are the 24 h cutoff; the 7 d cutoff is bound as the second argument,
// keeping the only placeholders in the WHERE clause where the argument order is
// simply (target, cutoff7d).
const snapshotTargetAggregateQuery = `SELECT outcome,
	   %s AS le_ms,
	   COUNT(*),
	   COALESCE(SUM(CASE WHEN started_at >= %d THEN 1 ELSE 0 END), 0),
	   COALESCE(SUM(CASE WHEN started_at >= %d THEN duration_ms ELSE 0 END), 0)
	 FROM results
	 WHERE target = ? AND started_at >= ?
	 GROUP BY outcome, le_ms`

// zeroStats returns the empty-window Stats: zero counts, a full zero histogram
// (a bucket count of 0 is a real measurement), and no percentile estimates.
func zeroStats() Stats {
	return Stats{DurationBuckets: zeroDurationBuckets()}
}

// zeroDurationBuckets builds the full bucket list (all bounds plus the
// open-ended bucket) with zero counts.
func zeroDurationBuckets() []DurationBucket {
	buckets := make([]DurationBucket, 0, len(durationBucketBoundsMs)+1)
	for _, upper := range durationBucketBoundsMs {
		buckets = append(buckets, DurationBucket{UpperBoundMs: upper})
	}
	return append(buckets, DurationBucket{UpperBoundMs: openBucketMs})
}

// rowScanner is the subset of *sql.Rows the scanning helpers need.
type rowScanner interface {
	Scan(dest ...any) error
}

// forEachRow runs query and invokes fn for every row, closing the result set
// and surfacing iteration errors.
func (s *Store) forEachRow(query string, args []any, fn func(rows rowScanner) error) error {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
