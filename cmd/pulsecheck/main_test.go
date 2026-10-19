package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	srv := httptest.NewServer(newHandler(config.NewAtomic(cfg), st, "wired-version", slog.New(slog.NewTextHandler(io.Discard, nil))))
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
		{http.MethodGet, "/debug/pprof/", http.StatusNotFound, ""}, // opt-in only (D43)
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
		_ = resp.Body.Close()

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

	srv := httptest.NewServer(newHandler(config.NewAtomic(&config.Config{}), st, "1.1.0-test", nil))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

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

// TestVersionOutput pins the -version output format and, by reading the same
// package-level var, the -ldflags "-X main.version=..." injection point.
func TestVersionOutput(t *testing.T) {
	old := version
	version = "v9.9.9-test"
	t.Cleanup(func() { version = old })

	if got, want := versionOutput(), "pulsecheck v9.9.9-test"; got != want {
		t.Errorf("versionOutput() = %q, want %q", got, want)
	}
}

// fakeReloader records the configurations asked of it.
type fakeReloader struct {
	applied []config.Config
}

// Reload implements the reloader seam for tests.
func (f *fakeReloader) Reload(cfg config.Config) {
	f.applied = append(f.applied, cfg)
}

// TestReloadConfig covers the SIGHUP policy (D41): a changed target list is
// applied to the holder and the scheduler, a parse failure or a
// restart-required change is rejected with the holder untouched, and an
// unchanged target list is a logged no-op.
func TestReloadConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	base := `{
		"server":   {"addr": ":8080"},
		"storage":  {"db_path": "reload.db"},
		"defaults": {"interval": "30s", "timeout": "5s", "workers": 2},
		"targets":  [{"name": "a", "url": "https://example.com"}]
	}`
	withTargetB := `{
		"server":   {"addr": ":8080"},
		"storage":  {"db_path": "reload.db"},
		"defaults": {"interval": "30s", "timeout": "5s", "workers": 2},
		"targets":  [
			{"name": "a", "url": "https://example.com"},
			{"name": "b", "url": "https://example.org", "interval": "10s"}
		]
	}`

	writeConfig := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}

	loadBase := func(t *testing.T) (*config.Atomic, *fakeReloader) {
		t.Helper()
		cfg, err := config.Load(writeConfig(t, base))
		if err != nil {
			t.Fatalf("load base config: %v", err)
		}
		return config.NewAtomic(cfg), &fakeReloader{}
	}

	t.Run("changed target list is applied", func(t *testing.T) {
		live, fake := loadBase(t)
		path := writeConfig(t, withTargetB)

		if !reloadConfig(path, live, fake, logger) {
			t.Fatal("reloadConfig = false, want the change applied")
		}
		if got := len(live.Load().Targets); got != 2 {
			t.Errorf("live targets = %d, want 2", got)
		}
		if len(fake.applied) != 1 || len(fake.applied[0].Targets) != 2 {
			t.Errorf("scheduler got %+v, want one reload with 2 targets", fake.applied)
		}
	})

	t.Run("parse failure keeps the running configuration", func(t *testing.T) {
		live, fake := loadBase(t)
		path := writeConfig(t, `{"targets": [`)

		if reloadConfig(path, live, fake, logger) {
			t.Fatal("reloadConfig = true, want the parse failure rejected")
		}
		if got := len(live.Load().Targets); got != 1 {
			t.Errorf("live targets = %d, want the original 1", got)
		}
		if len(fake.applied) != 0 {
			t.Errorf("scheduler got %+v, want nothing", fake.applied)
		}
	})

	t.Run("restart-required changes are rejected", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"server.addr", strings.Replace(base, `":8080"`, `":9999"`, 1)},
			{"storage.db_path", strings.Replace(base, `"reload.db"`, `"other.db"`, 1)},
			{"storage.retention", strings.Replace(base, `{"db_path": "reload.db"}`, `{"db_path": "reload.db", "retention": "6h"}`, 1)},
			{"defaults.workers", strings.Replace(base, `"workers": 2`, `"workers": 8`, 1)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				live, fake := loadBase(t)
				path := writeConfig(t, tc.body)

				if reloadConfig(path, live, fake, logger) {
					t.Fatalf("reloadConfig = true, want the %s change rejected", tc.name)
				}
				cfg := live.Load()
				if cfg.Server.Addr != ":8080" || cfg.Storage.DBPath != "reload.db" || cfg.Defaults.Workers != 2 {
					t.Errorf("live config changed after a rejected reload: %+v", cfg)
				}
				if len(fake.applied) != 0 {
					t.Errorf("scheduler got %+v, want nothing", fake.applied)
				}
			})
		}
	})

	t.Run("unchanged targets are a no-op", func(t *testing.T) {
		live, fake := loadBase(t)
		path := writeConfig(t, base)

		if reloadConfig(path, live, fake, logger) {
			t.Fatal("reloadConfig = true, want an unchanged list to be a no-op")
		}
		if len(fake.applied) != 0 {
			t.Errorf("scheduler got %+v, want nothing", fake.applied)
		}
	})
}

// TestDiffTargets pins the change summary that feeds the reload log.
func TestDiffTargets(t *testing.T) {
	tg := func(name, url string) config.Target {
		return config.Target{Name: name, URL: url, Interval: time.Minute, Timeout: time.Second}
	}
	old := []config.Target{tg("a", "https://a.example"), tg("b", "https://b.example"), tg("c", "https://c.example")}
	next := []config.Target{tg("a", "https://a.example"), tg("b", "https://changed.example"), tg("d", "https://d.example")}

	added, removed, updated := diffTargets(old, next)
	if len(added) != 1 || added[0] != "d" {
		t.Errorf("added = %v, want [d]", added)
	}
	if len(removed) != 1 || removed[0] != "c" {
		t.Errorf("removed = %v, want [c]", removed)
	}
	if len(updated) != 1 || updated[0] != "b" {
		t.Errorf("updated = %v, want [b]", updated)
	}
}

// TestPprofMux pins the dedicated mux of the opt-in pprof server (D43): the
// standard debug endpoints respond, and nothing else does — the application
// mux is never extended by it (pinned by the route-table 404 case).
func TestPprofMux(t *testing.T) {
	srv := httptest.NewServer(pprofMux())
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/symbol",
		"/debug/pprof/profile?seconds=1",
		"/debug/pprof/trace?seconds=1",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}

	// The dedicated mux is not an application server: unknown paths 404.
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/status on the pprof mux status = %d, want 404", resp.StatusCode)
	}
}
