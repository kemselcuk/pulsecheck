package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

// --- helpers -----------------------------------------------------------------

// newStore opens a real SQLite database under the test's temporary directory.
func newStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/web.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newServer starts a test server around New.
func newServer(t *testing.T, cfg config.Config, st *store.Store) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(New(config.NewAtomic(&cfg), st))
	t.Cleanup(srv.Close)
	return srv
}

// get fetches path from srv and returns the status, the Content-Type, and the
// body.
func get(t *testing.T, srv *httptest.Server, path string) (int, string, string) {
	t.Helper()

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// seed records one result age before now.
func seed(t *testing.T, st *store.Store, name string, age time.Duration, r checker.Result) {
	t.Helper()

	r.TargetName = name
	r.StartedAt = time.Now().Add(-age)
	if err := st.RecordResult(r); err != nil {
		t.Fatalf("RecordResult(%s, %s): %v", name, r.Outcome, err)
	}
}

// target builds a config target with sane durations.
func target(name, url string) config.Target {
	return config.Target{Name: name, URL: url, Interval: time.Minute, Timeout: 5 * time.Second}
}

// pointsPattern extracts the polyline's points attribute.
var pointsPattern = regexp.MustCompile(`points="([^"]*)"`)

// hrefPattern extracts every href attribute value.
var hrefPattern = regexp.MustCompile(`href="([^"]*)"`)

// assertSelfContained pins the offline requirement: a page must load nothing
// from the network. The only hrefs allowed are root-relative or in-page ones,
// and no src/link/script-loading attribute may appear. A target URL displayed as
// text is data, not a resource, so a https:// string in text content is fine.
func assertSelfContained(t *testing.T, page, body string) {
	t.Helper()

	for _, marker := range []string{`src=`, `<link`, `="http://`, `="https://`, `="//`, "javascript:"} {
		if strings.Contains(body, marker) {
			t.Errorf("%s page contains %q; it must reference no external resource", page, marker)
		}
	}
	for _, m := range hrefPattern.FindAllStringSubmatch(body, -1) {
		if !strings.HasPrefix(m[1], "/") && !strings.HasPrefix(m[1], "#") {
			t.Errorf("%s page has a non-relative href %q", page, m[1])
		}
	}
}

// --- index tests -------------------------------------------------------------

func TestIndexServed(t *testing.T) {
	srv := newServer(t, config.Config{}, newStore(t))

	status, contentType, body := get(t, srv, "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", contentType, "text/html; charset=utf-8")
	}
	for _, marker := range []string{"PulseCheck", "api/status"} {
		if !strings.Contains(body, marker) {
			t.Errorf("page does not contain %q", marker)
		}
	}
}

// TestIndexHasDetailsLink pins that the dashboard cards link to the detail page.
// The cards are rendered client-side, so the link construction is part of the
// page's script.
func TestIndexHasDetailsLink(t *testing.T) {
	srv := newServer(t, config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}, newStore(t))

	_, _, body := get(t, srv, "/")
	for _, marker := range []string{"details", `"/targets/"`} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard does not contain %q", marker)
		}
	}
}

// TestIndexShowsUptimeAndCert pins the dashboard's second stats line: the
// cards are built client-side, so the field references and the warn class live
// in the page's script.
func TestIndexShowsUptimeAndCert(t *testing.T) {
	srv := newServer(t, config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}, newStore(t))

	_, _, body := get(t, srv, "/")
	for _, marker := range []string{"uptime_24h", "uptime_7d", "cert_expires_at", `"warn"`} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard does not contain %q", marker)
		}
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	srv := newServer(t, config.Config{}, newStore(t))

	for _, path := range []string{"/nope", "/index.html", "/api/status"} {
		if status, _, _ := get(t, srv, path); status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}
}

// TestNoExternalResources pins the offline requirement for the dashboard.
func TestNoExternalResources(t *testing.T) {
	srv := newServer(t, config.Config{}, newStore(t))
	_, _, body := get(t, srv, "/")

	for _, scheme := range []string{"http://", "https://"} {
		if strings.Contains(body, scheme) {
			t.Errorf("page contains %q; the screen must work fully offline", scheme)
		}
	}
	// The page is served as a single file, so referencing a src or a literal href
	// at all would mean an external asset; assert there are none.
	if strings.Contains(body, "src=") || strings.Contains(body, "href=") {
		t.Errorf("page references an external attribute (src=/href=); it must be self-contained")
	}
}

// --- detail page tests -------------------------------------------------------

// TestDetailPageChart seeds a hand-computed mix and asserts the rendered detail
// page: the badge, the uptime figures, the certificate line (amber, because
// the seeded certificate is under 30 days away), and an SVG sparkline with
// exactly one point per stored result.
func TestDetailPageChart(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}

	// Five results, newest first: up 10ms, down 20ms, timeout 30ms, skipped 0ms,
	// up 40ms. Completed checks are 2 up + 1 down + 1 timeout = 4, so uptime is
	// 50.0% — a 40.0% would mean the skipped row leaked into the denominator.
	seed(t, st, "alpha", 1*time.Minute, checker.Result{Duration: 10 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200, CertExpiresAt: time.Now().Add(72 * time.Hour)})
	seed(t, st, "alpha", 2*time.Minute, checker.Result{Duration: 20 * time.Millisecond, Outcome: checker.OutcomeDown, StatusCode: 503})
	seed(t, st, "alpha", 3*time.Minute, checker.Result{Duration: 30 * time.Millisecond, Outcome: checker.OutcomeTimeout, Err: "context deadline exceeded"})
	seed(t, st, "alpha", 4*time.Minute, checker.Result{Outcome: checker.OutcomeSkipped, Err: "previous check still in flight"})
	seed(t, st, "alpha", 5*time.Minute, checker.Result{Duration: 40 * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200})

	srv := newServer(t, cfg, st)
	status, contentType, body := get(t, srv, "/targets/alpha")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", contentType, "text/html; charset=utf-8")
	}

	for _, marker := range []string{
		"alpha",
		"https://alpha.example/health",
		`class="badge up"`,
		"50.0%",
		"cert expires in:",
		"2 days",
		`class="warn"`,
		`href="/"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("detail page does not contain %q\n---\n%s", marker, body)
		}
	}
	if strings.Contains(body, "40.0%") {
		t.Errorf("detail page shows 40.0%%; the skipped row must be excluded\n---\n%s", body)
	}

	if !strings.Contains(body, "<svg") {
		t.Fatalf("detail page has no <svg>\n---\n%s", body)
	}
	if !strings.Contains(body, "<polyline") {
		t.Fatalf("detail page has no <polyline>\n---\n%s", body)
	}

	m := pointsPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("polyline has no points attribute\n---\n%s", body)
	}
	if got := len(strings.Fields(m[1])); got != 5 {
		t.Errorf("polyline has %d points, want 5: %q", got, m[1])
	}

	assertSelfContained(t, "detail", body)
}

// TestDetailPageCertWarningThreshold pins the amber treatment: a certificate
// with fewer than 30 days left carries the warn class, a distant one does not.
func TestDetailPageCertWarningThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires time.Duration
		want    bool
	}{
		{"under threshold", 20 * 24 * time.Hour, true},
		{"over threshold", 100 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}
			seed(t, st, "alpha", time.Minute, checker.Result{
				Outcome: checker.OutcomeUp, StatusCode: 200, CertExpiresAt: time.Now().Add(tc.expires),
			})

			srv := newServer(t, cfg, st)
			status, _, body := get(t, srv, "/targets/alpha")
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if !strings.Contains(body, "cert expires in:") {
				t.Fatalf("detail page lost the certificate line\n---\n%s", body)
			}
			if got := strings.Contains(body, `class="warn"`); got != tc.want {
				t.Errorf("warn class present = %v, want %v (cert %s away)", got, tc.want, tc.expires)
			}
		})
	}
}

// TestDetailPageCapsChartPoints pins the up-to-200 window.
func TestDetailPageCapsChartPoints(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("busy", "https://busy.example/health")}}

	for i := 1; i <= 205; i++ {
		seed(t, st, "busy", time.Duration(i)*time.Second, checker.Result{
			Duration: time.Duration(i) * time.Millisecond, Outcome: checker.OutcomeUp, StatusCode: 200,
		})
	}

	srv := newServer(t, cfg, st)
	_, _, body := get(t, srv, "/targets/busy")

	m := pointsPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("polyline has no points attribute\n---\n%s", body)
	}
	if got := len(strings.Fields(m[1])); got != chartPoints {
		t.Errorf("polyline has %d points, want %d", got, chartPoints)
	}
}

// TestDetailPageEmptyHistory pins the no-chart rendering and the "no data"
// uptime wording for a target that has never been checked.
func TestDetailPageEmptyHistory(t *testing.T) {
	st := newStore(t)
	cfg := config.Config{Targets: []config.Target{target("idle", "https://idle.example/health")}}

	srv := newServer(t, cfg, st)
	status, _, body := get(t, srv, "/targets/idle")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if strings.Contains(body, "<svg") || strings.Contains(body, "<polyline") {
		t.Errorf("an empty target must render without a chart\n---\n%s", body)
	}
	if !strings.Contains(body, `class="badge unknown"`) {
		t.Errorf("empty target badge missing\n---\n%s", body)
	}
	if strings.Count(body, "no data") != 2 {
		t.Errorf("want both uptime figures to say \"no data\"\n---\n%s", body)
	}
	assertSelfContained(t, "detail", body)
}

// TestDetailPageUnknownTarget pins the 404 for a name that configuration does
// not list.
func TestDetailPageUnknownTarget(t *testing.T) {
	srv := newServer(t, config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}, newStore(t))

	if status, _, _ := get(t, srv, "/targets/nope"); status != http.StatusNotFound {
		t.Errorf("GET /targets/nope status = %d, want 404", status)
	}
}

// TestDetailPageStoreError covers the degraded path.
func TestDetailPageStoreError(t *testing.T) {
	// web.New has no logger parameter, so the handler logs through the default
	// one; discard it for this deliberately failing case.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	st := newStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg := config.Config{Targets: []config.Target{target("alpha", "https://alpha.example/health")}}

	srv := newServer(t, cfg, st)
	if status, _, _ := get(t, srv, "/targets/alpha"); status != http.StatusInternalServerError {
		t.Errorf("GET /targets/alpha status = %d, want 500", status)
	}
}

// TestSparklinePoints pins the geometry directly: oldest left, newest right, the
// slowest at the top, the fastest at the bottom, a flat series on the middle
// line, and a single point centred.
func TestSparklinePoints(t *testing.T) {
	results := func(ms ...int64) []checker.Result {
		out := make([]checker.Result, len(ms))
		for i, m := range ms {
			out[i] = checker.Result{Duration: time.Duration(m) * time.Millisecond}
		}
		return out
	}

	if got := sparklinePoints(nil); got != "" {
		t.Errorf("sparklinePoints(nil) = %q, want empty", got)
	}

	// Flat series: one point, centred vertically and horizontally.
	if got := sparklinePoints(results(7)); got != "400.0,60.0" {
		t.Errorf("single point = %q, want %q", got, "400.0,60.0")
	}

	// Two points: the oldest (30ms) is left and highest, the newest (10ms) is
	// right and lowest.
	want := "10.0,10.0 790.0,110.0"
	if got := sparklinePoints(results(30, 10)); got != want {
		t.Errorf("two points = %q, want %q", got, want)
	}

	// Identical durations are flat rather than a division by zero.
	if got := sparklinePoints(results(5, 5, 5)); got != "10.0,60.0 400.0,60.0 790.0,60.0" {
		t.Errorf("flat series = %q", got)
	}
}
