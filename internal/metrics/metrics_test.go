package metrics

import (
	"encoding/json"
	"fmt"
	"io"
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

func newStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// scrape starts a server around New and returns the body of GET /metrics.
func scrape(t *testing.T, cfg config.Config, st *store.Store) string {
	t.Helper()

	srv := httptest.NewServer(New(cfg, st, "test-version", nil))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/plain; charset=utf-8")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// seed records one result age before now, unless the caller already set an
// explicit StartedAt (which keeps the expected values deterministic).
func seed(t *testing.T, st *store.Store, name string, age time.Duration, r checker.Result) {
	t.Helper()

	r.TargetName = name
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().Add(-age)
	}
	if err := st.RecordResult(r); err != nil {
		t.Fatalf("RecordResult(%s, %s): %v", name, r.Outcome, err)
	}
}

// --- tests -------------------------------------------------------------------

// TestMetricsBody seeds a checked target and a never-checked one and asserts
// every expected series with its exact value.
func TestMetricsBody(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{
		{Name: "alpha", URL: "https://alpha.example/health"},
		{Name: "idle", URL: "https://idle.example/health"},
	}}

	// The newest row (1 minute old) is up with a duration and a certificate; the
	// older rows add a down and a skipped count inside the window. All three
	// timestamps come from one base so the asserted unix second is exact.
	now := time.Now()
	newest := now.Add(-time.Minute).Truncate(time.Millisecond)
	seed(t, st, "alpha", 0, checker.Result{
		StartedAt:     newest,
		Duration:      12 * time.Millisecond,
		Outcome:       checker.OutcomeUp,
		StatusCode:    200,
		CertExpiresAt: now.Add(72 * time.Hour),
	})
	seed(t, st, "alpha", 0, checker.Result{StartedAt: now.Add(-2 * time.Minute), Duration: 30 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 503})
	seed(t, st, "alpha", 0, checker.Result{StartedAt: now.Add(-3 * time.Minute), Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"})

	body := scrape(t, cfg, st)

	wantLines := []string{
		"pulsecheck_info{version=\"test-version\"} 1",

		"pulsecheck_target_up{target=\"alpha\"} 1",
		"pulsecheck_target_up{target=\"idle\"} 0", // never checked -> 0

		"pulsecheck_check_duration_seconds{target=\"alpha\"} 0.012",

		"pulsecheck_check_results_24h{target=\"alpha\",outcome=\"up\"} 1",
		"pulsecheck_check_results_24h{target=\"alpha\",outcome=\"down\"} 1",
		"pulsecheck_check_results_24h{target=\"alpha\",outcome=\"timeout\"} 0",
		"pulsecheck_check_results_24h{target=\"alpha\",outcome=\"error\"} 0",
		"pulsecheck_check_results_24h{target=\"alpha\",outcome=\"skipped\"} 1",
		"pulsecheck_check_results_24h{target=\"idle\",outcome=\"up\"} 0",

		"pulsecheck_target_cert_expiry_days{target=\"alpha\"} 2", // 72h floored to 2 days

		fmt.Sprintf("pulsecheck_last_check_timestamp_seconds{target=\"alpha\"} %d", newest.Unix()),
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("body missing line %q\n---\n%s", want, body)
		}
	}

	// Families whose value is unknown for a target must be ABSENT, not zero.
	for _, absent := range []string{
		"pulsecheck_check_duration_seconds{target=\"idle\"}",
		"pulsecheck_last_check_timestamp_seconds{target=\"idle\"}",
		"pulsecheck_target_cert_expiry_days{target=\"idle\"}",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("body must not contain %q\n---\n%s", absent, body)
		}
	}

	// The classic format has no terminator marker.
	if strings.Contains(body, "# EOF") {
		t.Error("body contains \"# EOF\"; the classic text format has no terminator")
	}
}

// TestMetricsLabelling covers a set of series with no data, the config order,
// and the outcome label set.
func TestMetricsEmptyStore(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{{Name: "idle", URL: "https://idle.example/health"}}}

	body := scrape(t, cfg, st)

	if !strings.Contains(body, "pulsecheck_target_up{target=\"idle\"} 0\n") {
		t.Errorf("never-checked target must report 0\n---\n%s", body)
	}
	for _, o := range []string{"up", "down", "timeout", "error", "skipped"} {
		want := fmt.Sprintf("pulsecheck_check_results_24h{target=\"idle\",outcome=\"%s\"} 0", o)
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, "pulsecheck_last_check_timestamp_seconds{") {
		t.Error("empty store must not expose a last-check timestamp series")
	}
	if strings.Contains(body, "pulsecheck_check_duration_seconds{") {
		t.Error("empty store must not expose a duration series")
	}
	if strings.Contains(body, "pulsecheck_target_cert_expiry_days{") {
		t.Error("empty store must not expose a certificate expiry series")
	}
}

// TestMetricsEscaping pins the label escaping with a target name containing a
// quote and a backslash.
func TestMetricsEscaping(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{{Name: `we"ird\name`, URL: "https://x.example/"}}}

	body := scrape(t, cfg, st)

	want := `pulsecheck_target_up{target="we\"ird\\name"} 0`
	if !strings.Contains(body, want+"\n") {
		t.Errorf("body missing escaped series %q\n---\n%s", want, body)
	}
	// The raw, unescaped name must never reach the body.
	if strings.Contains(body, `target="we"ird\name"`) {
		t.Errorf("body contains an unescaped label value\n---\n%s", body)
	}
}

// TestEscapeLabelValue is the unit-level pin for the escaping rules.
func TestEscapeLabelValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a b", "a b"},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{`a\"b`, `a\\\"b`},
		{"\n\"\\", `\n\"\\`},
	}
	for _, tc := range cases {
		if got := escapeLabelValue(tc.in); got != tc.want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMetricsFormatIsStable checks the structural contract of the classic
// format: every HELP line is immediately followed by its TYPE line, every series
// line belongs to a family whose TYPE was already declared, and no series
// appears before its TYPE.
func TestMetricsFormatIsStable(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{{Name: "alpha", URL: "https://alpha.example/health"}}}
	seed(t, st, "alpha", time.Minute, checker.Result{Duration: time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200})

	body := scrape(t, cfg, st)

	declared := map[string]bool{}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			fam := strings.TrimSpace(strings.TrimPrefix(line, "# HELP "))
			if idx := strings.IndexByte(fam, ' '); idx >= 0 {
				fam = fam[:idx]
			}
			next := ""
			if i+1 < len(lines) {
				next = lines[i+1]
			}
			if !strings.HasPrefix(next, "# TYPE "+fam+" ") {
				t.Errorf("HELP for %q at line %d is not immediately followed by its TYPE (got %q)", fam, i+1, next)
			}
		case strings.HasPrefix(line, "# TYPE "):
			rest := strings.TrimPrefix(line, "# TYPE ")
			fam := strings.Fields(rest)[0]
			if len(strings.Fields(rest)) != 2 {
				t.Errorf("TYPE line %q must be \"# TYPE <name> <type>\"", line)
			}
			declared[fam] = true
		case line == "":
			t.Errorf("unexpected blank line at %d", i+1)
		default:
			fam := line
			if idx := strings.IndexByte(fam, '{'); idx >= 0 {
				fam = fam[:idx]
			} else if idx := strings.IndexByte(fam, ' '); idx >= 0 {
				fam = fam[:idx]
			}
			if !declared[fam] {
				t.Errorf("series %q at line %d appears before its TYPE", line, i+1)
			}
			if len(strings.Fields(line)) != 2 {
				t.Errorf("series line %q must be \"<name>{...} <value>\"", line)
			}
		}
	}
}

// TestMetricsMethodNotAllowed covers the JSON 405 with an Allow header.
func TestMetricsMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer(New(config.Config{}, newStore(t), "v", nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics status = %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want %q", allow, http.MethodGet)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode 405 body: %v", err)
	}
	if body.Error != "method not allowed" {
		t.Errorf("error = %q, want %q", body.Error, "method not allowed")
	}
}

// TestMetricsUnknownPath covers the JSON 404 for every other path.
func TestMetricsUnknownPath(t *testing.T) {
	srv := httptest.NewServer(New(config.Config{}, newStore(t), "v", nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/other")
	if err != nil {
		t.Fatalf("GET /other: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /other status = %d, want 404", resp.StatusCode)
	}
	var body errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode 404 body: %v", err)
	}
	if body.Error != "not found" {
		t.Errorf("error = %q, want %q", body.Error, "not found")
	}
}

// TestMetricsStoreError covers the degraded path: a closed store yields a 500 in
// the metrics media type instead of a panic or a truncated 200.
func TestMetricsStoreError(t *testing.T) {
	st := newStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv := httptest.NewServer(New(config.Config{Targets: []config.Target{{Name: "a", URL: "https://a/"}}}, st, "v", nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != contentMetrics {
		t.Errorf("Content-Type = %q, want %q", ct, contentMetrics)
	}
}

// TestMetricsNilStore covers the defensive guard for a missing dependency.
func TestMetricsNilStore(t *testing.T) {
	srv := httptest.NewServer(New(config.Config{}, nil, "v", nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}
