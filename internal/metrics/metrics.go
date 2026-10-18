package metrics

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

const (
	// contentMetrics is the media type of the exposition body. The classic
	// Prometheus text format has no content negotiation parameter of its own, so
	// the charset is stated explicitly instead (see the package comment for the
	// classic-vs-OpenMetrics decision).
	contentMetrics = "text/plain; charset=utf-8"
)

// outcomeOrder fixes the order in which the outcome-labelled series of a target
// are written, so the exposition body is byte-stable between scrapes with no
// data change.
var outcomeOrder = []checker.Outcome{
	checker.OutcomeUp,
	checker.OutcomeDown,
	checker.OutcomeTimeout,
	checker.OutcomeError,
	checker.OutcomeSkipped,
}

// server holds the read-only dependencies of the handlers.
type server struct {
	cfg     *config.Atomic
	store   *store.Store
	version string
	logger  *slog.Logger
}

// New returns the HTTP handler that serves GET /metrics.
//
// A nil logger falls back to one that discards output. The handler is safe for
// concurrent use and registers FULL paths ("GET /metrics"), so the caller mounts
// it without a StripPrefix. The target list is read from the Atomic holder per
// scrape (D41), so a SIGHUP reload becomes visible without a restart.
func New(live *config.Atomic, st *store.Store, version string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &server{
		cfg:     live,
		store:   st,
		version: version,
		logger:  logger,
	}

	mux := http.NewServeMux()
	// The method-qualified pattern wins for GET; the method-less one answers the
	// remaining methods with a JSON 405 and an Allow header. Everything else is
	// a JSON 404.
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("/metrics", s.handleMethodNotAllowed)
	mux.HandleFunc("/", s.handleNotFound)
	return mux
}

// handleMetrics renders and writes the exposition body.
func (s *server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.store == nil {
		s.logger.Error("metrics: scrape requested without a store")
		writeTextError(w, http.StatusInternalServerError)
		return
	}

	// The body is built completely before anything is written, so a mid-render
	// store failure still produces a clean 500 instead of a truncated 200.
	body, err := s.render()
	if err != nil {
		s.logger.Error("metrics: render", "err", err)
		writeTextError(w, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentMetrics)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// targetSample is the per-target state a single scrape needs: the newest result
// and the windowed outcome counts.
type targetSample struct {
	name    string
	last    checker.Result
	hasLast bool
	stats   store.Stats
}

// count returns the number of stored checks with outcome o inside the window.
func (t targetSample) count(o checker.Outcome) int {
	switch o {
	case checker.OutcomeUp:
		return t.stats.Up
	case checker.OutcomeDown:
		return t.stats.Down
	case checker.OutcomeTimeout:
		return t.stats.Timeout
	case checker.OutcomeError:
		return t.stats.Error
	case checker.OutcomeSkipped:
		return t.stats.Skipped
	}
	return 0
}

// render builds the exposition body.
//
// Each metric family emits its HELP and TYPE lines before every series of that
// family, and all seven families are always emitted even when they carry no
// series, so the output shape does not change as data appears or ages out.
// Families whose value is genuinely unknown for a target (duration, certificate
// expiry, last-check timestamp) simply omit that target's series rather than
// inventing a 0, which a scraper could not tell from a real measurement. The
// duration histogram is the exception that proves the rule: a bucket count of 0
// is a real measurement (no completed check fell in the bucket), so the
// histogram is emitted with zeros instead of being omitted.
//
// The store is queried live per scrape through the snapshot's measured read
// path (D40): one merged aggregate scan per target plus bounded index reads, so
// the scrape always reflects the database, while an in-process registry remains
// the documented future option.
func (s *server) render() (string, error) {
	targets := s.cfg.Load().Targets
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	snap, err := s.store.StatusSnapshot(names)
	if err != nil {
		return "", fmt.Errorf("metrics: snapshot: %w", err)
	}

	samples := make([]targetSample, 0, len(targets))
	for _, t := range targets {
		sn := snap[t.Name]
		samples = append(samples, targetSample{name: t.Name, last: sn.Latest, hasLast: sn.HasLatest, stats: sn.Stats})
	}

	var b strings.Builder

	b.WriteString("# HELP pulsecheck_info Build information for the running PulseCheck process; always 1.\n")
	b.WriteString("# TYPE pulsecheck_info gauge\n")
	fmt.Fprintf(&b, "pulsecheck_info{version=\"%s\"} 1\n", escapeLabelValue(s.version))

	b.WriteString("# HELP pulsecheck_target_up Whether the target's most recent check was up (1) or not (0); a target that has never been checked reports 0.\n")
	b.WriteString("# TYPE pulsecheck_target_up gauge\n")
	for _, sm := range samples {
		up := 0
		if sm.hasLast && sm.last.Outcome == checker.OutcomeUp {
			up = 1
		}
		fmt.Fprintf(&b, "pulsecheck_target_up{target=\"%s\"} %d\n", escapeLabelValue(sm.name), up)
	}

	b.WriteString("# HELP pulsecheck_check_duration_seconds Duration of the target's most recent check in seconds; absent until the first check.\n")
	b.WriteString("# TYPE pulsecheck_check_duration_seconds gauge\n")
	for _, sm := range samples {
		if !sm.hasLast {
			continue
		}
		fmt.Fprintf(&b, "pulsecheck_check_duration_seconds{target=\"%s\"} %.3f\n",
			escapeLabelValue(sm.name), sm.last.Duration.Seconds())
	}

	// A WINDOWED gauge, deliberately not a _total counter: retention deletes
	// rows older than the retention window, so a process-lifetime counter would
	// keep climbing while the retained history shrank — it would report checks
	// that no longer exist and could never be reconciled against the database.
	// A 24h gauge always describes exactly the rows that are still there.
	b.WriteString("# HELP pulsecheck_check_results_24h Completed checks by outcome over the last 24 hours; a windowed gauge, not a monotonic counter, because retention deletes older rows.\n")
	b.WriteString("# TYPE pulsecheck_check_results_24h gauge\n")
	for _, sm := range samples {
		for _, o := range outcomeOrder {
			fmt.Fprintf(&b, "pulsecheck_check_results_24h{target=\"%s\",outcome=\"%s\"} %d\n",
				escapeLabelValue(sm.name), escapeLabelValue(string(o)), sm.count(o))
		}
	}

	b.WriteString("# HELP pulsecheck_target_cert_expiry_days Days remaining until the leaf TLS certificate seen by the target's most recent check expires, floored; absent when unknown (plain HTTP, or no TLS handshake completed).\n")
	b.WriteString("# TYPE pulsecheck_target_cert_expiry_days gauge\n")
	for _, sm := range samples {
		if !sm.hasLast || sm.last.CertExpiresAt.IsZero() {
			continue
		}
		days := int64(math.Floor(time.Until(sm.last.CertExpiresAt).Hours() / 24))
		fmt.Fprintf(&b, "pulsecheck_target_cert_expiry_days{target=\"%s\"} %d\n", escapeLabelValue(sm.name), days)
	}

	b.WriteString("# HELP pulsecheck_last_check_timestamp_seconds Unix time of the target's most recent check in seconds; absent until the first check.\n")
	b.WriteString("# TYPE pulsecheck_last_check_timestamp_seconds gauge\n")
	for _, sm := range samples {
		if !sm.hasLast {
			continue
		}
		fmt.Fprintf(&b, "pulsecheck_last_check_timestamp_seconds{target=\"%s\"} %d\n",
			escapeLabelValue(sm.name), sm.last.StartedAt.Unix())
	}

	// The windowed duration histogram. Like the results family above it is a
	// window aggregate, not a lifetime histogram: retention deletes rows, so
	// bucket counts can decrease between scrapes. Completed checks only — a
	// skipped round never measured a duration (D30d/D39).
	b.WriteString("# HELP pulsecheck_check_duration_seconds_24h Duration histogram of completed checks over the last 24 hours, in seconds (cumulative classic buckets); skipped rounds are excluded. Windowed: retention shrinks the counts.\n")
	b.WriteString("# TYPE pulsecheck_check_duration_seconds_24h histogram\n")
	for _, sm := range samples {
		for _, bucket := range sm.stats.DurationBuckets {
			le := "+Inf"
			if bucket.UpperBoundMs != math.MaxInt64 {
				le = strconv.FormatFloat(float64(bucket.UpperBoundMs)/1000, 'g', -1, 64)
			}
			fmt.Fprintf(&b, "pulsecheck_check_duration_seconds_24h_bucket{target=\"%s\",le=\"%s\"} %d\n",
				escapeLabelValue(sm.name), le, bucket.Count)
		}
		total := 0
		if n := len(sm.stats.DurationBuckets); n > 0 {
			total = sm.stats.DurationBuckets[n-1].Count
		}
		fmt.Fprintf(&b, "pulsecheck_check_duration_seconds_24h_sum{target=\"%s\"} %.3f\n",
			escapeLabelValue(sm.name), float64(sm.stats.DurationSumMs)/1000)
		fmt.Fprintf(&b, "pulsecheck_check_duration_seconds_24h_count{target=\"%s\"} %d\n",
			escapeLabelValue(sm.name), total)
	}

	return b.String(), nil
}

// labelEscaper escapes the three characters the classic Prometheus text format
// reserves inside a label value: backslash, double quote, and newline. Target
// names come from user configuration, so an unescaped quote would break the
// exposition body — or let a name inject a whole extra series — which makes this
// escaping a correctness requirement rather than a nicety.
//
// strings.Replacer performs one left-to-right pass, so a backslash written by an
// earlier rule is never escaped a second time.
var labelEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	"\n", `\n`,
)

// escapeLabelValue escapes v for use inside a double-quoted Prometheus label.
func escapeLabelValue(v string) string {
	return labelEscaper.Replace(v)
}

// handleMethodNotAllowed answers any non-GET method on /metrics.
func (s *server) handleMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
}

// handleNotFound answers any path other than /metrics.
func (s *server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
}

// errorResponse is the body of every JSON error reply.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes v as compact JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeTextError reports an internal failure on the metrics endpoint. It stays
// in the metrics media type so a scraper never has to parse a JSON body from an
// endpoint it expects to be plain text.
func writeTextError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", contentMetrics)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "internal error\n")
}
