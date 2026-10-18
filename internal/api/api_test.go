package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

// --- helpers -----------------------------------------------------------------

// testVersion is the build version every test server reports.
const testVersion = "test-version"

// testLogger discards output so a failing store path does not spam the test log.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newStore opens a real SQLite database under the test's temporary directory.
// The returned store is closed on cleanup; tests that need a broken store close
// it earlier (the extra Close is a no-op).
func newStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newServer starts an httptest server around New and returns its base URL.
func newServer(t *testing.T, cfg config.Config, st *store.Store) string {
	t.Helper()

	srv := httptest.NewServer(New(config.NewAtomic(&cfg), st, testVersion, testLogger()))
	t.Cleanup(srv.Close)
	return srv.URL
}

// target builds a config target with sane durations; only name and URL matter
// to the API.
func target(name, url string) config.Target {
	return config.Target{Name: name, URL: url, Interval: time.Minute, Timeout: 5 * time.Second}
}

// seed records a result at the given age before now.
func seed(t *testing.T, st *store.Store, name string, age time.Duration, r checker.Result) {
	t.Helper()

	r.TargetName = name
	r.StartedAt = time.Now().Add(-age)
	if err := st.RecordResult(r); err != nil {
		t.Fatalf("RecordResult(%s, %s): %v", name, r.Outcome, err)
	}
}

// getJSON performs GET path and decodes the body into out, returning the status
// code and the raw body.
func getJSON(t *testing.T, url string, out any) (int, []byte) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("decode %s: %v (body: %s)", url, err, body)
		}
	}
	return resp.StatusCode, body
}

// --- payload mirrors (independent of the production types) -------------------

type jsonStatus struct {
	CheckedAt string `json:"checked_at"`
	Targets   []struct {
		Name      string      `json:"name"`
		URL       string      `json:"url"`
		Last      *jsonResult `json:"last"`
		Recent    []string    `json:"recent"`
		Stats     jsonStats   `json:"stats"`
		Uptime24h *float64    `json:"uptime_24h"`
		Uptime7d  *float64    `json:"uptime_7d"`
	} `json:"targets"`
}

type jsonResult struct {
	Outcome       string  `json:"outcome"`
	StatusCode    int     `json:"status_code"`
	DurationMs    int64   `json:"duration_ms"`
	At            string  `json:"at"`
	Error         string  `json:"error"`
	CertExpiresAt *string `json:"cert_expires_at"`
}

type jsonStats struct {
	Up            int    `json:"up"`
	Down          int    `json:"down"`
	Timeout       int    `json:"timeout"`
	Error         int    `json:"error"`
	Skipped       int    `json:"skipped"`
	AvgDurationMs int64  `json:"avg_duration_ms"`
	P50DurationMs *int64 `json:"p50_duration_ms"`
	P95DurationMs *int64 `json:"p95_duration_ms"`
}

type errorPayload struct {
	Error string `json:"error"`
}

// jsonHealth mirrors the /healthz payload.
type jsonHealth struct {
	OK                  bool     `json:"ok"`
	Version             string   `json:"version"`
	UptimeSeconds       float64  `json:"uptime_seconds"`
	LastWriteAgeSeconds *float64 `json:"last_write_age_seconds"`
}

// --- tests -------------------------------------------------------------------

// TestStatusShape seeds three targets with a mixed outcome history and asserts
// the whole /api/status payload: config order, URLs, the last result, the
// newest-first recent strip capped at 30, and the 24h statistics.
func TestStatusShape(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{
		target("alpha", "https://alpha.example/health"),
		target("bravo", "https://bravo.example/health"),
		target("charlie", "https://charlie.example/health"),
	}}

	// alpha: five distinct outcomes, newest first.
	seed(t, st, "alpha", 1*time.Minute, checker.Result{Duration: 10 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200})
	seed(t, st, "alpha", 2*time.Minute, checker.Result{Duration: 20 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 503})
	seed(t, st, "alpha", 3*time.Minute, checker.Result{Duration: 30 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200})
	seed(t, st, "alpha", 4*time.Minute, checker.Result{Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"})
	seed(t, st, "alpha", 5*time.Minute, checker.Result{Duration: 40 * time.Millisecond, Outcome: checker.OutcomeTimeout, Err: "context deadline exceeded"})

	// bravo: 40 rows so the recent strip must be capped at 30. The newest row
	// carries a certificate so the JSON cert_expires_at field is exercised; the
	// API renders it with second precision (RFC3339, like "at"), so the seed is
	// truncated to match.
	bravoCert := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	for i := 1; i <= 40; i++ {
		res := checker.Result{Duration: 5 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200}
		if i == 1 {
			res.CertExpiresAt = bravoCert
		}
		seed(t, st, "bravo", time.Duration(i)*time.Minute, res)
	}

	// charlie has no rows on purpose.

	base := newServer(t, cfg, st)
	var got jsonStatus
	status, _ := getJSON(t, base+"/api/status", &got)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	if _, err := time.Parse(time.RFC3339, got.CheckedAt); err != nil {
		t.Errorf("checked_at = %q, not RFC3339: %v", got.CheckedAt, err)
	}
	if len(got.Targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(got.Targets))
	}

	// Config order and URLs.
	wantNames := []string{"alpha", "bravo", "charlie"}
	wantURLs := []string{
		"https://alpha.example/health",
		"https://bravo.example/health",
		"https://charlie.example/health",
	}
	for i, want := range wantNames {
		if got.Targets[i].Name != want {
			t.Errorf("targets[%d].name = %q, want %q", i, got.Targets[i].Name, want)
		}
		if got.Targets[i].URL != wantURLs[i] {
			t.Errorf("targets[%d].url = %q, want %q", i, got.Targets[i].URL, wantURLs[i])
		}
	}

	// alpha: last result.
	alpha := got.Targets[0]
	if alpha.Last == nil {
		t.Fatal("alpha.last = null, want the newest result")
	}
	if alpha.Last.Outcome != "up" {
		t.Errorf("alpha.last.outcome = %q, want %q", alpha.Last.Outcome, "up")
	}
	if alpha.Last.StatusCode != 200 {
		t.Errorf("alpha.last.status_code = %d, want 200", alpha.Last.StatusCode)
	}
	if alpha.Last.DurationMs != 10 {
		t.Errorf("alpha.last.duration_ms = %d, want 10", alpha.Last.DurationMs)
	}
	if alpha.Last.Error != "" {
		t.Errorf("alpha.last.error = %q, want empty", alpha.Last.Error)
	}
	if _, err := time.Parse(time.RFC3339, alpha.Last.At); err != nil {
		t.Errorf("alpha.last.at = %q, not RFC3339: %v", alpha.Last.At, err)
	}
	// The newest alpha row is seeded one minute in the past, which also proves
	// "at" tracks the row's timestamp rather than the server clock.
	if at, _ := time.Parse(time.RFC3339, alpha.Last.At); time.Since(at) > 2*time.Minute {
		t.Errorf("alpha.last.at = %v, want the newest row (~1 minute ago)", at)
	}
	if alpha.Last.CertExpiresAt != nil {
		t.Errorf("alpha.last.cert_expires_at = %q, want null for a result without a certificate", *alpha.Last.CertExpiresAt)
	}

	// alpha: recent strip, newest first.
	wantRecent := []string{"up", "down", "up", "skipped", "timeout"}
	if len(alpha.Recent) != len(wantRecent) {
		t.Fatalf("alpha.recent = %v, want %v", alpha.Recent, wantRecent)
	}
	for i, want := range wantRecent {
		if alpha.Recent[i] != want {
			t.Errorf("alpha.recent[%d] = %q, want %q", i, alpha.Recent[i], want)
		}
	}

	// alpha: 24h stats. avg = (10+20+30+0+40)/5 = 20. The percentile estimates
	// come from the histogram buckets over completed checks (skipped excluded):
	// durations 10, 20, 30, 40 ms, so p50 rank 2 -> the 25ms ceiling and p95
	// rank 4 -> the 50ms ceiling.
	if alpha.Stats.Up != 2 || alpha.Stats.Down != 1 || alpha.Stats.Timeout != 1 ||
		alpha.Stats.Error != 0 || alpha.Stats.Skipped != 1 || alpha.Stats.AvgDurationMs != 20 {
		t.Errorf("alpha.stats = %+v, want 2 up/1 down/1 timeout/1 skipped, avg 20ms", alpha.Stats)
	}
	if alpha.Stats.P50DurationMs == nil || *alpha.Stats.P50DurationMs != 25 {
		t.Errorf("alpha.stats.p50_duration_ms = %v, want 25", alpha.Stats.P50DurationMs)
	}
	if alpha.Stats.P95DurationMs == nil || *alpha.Stats.P95DurationMs != 50 {
		t.Errorf("alpha.stats.p95_duration_ms = %v, want 50", alpha.Stats.P95DurationMs)
	}

	// alpha: uptime. Completed checks are 2 up + 1 down + 1 timeout = 4, and the
	// skipped row is excluded, so 2/4 = 50.0% in both windows (every row is
	// inside 24h, hence also inside 7d).
	for _, tc := range []struct {
		name string
		got  *float64
	}{{"uptime_24h", alpha.Uptime24h}, {"uptime_7d", alpha.Uptime7d}} {
		if tc.got == nil {
			t.Errorf("alpha.%s = null, want 50", tc.name)
			continue
		}
		if *tc.got != 50.0 {
			t.Errorf("alpha.%s = %v, want 50", tc.name, *tc.got)
		}
	}

	// bravo: recent capped at 30 even though 40 rows exist; stats count all 40.
	bravo := got.Targets[1]
	if len(bravo.Recent) != 30 {
		t.Errorf("bravo.recent length = %d, want 30", len(bravo.Recent))
	}
	for i, o := range bravo.Recent {
		if o != "up" {
			t.Errorf("bravo.recent[%d] = %q, want %q", i, o, "up")
			break
		}
	}
	if bravo.Stats.Up != 40 || bravo.Stats.AvgDurationMs != 5 {
		t.Errorf("bravo.stats = %+v, want 40 up with avg 5ms", bravo.Stats)
	}
	if bravo.Last == nil || bravo.Last.Outcome != "up" {
		t.Errorf("bravo.last = %+v, want the newest up result", bravo.Last)
	}
	if bravo.Last != nil {
		if bravo.Last.CertExpiresAt == nil {
			t.Error("bravo.last.cert_expires_at = null, want the seeded expiry")
		} else if got, err := time.Parse(time.RFC3339, *bravo.Last.CertExpiresAt); err != nil {
			t.Errorf("bravo.last.cert_expires_at = %q, not RFC3339: %v", *bravo.Last.CertExpiresAt, err)
		} else if !got.Equal(bravoCert) {
			t.Errorf("bravo.last.cert_expires_at = %v, want %v", got, bravoCert)
		}
	}
	// bravo is 40/40 up -> 100.0% in both windows, and 40 checks at exactly 5ms
	// put both percentile estimates on the first bucket's 5ms ceiling.
	if bravo.Uptime24h == nil || *bravo.Uptime24h != 100.0 {
		t.Errorf("bravo.uptime_24h = %v, want 100", bravo.Uptime24h)
	}
	if bravo.Uptime7d == nil || *bravo.Uptime7d != 100.0 {
		t.Errorf("bravo.uptime_7d = %v, want 100", bravo.Uptime7d)
	}
	if bravo.Stats.P50DurationMs == nil || *bravo.Stats.P50DurationMs != 5 {
		t.Errorf("bravo.stats.p50_duration_ms = %v, want 5", bravo.Stats.P50DurationMs)
	}
	if bravo.Stats.P95DurationMs == nil || *bravo.Stats.P95DurationMs != 5 {
		t.Errorf("bravo.stats.p95_duration_ms = %v, want 5", bravo.Stats.P95DurationMs)
	}

	// charlie has no rows, so both uptime figures and both percentile estimates
	// are null.
	if got.Targets[2].Uptime24h != nil || got.Targets[2].Uptime7d != nil {
		t.Errorf("charlie uptime = (%v, %v), want (null, null)", got.Targets[2].Uptime24h, got.Targets[2].Uptime7d)
	}
	if got.Targets[2].Stats.P50DurationMs != nil || got.Targets[2].Stats.P95DurationMs != nil {
		t.Errorf("charlie percentiles = (%v, %v), want (null, null)", got.Targets[2].Stats.P50DurationMs, got.Targets[2].Stats.P95DurationMs)
	}
}

// TestStatusEmptyTarget asserts the no-rows representation: null last, empty
// recent (not null), zero statistics.
func TestStatusEmptyTarget(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("idle", "https://idle.example/health")}}

	base := newServer(t, cfg, st)
	var got jsonStatus
	status, body := getJSON(t, base+"/api/status", &got)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(got.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(got.Targets))
	}
	ts := got.Targets[0]
	if ts.Last != nil {
		t.Errorf("last = %+v, want null", ts.Last)
	}
	if ts.Recent == nil {
		t.Error("recent = null, want an empty array")
	}
	if len(ts.Recent) != 0 {
		t.Errorf("recent = %v, want empty", ts.Recent)
	}
	if ts.Stats != (jsonStats{}) {
		t.Errorf("stats = %+v, want zeros", ts.Stats)
	}
	// The raw JSON must say "recent":[] and "last":null, not the Go zero values,
	// and both uptime figures must be null rather than 0.
	if !strings.Contains(string(body), `"recent":[]`) || !strings.Contains(string(body), `"last":null`) {
		t.Errorf("body = %s, want \"recent\":[] and \"last\":null", body)
	}
	for _, field := range []string{`"uptime_24h":null`, `"uptime_7d":null`} {
		if !strings.Contains(string(body), field) {
			t.Errorf("body = %s, want %s", body, field)
		}
	}
}

// TestHistory seeds 600 rows and checks the default, explicit, capped, and
// invalid limits plus the unknown-target 404.
func TestHistory(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}

	const rows = 600
	// Second precision on purpose: the API formats the expiry as plain RFC3339.
	cert := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	for i := 1; i <= rows; i++ {
		res := checker.Result{
			Duration: time.Duration(i) * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200,
		}
		if i == 1 {
			res.CertExpiresAt = cert
		}
		seed(t, st, "alpha", time.Duration(i)*time.Second, res)
	}

	base := newServer(t, cfg, st)

	// Default limit is 50, newest first.
	var def []jsonResult
	status, _ := getJSON(t, base+"/api/history/alpha", &def)
	if status != http.StatusOK {
		t.Fatalf("default status = %d, want 200", status)
	}
	if len(def) != 50 {
		t.Fatalf("default returned %d rows, want 50", len(def))
	}
	// i=1 is the newest (age 1s), so duration_ms counts down from 1.
	if def[0].DurationMs != 1 || def[49].DurationMs != 50 {
		t.Errorf("default first/last duration = %d/%d, want 1/50 (newest first)", def[0].DurationMs, def[49].DurationMs)
	}
	// The newest row's stored certificate rides along in the history element.
	if def[0].CertExpiresAt == nil {
		t.Error("history[0].cert_expires_at = null, want the seeded expiry")
	} else if got, err := time.Parse(time.RFC3339, *def[0].CertExpiresAt); err != nil {
		t.Errorf("history[0].cert_expires_at = %q, not RFC3339: %v", *def[0].CertExpiresAt, err)
	} else if !got.Equal(cert) {
		t.Errorf("history[0].cert_expires_at = %v, want %v", got, cert)
	}

	// Explicit limit returns that many of the newest rows.
	var five []jsonResult
	status, _ = getJSON(t, base+"/api/history/alpha?limit=5", &five)
	if status != http.StatusOK {
		t.Fatalf("limit=5 status = %d, want 200", status)
	}
	if len(five) != 5 {
		t.Fatalf("limit=5 returned %d rows, want 5", len(five))
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		if five[i].DurationMs != want {
			t.Errorf("limit=5 row %d duration = %d, want %d", i, five[i].DurationMs, want)
		}
	}

	// A limit above the maximum is capped at 500.
	var capped []jsonResult
	status, _ = getJSON(t, base+"/api/history/alpha?limit=9999", &capped)
	if status != http.StatusOK {
		t.Fatalf("limit=9999 status = %d, want 200", status)
	}
	if len(capped) != 500 {
		t.Errorf("limit=9999 returned %d rows, want 500", len(capped))
	}

	// Absent, non-positive, and non-numeric limits all fall back to 50.
	for _, q := range []string{"?limit=0", "?limit=-3", "?limit=abc"} {
		var got []jsonResult
		status, _ = getJSON(t, base+"/api/history/alpha"+q, &got)
		if status != http.StatusOK {
			t.Errorf("%s status = %d, want 200", q, status)
			continue
		}
		if len(got) != 50 {
			t.Errorf("%s returned %d rows, want the default 50", q, len(got))
		}
	}

	// An integer too large to parse means "as many as allowed" and is capped.
	var over []jsonResult
	status, _ = getJSON(t, base+"/api/history/alpha?limit=99999999999999999999", &over)
	if status != http.StatusOK {
		t.Errorf("overflow limit status = %d, want 200", status)
	} else if len(over) != 500 {
		t.Errorf("overflow limit returned %d rows, want the cap 500", len(over))
	}

	// Unknown target: 404 with a JSON error. The name is unknown because config
	// does not list it, even though it may match an empty result set.
	var errBody errorPayload
	status, _ = getJSON(t, base+"/api/history/nope", &errBody)
	if status != http.StatusNotFound {
		t.Errorf("unknown target status = %d, want 404", status)
	}
	if errBody.Error != "target not found" {
		t.Errorf("unknown target error = %q, want %q", errBody.Error, "target not found")
	}
}

// TestMethodNotAllowed asserts non-GET methods on known paths are answered with
// a JSON 405.
func TestMethodNotAllowed(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}
	base := newServer(t, cfg, st)

	for _, path := range []string{"/api/status", "/api/history/alpha", "/healthz"} {
		resp, err := http.Post(base+path, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want 405", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("POST %s Content-Type = %q, want application/json", path, ct)
		}
		var errBody errorPayload
		if err := json.Unmarshal(body, &errBody); err != nil {
			t.Errorf("POST %s body %q is not JSON: %v", path, body, err)
		}
		if errBody.Error != "method not allowed" {
			t.Errorf("POST %s error = %q, want %q", path, errBody.Error, "method not allowed")
		}
	}
}

// TestUnknownPathIsNotFound documents that unrecognised paths keep the mux
// default 404.
func TestUnknownPathIsNotFound(t *testing.T) {
	st := newStore(t)
	base := newServer(t, config.Config{}, st)

	resp, err := http.Get(base + "/api/unknown")
	if err != nil {
		t.Fatalf("GET /api/unknown: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestStoreErrorPath closes the store and asserts the handlers degrade to a JSON
// 500 instead of leaking or panicking.
func TestStoreErrorPath(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	base := newServer(t, cfg, st)

	for _, path := range []string{"/api/status", "/api/history/alpha", "/healthz"} {
		var errBody errorPayload
		status, body := getJSON(t, base+path, &errBody)
		if status != http.StatusInternalServerError {
			t.Errorf("GET %s status = %d, want 500", path, status)
		}
		if errBody.Error != "internal error" {
			t.Errorf("GET %s error = %q, want %q", path, errBody.Error, "internal error")
		}
		// The internal detail must not reach the client.
		if strings.Contains(string(body), "sql") || strings.Contains(string(body), "closed") {
			t.Errorf("GET %s leaked internals: %s", path, body)
		}
	}
}

// TestNilStore covers the defensive guard for a missing store dependency.
func TestNilStore(t *testing.T) {
	base := newServer(t, config.Config{Targets: []config.Target{target("alpha", "https://a/")}}, nil)

	var errBody errorPayload
	status, _ := getJSON(t, base+"/api/status", &errBody)
	if status != http.StatusInternalServerError || errBody.Error != "internal error" {
		t.Errorf("/api/status = (%d, %q), want (500, \"internal error\")", status, errBody.Error)
	}

	status, _ = getJSON(t, base+"/api/history/alpha", &errBody)
	if status != http.StatusInternalServerError || errBody.Error != "internal error" {
		t.Errorf("/api/history/alpha = (%d, %q), want (500, \"internal error\")", status, errBody.Error)
	}

	status, _ = getJSON(t, base+"/healthz", &errBody)
	if status != http.StatusInternalServerError || errBody.Error != "internal error" {
		t.Errorf("/healthz = (%d, %q), want (500, \"internal error\")", status, errBody.Error)
	}
}

// TestNilLogger covers the nil-logger fallback in New.
func TestNilLogger(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}

	srv := httptest.NewServer(New(config.NewAtomic(&cfg), st, testVersion, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHealthzWithData covers the populated shape: ok, version, a growing
// uptime, and a numeric last-write age.
func TestHealthzWithData(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}
	seed(t, st, "alpha", 2*time.Minute, checker.Result{Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200})

	base := newServer(t, cfg, st)

	var first jsonHealth
	status, body := getJSON(t, base+"/healthz", &first)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", status)
	}
	if !first.OK {
		t.Error("ok = false, want true")
	}
	if first.Version != testVersion {
		t.Errorf("version = %q, want %q", first.Version, testVersion)
	}
	if first.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", first.UptimeSeconds)
	}
	if first.LastWriteAgeSeconds == nil {
		t.Fatalf("last_write_age_seconds = null, want a number; body = %s", body)
	}
	// The seeded row is 2 minutes old, so the age is in the tens of seconds or
	// more but nowhere near an hour.
	if *first.LastWriteAgeSeconds < 60 || *first.LastWriteAgeSeconds > 3600 {
		t.Errorf("last_write_age_seconds = %v, want roughly 120", *first.LastWriteAgeSeconds)
	}
	if strings.Contains(string(body), `"last_write_age_seconds":null`) {
		t.Errorf("body = %s, want a numeric age", body)
	}

	// Uptime grows between two probes.
	time.Sleep(25 * time.Millisecond)
	var second jsonHealth
	if status, _ := getJSON(t, base+"/healthz", &second); status != http.StatusOK {
		t.Fatalf("second GET /healthz status = %d, want 200", status)
	}
	if second.UptimeSeconds <= first.UptimeSeconds {
		t.Errorf("uptime did not grow: %v then %v", first.UptimeSeconds, second.UptimeSeconds)
	}
	if second.LastWriteAgeSeconds == nil || *second.LastWriteAgeSeconds < *first.LastWriteAgeSeconds {
		t.Errorf("last_write_age_seconds did not grow: %v then %v", first.LastWriteAgeSeconds, second.LastWriteAgeSeconds)
	}
}

// TestHealthzNoData covers the empty-database shape: the age is null while the
// process itself is still healthy.
func TestHealthzNoData(t *testing.T) {
	st := newStore(t)
	base := newServer(t, config.Config{}, st)

	var got jsonHealth
	status, body := getJSON(t, base+"/healthz", &got)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", status)
	}
	if !got.OK {
		t.Error("ok = false, want true")
	}
	if got.LastWriteAgeSeconds != nil {
		t.Errorf("last_write_age_seconds = %v, want null", *got.LastWriteAgeSeconds)
	}
	if !strings.Contains(string(body), `"last_write_age_seconds":null`) {
		t.Errorf("body = %s, want \"last_write_age_seconds\":null", body)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %s, want \"ok\":true", body)
	}
}
