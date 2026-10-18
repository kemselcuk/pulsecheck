package store

import (
	"database/sql"
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
	if !got.CertExpiresAt.Equal(want.CertExpiresAt) {
		t.Errorf("CertExpiresAt = %v, want %v", got.CertExpiresAt, want.CertExpiresAt)
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
	defer func() { _ = second.Close() }()

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

// TestCertExpiresAtRoundtrip proves the certificate expiry survives a
// Record/Latest/History round trip: a set value comes back as the same instant
// in UTC, an unset value comes back as the zero time, and the raw column holds
// Unix milliseconds or NULL accordingly.
func TestCertExpiresAtRoundtrip(t *testing.T) {
	s, _ := openStore(t, "cert_roundtrip.db")
	base := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	withCert := checker.Result{
		TargetName:    "tls",
		StartedAt:     base,
		Duration:      15 * time.Millisecond,
		Outcome:       checker.OutcomeUp,
		StatusCode:    200,
		CertExpiresAt: time.Now().Add(48 * time.Hour).UTC().Truncate(time.Millisecond),
	}
	if err := s.RecordResult(withCert); err != nil {
		t.Fatalf("RecordResult(with cert): %v", err)
	}

	got, ok, err := s.Latest("tls")
	if err != nil {
		t.Fatalf("Latest(tls): %v", err)
	}
	if !ok {
		t.Fatal("Latest(tls): not found")
	}
	if got.CertExpiresAt.IsZero() {
		t.Fatal("CertExpiresAt is zero, want the recorded expiry")
	}
	if !got.CertExpiresAt.Equal(withCert.CertExpiresAt) {
		t.Errorf("CertExpiresAt = %v, want the same instant %v", got.CertExpiresAt, withCert.CertExpiresAt)
	}
	if got.CertExpiresAt.Location() != time.UTC {
		t.Errorf("CertExpiresAt location = %v, want UTC", got.CertExpiresAt.Location())
	}
	assertResult(t, withCert, got)

	hist, err := s.History("tls", 10)
	if err != nil {
		t.Fatalf("History(tls): %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("History(tls) returned %d rows, want 1", len(hist))
	}
	if !hist[0].CertExpiresAt.Equal(withCert.CertExpiresAt) {
		t.Errorf("History CertExpiresAt = %v, want %v", hist[0].CertExpiresAt, withCert.CertExpiresAt)
	}

	withoutCert := checker.Result{
		TargetName: "plain",
		StartedAt:  base.Add(time.Second),
		Duration:   5 * time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}
	if err := s.RecordResult(withoutCert); err != nil {
		t.Fatalf("RecordResult(without cert): %v", err)
	}
	got, ok, err = s.Latest("plain")
	if err != nil {
		t.Fatalf("Latest(plain): %v", err)
	}
	if !ok {
		t.Fatal("Latest(plain): not found")
	}
	if !got.CertExpiresAt.IsZero() {
		t.Errorf("CertExpiresAt = %v, want the zero time when unset", got.CertExpiresAt)
	}
	assertResult(t, withoutCert, got)

	// The raw storage shape: Unix ms for a set value, NULL for an unset one.
	var storedCert sql.NullInt64
	if err := s.db.QueryRow(`SELECT cert_expires_at FROM results WHERE target = 'tls'`).Scan(&storedCert); err != nil {
		t.Fatalf("read cert_expires_at: %v", err)
	}
	if !storedCert.Valid || storedCert.Int64 != withCert.CertExpiresAt.UnixMilli() {
		t.Errorf("stored cert_expires_at = %+v, want %d", storedCert, withCert.CertExpiresAt.UnixMilli())
	}

	var storedNull sql.NullInt64
	if err := s.db.QueryRow(`SELECT cert_expires_at FROM results WHERE target = 'plain'`).Scan(&storedNull); err != nil {
		t.Fatalf("read cert_expires_at: %v", err)
	}
	if storedNull.Valid {
		t.Errorf("stored cert_expires_at = %d, want NULL when unset", storedNull.Int64)
	}
}

// TestOpenMigratesOldSchema is the migration evidence test. It builds a database
// with the pre-v1.1 results table (no cert_expires_at), seeds rows through raw
// SQL, and then confirms that store.Open adds the column without touching the
// existing data and that the full read/write API works afterwards. Reopening
// again proves the migration is idempotent.
func TestOpenMigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old_schema.db")

	// 1. A database exactly as an older PulseCheck release would have left it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE results (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		target      TEXT    NOT NULL,
		started_at  INTEGER NOT NULL,
		outcome     TEXT    NOT NULL CHECK (outcome IN ('up', 'down', 'timeout', 'error', 'skipped')),
		status_code INTEGER,
		duration_ms INTEGER,
		error       TEXT
	)`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_results_target_time ON results (target, started_at)`); err != nil {
		t.Fatalf("create old index: %v", err)
	}

	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	legacy := []checker.Result{
		{TargetName: "legacy", StartedAt: base, Duration: 5 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200},
		{TargetName: "legacy", StartedAt: base.Add(time.Second), Duration: 7 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 500},
	}
	for _, r := range legacy {
		if _, err := db.Exec(
			`INSERT INTO results (target, started_at, outcome, status_code, duration_ms, error) VALUES (?, ?, ?, ?, ?, ?)`,
			r.TargetName, r.StartedAt.UnixMilli(), string(r.Outcome), r.StatusCode, r.Duration.Milliseconds(), r.Err,
		); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// 2. Open must detect the missing column and ALTER it in.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(old db): %v", err)
	}

	has, err := s.hasColumn("results", "cert_expires_at")
	if err != nil {
		t.Fatalf("hasColumn after migration: %v", err)
	}
	if !has {
		t.Fatal("cert_expires_at column missing after Open on an old database")
	}

	// 3. The seeded rows survive; the new column reads back as the zero time.
	hist, err := s.History("legacy", 10)
	if err != nil {
		t.Fatalf("History after migration: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("History returned %d rows, want the 2 seeded rows", len(hist))
	}
	assertResult(t, legacy[1], hist[0]) // newest first
	assertResult(t, legacy[0], hist[1])
	for _, r := range hist {
		if !r.CertExpiresAt.IsZero() {
			t.Errorf("migrated row has CertExpiresAt = %v, want the zero time", r.CertExpiresAt)
		}
	}

	// 4. RecordResult/Latest work against the migrated table, new column included.
	fresh := checker.Result{
		TargetName:    "legacy",
		StartedAt:     base.Add(2 * time.Second),
		Duration:      9 * time.Millisecond,
		Outcome:       checker.OutcomeUp,
		StatusCode:    200,
		CertExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Millisecond),
	}
	if err := s.RecordResult(fresh); err != nil {
		t.Fatalf("RecordResult after migration: %v", err)
	}
	got, ok, err := s.Latest("legacy")
	if err != nil {
		t.Fatalf("Latest after migration: %v", err)
	}
	if !ok {
		t.Fatal("Latest after migration: not found")
	}
	assertResult(t, fresh, got)

	// 5. Idempotent: a further reopen changes nothing and loses no rows.
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })

	hasAgain, err := again.hasColumn("results", "cert_expires_at")
	if err != nil {
		t.Fatalf("hasColumn after reopen: %v", err)
	}
	if !hasAgain {
		t.Fatal("cert_expires_at column missing after reopening a migrated database")
	}

	var count int
	if err := again.db.QueryRow(`SELECT COUNT(*) FROM results WHERE target = 'legacy'`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 { // 2 legacy + 1 fresh
		t.Errorf("row count after reopen = %d, want 3", count)
	}
}
