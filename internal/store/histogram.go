package store

import (
	"fmt"
	"math"
	"strings"
)

// durationBucketBoundsMs are the finite upper bounds (inclusive, milliseconds)
// of the windowed duration histogram, mirroring the classic Prometheus HTTP
// bucket layout (5ms..10s). The open-ended bucket above the last bound is
// appended at query time.
var durationBucketBoundsMs = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// openBucketMs marks the trailing open-ended bucket: durations above the last
// finite bound.
const openBucketMs = math.MaxInt64

// durationBucketsQuery is built from the bounds so the CASE arms and the bucket
// list can never drift apart. Skipped rows are excluded: no check ran, so they
// carry no duration (D30d consistency).
var durationBucketsQuery = buildDurationBucketsQuery(durationBucketBoundsMs)

// buildDurationBucketsQuery renders the grouped histogram query: for every
// non-empty bucket it returns the bucket's upper bound, the row count, and the
// summed duration, in exactly the shape Stats consumes.
func buildDurationBucketsQuery(bounds []int64) string {
	var b strings.Builder
	b.WriteString("SELECT CASE")
	for _, ub := range bounds {
		fmt.Fprintf(&b, " WHEN duration_ms <= %d THEN %d", ub, ub)
	}
	fmt.Fprintf(&b, " ELSE %d END AS upper_ms, COUNT(*), COALESCE(SUM(duration_ms), 0)", openBucketMs)
	b.WriteString(" FROM results WHERE target = ? AND started_at >= ? AND outcome != 'skipped' GROUP BY upper_ms")
	return b.String()
}

// durationPercentile estimates the q-th percentile (q in [0,1]) of the
// completed checks from their cumulative buckets: the upper bound of the first
// bucket whose cumulative count reaches ceil(q * total). ok is false when there
// are no completed checks, or when the rank falls in the open-ended bucket — a
// bucket ceiling cannot express a duration above the last finite bound.
func durationPercentile(buckets []DurationBucket, q float64) (int64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	total := buckets[len(buckets)-1].Count
	if total == 0 {
		return 0, false
	}
	rank := int(math.Ceil(q * float64(total)))
	for _, bucket := range buckets {
		if bucket.Count >= rank {
			if bucket.UpperBoundMs == openBucketMs {
				return 0, false
			}
			return bucket.UpperBoundMs, true
		}
	}
	return 0, false
}
