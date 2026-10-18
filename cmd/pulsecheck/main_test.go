package main

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

// TestNewHandlerMountsEveryRoute is the wiring test: it proves that the real
// route table serves the dashboard, the JSON API, the liveness probe, the
// metrics endpoint, and the target detail pages, each from its top-level path.
func TestNewHandlerMountsEveryRoute(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/wiring.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		Targets: []config.Target{
			{Name: "alpha", URL: "https://alpha.example/health", Interval: time.Minute, Timeout: 5 * time.Second},
		},
	}
	// One stored result so the detail page renders its sparkline too.
	if err := st.RecordResult(checker.Result{
		TargetName: "alpha",
		StartedAt:  time.Now().Add(-time.Minute),
		Duration:   5 * time.Millisecond,
		Outcome:    checker.OutcomeUp,
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	srv := httptest.NewServer(newHandler(cfg, st, "wired-version", slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	cases := []struct {
		method string
		path   string
		want   int
		body   string // substring that must appear in the body
	}{
		{http.MethodGet, "/", http.StatusOK, "PulseCheck"},
		{http.MethodGet, "/api/status", http.StatusOK, `"targets"`},
		{http.MethodGet, "/healthz", http.StatusOK, `"ok":true`},
		{http.MethodGet, "/metrics", http.StatusOK, "pulsecheck_info{version=\"wired-version\"} 1"},
		{http.MethodGet, "/targets/alpha", http.StatusOK, "<polyline"},
		{http.MethodGet, "/targets/nope", http.StatusNotFound, ""},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed, `"method not allowed"`},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed, `"method not allowed"`},
	}

	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", tc.method, tc.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != tc.want {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
		if tc.body != "" && !strings.Contains(string(body), tc.body) {
			t.Errorf("%s %s body missing %q: %s", tc.method, tc.path, tc.body, body)
		}
	}
}

// TestNewHandlerHealthzVersion proves the version flows from the wiring into the
// liveness payload as JSON.
func TestNewHandlerHealthzVersion(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/wiring-version.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := httptest.NewServer(newHandler(&config.Config{}, st, "1.1.0-test", nil))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	var got struct {
		OK      bool    `json:"ok"`
		Version string  `json:"version"`
		Uptime  float64 `json:"uptime_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if !got.OK || got.Version != "1.1.0-test" {
		t.Errorf("/healthz = %+v, want ok with version 1.1.0-test", got)
	}
}
