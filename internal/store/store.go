package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pulsecheck/internal/checker"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, so CGO_ENABLED=0 builds work.
)

// Compile-time proof that the store is a valid result sink for the scheduler.
var _ checker.Recorder = (*Store)(nil)

// busyTimeoutMs is how long SQLite waits for a competing writer before
// returning SQLITE_BUSY. Writes are additionally serialized in-process by
// Store.mu, so this only covers external writers such as a sqlite3 shell.
const busyTimeoutMs = 5000

// maxOpenConns bounds the connection pool. WAL allows readers to proceed
// while one writer is active, so a handful of connections is enough.
const maxOpenConns = 4

// schemaStatements are idempotent, so reopening an existing database is safe.
//
// Migration story (v1.1): the results table is created with every current
// column, so a NEW database is born complete. A database created by an older
// release already has the table, so CREATE TABLE IF NOT EXISTS is a no-op and
// the missing column is added by an explicit, idempotent ALTER TABLE in
// migrate(). Schema evolution is therefore additive and detected from the live
// schema (PRAGMA table_info) rather than tracked with a version table, which
// keeps reopening an old database on every start safe and loss-free.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS results (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		target          TEXT    NOT NULL,
		started_at      INTEGER NOT NULL,
		outcome         TEXT    NOT NULL CHECK (outcome IN ('up', 'down', 'timeout', 'error', 'skipped')),
		status_code     INTEGER,
		duration_ms     INTEGER,
		error           TEXT,
		cert_expires_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_results_target_time ON results (target, started_at)`,
}

// Stats aggregates the results of one target over a time window.
//
// The histogram fields (DurationBuckets, DurationSumMs) and the percentile
// estimates describe only the COMPLETED checks (up + down + timeout + error):
// a skipped row means no check ran, so it carries no duration signal
// (D30d consistency). AvgDurationMs is the exception and keeps its historical
// semantics: it averages the duration_ms column over ALL rows in the window —
// including the 0 ms skipped rows — so it stays comparable with the values
// recorded since v1.0.
type Stats struct {
	Up            int
	Down          int
	Timeout       int
	Error         int
	Skipped       int
	AvgDurationMs int64
	// P50DurationMs and P95DurationMs estimate the completed-check duration
	// percentiles from DurationBuckets: the value is the upper bound of the
	// bucket containing the rank, so it is a CEILING, not an interpolated
	// percentile. nil when the window holds no completed checks, or when the
	// percentile falls in the open-ended bucket (above the last finite bound) —
	// a bucket ceiling cannot express that case.
	P50DurationMs *int64
	P95DurationMs *int64
	// DurationBuckets is the CUMULATIVE duration histogram of the completed
	// checks over the window: each Count is the number of completed checks with
	// duration less than or equal to UpperBoundMs. The list ends with the
	// open-ended bucket (UpperBoundMs == math.MaxInt64), whose Count equals the
	// total number of completed checks. Empty only when the window is not
	// positive.
	DurationBuckets []DurationBucket
	// DurationSumMs is the summed duration of the completed checks, the
	// histogram's _sum analogue.
	DurationSumMs int64
}

// DurationBucket is one bucket of the windowed duration histogram: UpperBoundMs
// is the inclusive upper bound in milliseconds (math.MaxInt64 marks the
// open-ended trailing bucket) and Count is the cumulative number of completed
// checks at or below that bound.
type DurationBucket struct {
	UpperBoundMs int64
	Count        int
}

// Store persists check results in a local SQLite database.
//
// The database handle is shared by all callers; writes are serialized by mutex
// because SQLite allows only one writer at a time. Reads rely on WAL mode so
// they can proceed concurrently with a writer.
//
// Store is safe for concurrent use.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serializes all writes
}

// Open opens (creating it if needed) the SQLite database at path, enables WAL
// mode and a busy timeout, and ensures the schema exists. Missing parent
// directories are created, so a path such as data/demo.db works on a fresh
// checkout.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
	}

	// busy_timeout is per connection, so it belongs in the DSN where the driver
	// applies it to every new connection. WAL is a persistent database property
	// and is enabled once below.
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", path, busyTimeoutMs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)

	// Force a connection now so an unusable path or bad DSN fails at Open
	// instead of on the first query.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL for %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// createSchema applies the idempotent schema statements.
func (s *Store) createSchema() error {
	for _, stmt := range schemaStatements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: create schema: %w", err)
		}
	}
	return nil
}

// migrate brings a database created by an older release up to the current
// schema. It is intentionally additive and idempotent: each step is skipped
// when the live schema already satisfies it, so Open can run it on every start
// — including on a brand-new database, where createSchema already added the
// column — without side effects.
func (s *Store) migrate() error {
	hasCert, err := s.hasColumn("results", "cert_expires_at")
	if err != nil {
		return err
	}
	if !hasCert {
		// Older databases lack the column; add it. Existing rows get NULL,
		// which scanResult reads back as the zero time (no certificate info).
		if _, err := s.db.Exec(`ALTER TABLE results ADD COLUMN cert_expires_at INTEGER`); err != nil {
			return fmt.Errorf("store: migrate results: add cert_expires_at: %w", err)
		}
	}
	return nil
}

// hasColumn reports whether table has a column named column, as read from the
// live schema. table and column are internal constants, never user input, so
// interpolating the table name into the pragma carries no injection risk.
func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("store: inspect %s schema: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return false, fmt.Errorf("store: scan %s schema: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: inspect %s schema: %w", table, err)
	}
	return false, nil
}

// Record persists r, implementing checker.Recorder.
//
// The checker.Recorder interface has no error return, so persistence failures
// are logged instead of propagated. Callers that need the error (tests, future
// wiring) can use RecordResult.
func (s *Store) Record(r checker.Result) {
	if err := s.RecordResult(r); err != nil {
		slog.Error("store: record result", "target", r.TargetName, "err", err)
	}
}

// RecordResult persists r and reports any persistence error.
//
// r.StartedAt is stored exactly as given (as Unix milliseconds), never replaced
// with the current time, so callers and tests control the recorded timestamp.
// r.URL is not part of the current schema and is not persisted. A zero
// r.CertExpiresAt is stored as SQL NULL; otherwise the instant is stored as Unix
// milliseconds.
func (s *Store) RecordResult(r checker.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var certExpiresAt any // NULL when there is no certificate info
	if !r.CertExpiresAt.IsZero() {
		certExpiresAt = r.CertExpiresAt.UnixMilli()
	}

	_, err := s.db.Exec(
		`INSERT INTO results (target, started_at, outcome, status_code, duration_ms, error, cert_expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.TargetName,
		r.StartedAt.UnixMilli(),
		string(r.Outcome),
		r.StatusCode,
		r.Duration.Milliseconds(),
		r.Err,
		certExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("store: record result for %q: %w", r.TargetName, err)
	}
	return nil
}

// Latest returns the most recent result for target. The boolean is false when
// no result exists yet.
func (s *Store) Latest(target string) (checker.Result, bool, error) {
	row := s.db.QueryRow(
		`SELECT target, started_at, duration_ms, outcome, status_code, error, cert_expires_at
		 FROM results
		 WHERE target = ?
		 ORDER BY started_at DESC, id DESC
		 LIMIT 1`,
		target,
	)

	r, err := scanResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return checker.Result{}, false, nil
	}
	if err != nil {
		return checker.Result{}, false, fmt.Errorf("store: latest result for %q: %w", target, err)
	}
	return r, true, nil
}

// History returns up to limit of the most recent results for target, newest
// first. A non-positive limit yields an empty slice and no error.
func (s *Store) History(target string, limit int) ([]checker.Result, error) {
	if limit <= 0 {
		return []checker.Result{}, nil
	}

	rows, err := s.db.Query(
		`SELECT target, started_at, duration_ms, outcome, status_code, error, cert_expires_at
		 FROM results
		 WHERE target = ?
		 ORDER BY started_at DESC, id DESC
		 LIMIT ?`,
		target, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: history for %q: %w", target, err)
	}
	defer func() { _ = rows.Close() }()

	results := make([]checker.Result, 0, limit)
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan history for %q: %w", target, err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: history for %q: %w", target, err)
	}
	return results, nil
}

// Stats aggregates the counts, the average duration, the windowed duration
// histogram, and the duration percentile estimates of target over the window
// ending now. Durations are stored and averaged in whole milliseconds, and the
// average is rounded to the nearest millisecond.
//
// A non-positive window describes an empty interval and returns zero Stats.
func (s *Store) Stats(target string, window time.Duration) (Stats, error) {
	if window <= 0 {
		return Stats{}, nil
	}
	cutoff := time.Now().Add(-window).UnixMilli()

	var (
		stats Stats
		avgMs sql.NullFloat64
	)
	err := s.db.QueryRow(
		`SELECT
			COALESCE(SUM(CASE WHEN outcome = 'up'      THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome = 'down'    THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome = 'timeout' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome = 'error'   THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome = 'skipped' THEN 1 ELSE 0 END), 0),
			AVG(duration_ms)
		 FROM results
		 WHERE target = ? AND started_at >= ?`,
		target, cutoff,
	).Scan(
		&stats.Up, &stats.Down, &stats.Timeout,
		&stats.Error, &stats.Skipped, &avgMs,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("store: stats for %q: %w", target, err)
	}
	if avgMs.Valid {
		stats.AvgDurationMs = int64(math.Round(avgMs.Float64))
	}

	if err := s.durationHistogram(target, cutoff, &stats); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// durationHistogram fills the duration fields of stats from the grouped bucket
// query: per-bucket counts and sums are folded into cumulative buckets and the
// p50/p95 estimates are derived from them. The open-ended bucket is always
// appended, so the final cumulative count equals the number of completed checks
// even when no bucket row was returned.
func (s *Store) durationHistogram(target string, cutoff int64, stats *Stats) error {
	rows, err := s.db.Query(durationBucketsQuery, target, cutoff)
	if err != nil {
		return fmt.Errorf("store: duration histogram for %q: %w", target, err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[int64]int, len(durationBucketBoundsMs)+1)
	sums := make(map[int64]int64, len(durationBucketBoundsMs)+1)
	for rows.Next() {
		var (
			upper int64
			count int
			sum   int64
		)
		if err := rows.Scan(&upper, &count, &sum); err != nil {
			return fmt.Errorf("store: scan duration histogram for %q: %w", target, err)
		}
		counts[upper] = count
		sums[upper] = sum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: duration histogram for %q: %w", target, err)
	}

	stats.DurationBuckets = make([]DurationBucket, 0, len(durationBucketBoundsMs)+1)
	cumulative := 0
	for _, ub := range durationBucketBoundsMs {
		cumulative += counts[ub]
		stats.DurationSumMs += sums[ub]
		stats.DurationBuckets = append(stats.DurationBuckets, DurationBucket{UpperBoundMs: ub, Count: cumulative})
	}
	cumulative += counts[openBucketMs]
	stats.DurationSumMs += sums[openBucketMs]
	stats.DurationBuckets = append(stats.DurationBuckets, DurationBucket{UpperBoundMs: openBucketMs, Count: cumulative})

	if p50, ok := durationPercentile(stats.DurationBuckets, 0.50); ok {
		stats.P50DurationMs = &p50
	}
	if p95, ok := durationPercentile(stats.DurationBuckets, 0.95); ok {
		stats.P95DurationMs = &p95
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// scanResult reads the common result column list into a checker.Result. The
// column order must match the SELECT lists in Latest and History:
// target, started_at, duration_ms, outcome, status_code, error, cert_expires_at.
func scanResult(row interface{ Scan(dest ...any) error }) (checker.Result, error) {
	var (
		target        string
		startedAt     int64
		durationMs    sql.NullInt64
		outcome       string
		statusCode    sql.NullInt64
		errMsg        sql.NullString
		certExpiresAt sql.NullInt64
	)
	if err := row.Scan(&target, &startedAt, &durationMs, &outcome, &statusCode, &errMsg, &certExpiresAt); err != nil {
		return checker.Result{}, err
	}
	var cert time.Time
	if certExpiresAt.Valid {
		cert = time.UnixMilli(certExpiresAt.Int64).UTC()
	}
	return checker.Result{
		TargetName:    target,
		StartedAt:     time.UnixMilli(startedAt).UTC(),
		Duration:      time.Duration(durationMs.Int64) * time.Millisecond,
		Outcome:       checker.Outcome(outcome),
		StatusCode:    int(statusCode.Int64),
		Err:           errMsg.String,
		CertExpiresAt: cert,
	}, nil
}
