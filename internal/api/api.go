// Package api implements the read-only JSON API described in doc.go.

package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

const (
	// defaultHistoryLimit is the result count of /api/history when the limit
	// query parameter is absent or not a positive integer.
	defaultHistoryLimit = 50

	// maxHistoryLimit caps the result count of /api/history so a single request
	// cannot pull an unbounded slice out of the database.
	maxHistoryLimit = 500

	// genericErrorMessage is the only message ever sent to a client for an
	// internal failure; the real error goes to the logger instead.
	genericErrorMessage = "internal error"
)

// statusResponse is the body of GET /api/status.
type statusResponse struct {
	CheckedAt string         `json:"checked_at"`
	Targets   []targetStatus `json:"targets"`
}

// targetStatus is one entry of the /api/status payload.
type targetStatus struct {
	Name   string      `json:"name"`
	URL    string      `json:"url"`
	Last   *resultView `json:"last"`
	Recent []string    `json:"recent"`
	Stats  statsView   `json:"stats"`
	// Uptime24h and Uptime7d are percentages rounded to one decimal place, or
	// null when the window holds no completed check (store.Uptime's hasData).
	// Skipped rows are excluded from the percentage by the store (decision D30d).
	Uptime24h *float64 `json:"uptime_24h"`
	Uptime7d  *float64 `json:"uptime_7d"`
}

// resultView is the JSON shape shared by the "last" field and the elements of
// /api/history. Times are RFC3339 in UTC; the URL is deliberately absent because
// the store does not persist it.
type resultView struct {
	Outcome    string `json:"outcome"`
	StatusCode int    `json:"status_code"`
	DurationMs int64  `json:"duration_ms"`
	At         string `json:"at"`
	Error      string `json:"error"`
	// CertExpiresAt is the leaf TLS certificate's expiry (RFC3339 UTC), or null
	// when no TLS handshake completed (plain HTTP, or a check that never got a
	// response). It carries the store's NULL semantic to clients.
	CertExpiresAt *string `json:"cert_expires_at"`
}

// statsView mirrors store.Stats in snake_case JSON. The duration percentiles
// are estimates derived from the histogram buckets (store.DurationBuckets), not
// interpolated values; see the store package for the exact semantics.
type statsView struct {
	Up            int    `json:"up"`
	Down          int    `json:"down"`
	Timeout       int    `json:"timeout"`
	Error         int    `json:"error"`
	Skipped       int    `json:"skipped"`
	AvgDurationMs int64  `json:"avg_duration_ms"`
	P50DurationMs *int64 `json:"p50_duration_ms"`
	P95DurationMs *int64 `json:"p95_duration_ms"`
}

// healthResponse is the body of GET /healthz.
type healthResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	// UptimeSeconds is the process uptime in seconds. It carries millisecond
	// resolution (three decimals) so the value visibly grows between two rapid
	// probes rather than staying pinned at a whole second.
	UptimeSeconds float64 `json:"uptime_seconds"`
	// LastWriteAgeSeconds is how long ago the newest check was recorded, or null
	// when the database holds no results at all.
	LastWriteAgeSeconds *float64 `json:"last_write_age_seconds"`
}

// errorResponse is the body of every error reply.
type errorResponse struct {
	Error string `json:"error"`
}

// apiServer holds the read-only dependencies of the handlers. It is a plain
// struct rather than a global so tests can build it without binding a port.
type apiServer struct {
	targets   []config.Target
	byName    map[string]struct{}
	store     *store.Store
	version   string
	startedAt time.Time
	logger    *slog.Logger
}

// New returns the HTTP handler for the read-only JSON API.
//
// version is reported verbatim by /healthz. startedAt is captured here so the
// reported process uptime is measured from handler construction.
//
// A nil logger falls back to one that discards output, so a caller cannot panic
// on a missing dependency. The returned handler is safe for concurrent use.
func New(cfg config.Config, st *store.Store, version string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &apiServer{
		targets:   cfg.Targets,
		byName:    make(map[string]struct{}, len(cfg.Targets)),
		store:     st,
		version:   version,
		startedAt: time.Now(),
		logger:    logger,
	}
	for _, t := range cfg.Targets {
		s.byName[t.Name] = struct{}{}
	}

	mux := http.NewServeMux()
	// The method-qualified patterns take precedence for GET; the method-less
	// patterns match the remaining methods and answer with a JSON 405. Unknown
	// paths fall through to the mux default 404.
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("/api/status", s.handleMethodNotAllowed)
	mux.HandleFunc("GET /api/history/{name}", s.handleHistory)
	mux.HandleFunc("/api/history/{name}", s.handleMethodNotAllowed)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("/healthz", s.handleMethodNotAllowed)

	return mux
}

// handleStatus reports the configuration-ordered state of every target. One
// batched snapshot (store.StatusSnapshot) supplies every target in a fixed
// number of queries (D40); the whole payload is built before anything is
// written, so a store failure still produces a clean 500 instead of a
// truncated 200.
func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.logger.Error("api: status requested without a store")
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	names := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		names = append(names, t.Name)
	}
	snap, err := s.store.StatusSnapshot(names)
	if err != nil {
		s.logger.Error("api: status snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	resp := statusResponse{
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Targets:   make([]targetStatus, 0, len(s.targets)),
	}
	for _, t := range s.targets {
		sn := snap[t.Name]
		ts := targetStatus{Name: t.Name, URL: t.URL, Recent: make([]string, 0, len(sn.Recent))}
		if sn.HasLatest {
			view := newResultView(sn.Latest)
			ts.Last = &view
		}
		for _, outcome := range sn.Recent {
			ts.Recent = append(ts.Recent, string(outcome))
		}
		ts.Stats = statsView{
			Up:            sn.Stats.Up,
			Down:          sn.Stats.Down,
			Timeout:       sn.Stats.Timeout,
			Error:         sn.Stats.Error,
			Skipped:       sn.Stats.Skipped,
			AvgDurationMs: sn.Stats.AvgDurationMs,
			P50DurationMs: sn.Stats.P50DurationMs,
			P95DurationMs: sn.Stats.P95DurationMs,
		}
		ts.Uptime24h = sn.Uptime24h
		ts.Uptime7d = sn.Uptime7d
		resp.Targets = append(resp.Targets, ts)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleHealthz reports process liveness plus the build version and how fresh
// the stored data is. A failing store is reported as 500: the endpoint answers
// "is this process healthy", and a process whose database cannot be read is not.
func (s *apiServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	resp := healthResponse{
		OK:            true,
		Version:       s.version,
		UptimeSeconds: roundSeconds(time.Since(s.startedAt).Seconds()),
	}

	if s.store == nil {
		s.logger.Error("api: healthz requested without a store")
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	lastWrite, ok, err := s.store.LastWriteTime()
	if err != nil {
		s.logger.Error("api: healthz last write time", "err", err)
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}
	if ok {
		age := roundSeconds(time.Since(lastWrite).Seconds())
		resp.LastWriteAgeSeconds = &age
	}

	writeJSON(w, http.StatusOK, resp)
}

// roundSeconds rounds a duration in seconds to millisecond resolution, which is
// plenty for a health probe and keeps the JSON compact.
func roundSeconds(seconds float64) float64 {
	return math.Round(seconds*1000) / 1000
}

// handleHistory returns the newest results of one configured target as a JSON
// array. Unknown names are a 404 because config, not the store, owns the target
// list.
func (s *apiServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.byName[name]; !ok {
		writeError(w, http.StatusNotFound, "target not found")
		return
	}
	if s.store == nil {
		s.logger.Error("api: history requested without a store", "target", name)
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	limit := historyLimit(r.URL.Query().Get("limit"))
	results, err := s.store.History(name, limit)
	if err != nil {
		s.logger.Error("api: history", "target", name, "err", err)
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	out := make([]resultView, 0, len(results))
	for _, res := range results {
		out = append(out, newResultView(res))
	}

	writeJSON(w, http.StatusOK, out)
}

// handleMethodNotAllowed answers any non-GET method on a known path.
func (s *apiServer) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// historyLimit resolves the limit query parameter: absent, non-numeric, or
// non-positive values fall back to the default — except an out-of-range
// integer, which plainly meant "as many as possible" and is capped. Values
// above the maximum are capped as well.
func historyLimit(raw string) int {
	if raw == "" {
		return defaultHistoryLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return maxHistoryLimit
		}
		return defaultHistoryLimit
	}
	if n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}

// newResultView converts a stored result into its JSON representation.
func newResultView(r checker.Result) resultView {
	v := resultView{
		Outcome:    string(r.Outcome),
		StatusCode: r.StatusCode,
		DurationMs: r.Duration.Milliseconds(),
		At:         r.StartedAt.UTC().Format(time.RFC3339),
		Error:      r.Err,
	}
	if !r.CertExpiresAt.IsZero() {
		cert := r.CertExpiresAt.UTC().Format(time.RFC3339)
		v.CertExpiresAt = &cert
	}
	return v
}

// writeJSON writes v as compact JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding our own plain structs cannot fail, so a write error here is at
	// worst a disconnected client; there is nothing useful left to send.
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error body.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
