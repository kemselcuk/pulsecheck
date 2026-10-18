// Package api implements the read-only JSON API described in doc.go.

package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"pulsecheck/internal/checker"
	"pulsecheck/internal/config"
	"pulsecheck/internal/store"
)

const (
	// recentLimit is how many of the newest outcomes /api/status reports per
	// target in its "recent" strip.
	recentLimit = 30

	// defaultHistoryLimit is the result count of /api/history when the limit
	// query parameter is absent or not a positive integer.
	defaultHistoryLimit = 50

	// maxHistoryLimit caps the result count of /api/history so a single request
	// cannot pull an unbounded slice out of the database.
	maxHistoryLimit = 500

	// statsWindow is the look-back period of the per-target statistics.
	statsWindow = 24 * time.Hour

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
}

// statsView mirrors store.Stats in snake_case JSON.
type statsView struct {
	Up            int   `json:"up"`
	Down          int   `json:"down"`
	Timeout       int   `json:"timeout"`
	Error         int   `json:"error"`
	Skipped       int   `json:"skipped"`
	AvgDurationMs int64 `json:"avg_duration_ms"`
}

// errorResponse is the body of every error reply.
type errorResponse struct {
	Error string `json:"error"`
}

// apiServer holds the read-only dependencies of the handlers. It is a plain
// struct rather than a global so tests can build it without binding a port.
type apiServer struct {
	targets []config.Target
	byName  map[string]struct{}
	store   *store.Store
	logger  *slog.Logger
}

// New returns the HTTP handler for the read-only JSON API.
//
// A nil logger falls back to one that discards output, so a caller cannot panic
// on a missing dependency. The returned handler is safe for concurrent use.
func New(cfg config.Config, st *store.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &apiServer{
		targets: cfg.Targets,
		byName:  make(map[string]struct{}, len(cfg.Targets)),
		store:   st,
		logger:  logger,
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

	return mux
}

// handleStatus reports the configuration-ordered state of every target.
func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.logger.Error("api: status requested without a store")
		writeError(w, http.StatusInternalServerError, genericErrorMessage)
		return
	}

	resp := statusResponse{
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Targets:   make([]targetStatus, 0, len(s.targets)),
	}
	// Build the whole payload before writing anything so a mid-loop store
	// failure can still produce a clean 500 instead of a truncated 200.
	for _, t := range s.targets {
		ts, err := s.targetStatus(t)
		if err != nil {
			s.logger.Error("api: build status", "target", t.Name, "err", err)
			writeError(w, http.StatusInternalServerError, genericErrorMessage)
			return
		}
		resp.Targets = append(resp.Targets, ts)
	}

	writeJSON(w, http.StatusOK, resp)
}

// targetStatus assembles one target's entry. A target without rows yields a
// null "last", an empty "recent", and zero statistics.
func (s *apiServer) targetStatus(t config.Target) (targetStatus, error) {
	ts := targetStatus{Name: t.Name, URL: t.URL, Recent: []string{}}

	last, ok, err := s.store.Latest(t.Name)
	if err != nil {
		return targetStatus{}, err
	}
	if ok {
		view := newResultView(last)
		ts.Last = &view
	}

	history, err := s.store.History(t.Name, recentLimit)
	if err != nil {
		return targetStatus{}, err
	}
	for _, res := range history {
		ts.Recent = append(ts.Recent, string(res.Outcome))
	}

	stats, err := s.store.Stats(t.Name, statsWindow)
	if err != nil {
		return targetStatus{}, err
	}
	ts.Stats = statsView{
		Up:            stats.Up,
		Down:          stats.Down,
		Timeout:       stats.Timeout,
		Error:         stats.Error,
		Skipped:       stats.Skipped,
		AvgDurationMs: stats.AvgDurationMs,
	}

	return ts, nil
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
	return resultView{
		Outcome:    string(r.Outcome),
		StatusCode: r.StatusCode,
		DurationMs: r.Duration.Milliseconds(),
		At:         r.StartedAt.UTC().Format(time.RFC3339),
		Error:      r.Err,
	}
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
