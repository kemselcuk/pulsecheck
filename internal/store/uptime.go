package store

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	"pulsecheck/internal/checker"
)

// outcomeCountsQuery counts the stored checks of one target per outcome inside
// the window. Grouping in SQL keeps the whole aggregation in one round trip;
// outcomes with no rows simply produce no group.
const outcomeCountsQuery = `SELECT outcome, COUNT(*)
	 FROM results
	 WHERE target = ? AND started_at >= ?
	 GROUP BY outcome`

// uptimePercent turns outcome counts into the uptime percentage shared by
// Uptime and the batched StatusSnapshot: the share of completed checks that
// were up, as a percentage rounded to one decimal place. The skipped outcome is
// deliberately EXCLUDED (decision D30d): a skipped row means no check ran — the
// previous check was still in flight or the worker pool was saturated — so
// counting it would score an unmeasured interval as an outage and make a busy
// process look less available than it is. hasData is false when the window
// holds no completed check, so callers distinguish "0% up" from "no
// measurements".
func uptimePercent(counts map[checker.Outcome]int) (percent float64, hasData bool) {
	up := counts[checker.OutcomeUp]
	completed := up +
		counts[checker.OutcomeDown] +
		counts[checker.OutcomeTimeout] +
		counts[checker.OutcomeError]
	if completed == 0 {
		return 0, false
	}

	return math.Round(float64(up)/float64(completed)*1000) / 10, true
}

// Uptime reports the share of completed checks in the window ending now that
// resulted in an up outcome, as a percentage rounded to one decimal place.
//
// The denominator is up + down + timeout + error. The skipped outcome is
// deliberately EXCLUDED (decision D30d): a skipped row means no check ran — the
// previous check was still in flight or the worker pool was saturated — so
// counting it would score an unmeasured interval as an outage and make a busy
// process look less available than it is.
//
// hasData is false, with percent 0, when the window is not positive or when the
// window holds no completed checks (an empty target, or one that only has
// skipped rows). Callers therefore distinguish "0% up" from "no measurements".
func (s *Store) Uptime(target string, window time.Duration) (percent float64, hasData bool, err error) {
	if window <= 0 {
		return 0, false, nil
	}

	counts, err := s.OutcomeCounts(target, window)
	if err != nil {
		return 0, false, err
	}

	percent, hasData = uptimePercent(counts)
	return percent, hasData, nil
}

// OutcomeCounts counts the stored checks of target per outcome over the window
// ending now. The returned map omits outcomes with no rows; a non-positive
// window describes an empty interval and yields an empty (non-nil) map.
func (s *Store) OutcomeCounts(target string, window time.Duration) (map[checker.Outcome]int, error) {
	counts := make(map[checker.Outcome]int)
	if window <= 0 {
		return counts, nil
	}

	cutoff := time.Now().Add(-window).UnixMilli()
	rows, err := s.db.Query(outcomeCountsQuery, target, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: outcome counts for %q: %w", target, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			outcome string
			count   int
		)
		if err := rows.Scan(&outcome, &count); err != nil {
			return nil, fmt.Errorf("store: scan outcome counts for %q: %w", target, err)
		}
		counts[checker.Outcome(outcome)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: outcome counts for %q: %w", target, err)
	}
	return counts, nil
}

// LastWriteTime returns the newest started_at across all targets, which is when
// the most recent check was recorded. The boolean is false when the database
// holds no results at all, so a caller can report "no data yet" instead of a
// fabricated timestamp.
//
// The instant is read from started_at rather than from the wall clock at write
// time: every row already carries the checker's own timestamp, so this needs no
// extra bookkeeping and stays correct across restarts.
func (s *Store) LastWriteTime() (time.Time, bool, error) {
	var latest sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(started_at) FROM results`).Scan(&latest); err != nil {
		return time.Time{}, false, fmt.Errorf("store: last write time: %w", err)
	}
	if !latest.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(latest.Int64).UTC(), true, nil
}
