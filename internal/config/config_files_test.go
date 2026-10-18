package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoFile walks up from the test's working directory to locate a file checked
// into the repository root.
func repoFile(t *testing.T, name string) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	for {
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find %s in any directory above %s", name, dir)
		}
		dir = parent
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(repoFile(t, "config.example.json"))
	if err != nil {
		t.Fatalf("Load(config.example.json) error = %v", err)
	}

	if len(cfg.Targets) < 2 {
		t.Fatalf("len(Targets) = %d, want at least 2 example targets", len(cfg.Targets))
	}
	if cfg.Server.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want the default %q", cfg.Server.Addr, DefaultAddr)
	}
	if cfg.Storage.DBPath != DefaultDBPath {
		t.Errorf("DBPath = %q, want the default %q", cfg.Storage.DBPath, DefaultDBPath)
	}
	if cfg.Storage.Retention != DefaultRetention {
		t.Errorf("Retention = %v, want the default %v", cfg.Storage.Retention, DefaultRetention)
	}
	if cfg.Defaults.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want the default %d", cfg.Defaults.Workers, DefaultWorkers)
	}

	// Every target must have concrete values, and at least one must override.
	overridden := false
	for _, target := range cfg.Targets {
		if target.Interval <= 0 || target.Timeout <= 0 {
			t.Errorf("target %q has unresolved values: %+v", target.Name, target)
		}
		if target.Interval != cfg.Defaults.Interval || target.Timeout != cfg.Defaults.Timeout {
			overridden = true
		}
	}
	if !overridden {
		t.Error("config.example.json should demonstrate at least one per-target override")
	}
}

func TestDemoConfigLoads(t *testing.T) {
	cfg, err := Load(repoFile(t, "config.demo.json"))
	if err != nil {
		t.Fatalf("Load(config.demo.json) error = %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.Storage.DBPath != "data/demo.db" {
		t.Errorf("DBPath = %q, want %q", cfg.Storage.DBPath, "data/demo.db")
	}
	if cfg.Storage.Retention != DefaultRetention {
		t.Errorf("Retention = %v, want the default %v", cfg.Storage.Retention, DefaultRetention)
	}
	if cfg.Defaults.Interval != 10*time.Second {
		t.Errorf("Defaults.Interval = %v, want 10s", cfg.Defaults.Interval)
	}
	if cfg.Defaults.Timeout != 5*time.Second {
		t.Errorf("Defaults.Timeout = %v, want the default 5s", cfg.Defaults.Timeout)
	}
	if cfg.Defaults.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want the default %d", cfg.Defaults.Workers, DefaultWorkers)
	}

	wantPaths := []string{"/ok", "/broken", "/slow"}
	// demo-slow deliberately overrides its interval below the default timeout
	// so live demo runs visibly produce 'skipped' records (decision D4).
	wantIntervals := []time.Duration{10 * time.Second, 10 * time.Second, 3 * time.Second}
	// The port is pinned so a revert to a commonly-colliding port (decision
	// D23: 8081 is taken by unrelated dev tooling on real machines) cannot
	// slip through the tests.
	wantPrefix := "http://localhost:18081"
	if len(cfg.Targets) != len(wantPaths) {
		t.Fatalf("len(Targets) = %d, want %d demo targets", len(cfg.Targets), len(wantPaths))
	}
	for i, wantPath := range wantPaths {
		target := cfg.Targets[i]
		if target.Interval != wantIntervals[i] {
			t.Errorf("targets[%d].Interval = %v, want %v", i, target.Interval, wantIntervals[i])
		}
		if !strings.HasPrefix(target.URL, wantPrefix) {
			t.Errorf("targets[%d].URL = %q, want it to start with %q", i, target.URL, wantPrefix)
		}
		if target.Timeout != 5*time.Second {
			t.Errorf("targets[%d].Timeout = %v, want the default 5s", i, target.Timeout)
		}
		if got := target.URL; len(got) < len(wantPath) || got[len(got)-len(wantPath):] != wantPath {
			t.Errorf("targets[%d].URL = %q, want it to end with %q", i, got, wantPath)
		}
	}
}

// TestDockerConfigLoads pins the compose configuration: config.docker.json must
// mirror config.demo.json except that every target points at the compose
// network's service name (demo-targets) instead of localhost, so the container
// resolves its targets over the compose network rather than its own loopback.
func TestDockerConfigLoads(t *testing.T) {
	cfg, err := Load(repoFile(t, "config.docker.json"))
	if err != nil {
		t.Fatalf("Load(config.docker.json) error = %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Server.Addr, ":8080")
	}
	if cfg.Storage.DBPath != "data/demo.db" {
		t.Errorf("DBPath = %q, want %q", cfg.Storage.DBPath, "data/demo.db")
	}
	if cfg.Storage.Retention != DefaultRetention {
		t.Errorf("Retention = %v, want the default %v", cfg.Storage.Retention, DefaultRetention)
	}
	if cfg.Defaults.Interval != 10*time.Second {
		t.Errorf("Defaults.Interval = %v, want 10s", cfg.Defaults.Interval)
	}
	if cfg.Defaults.Timeout != 5*time.Second {
		t.Errorf("Defaults.Timeout = %v, want the default 5s", cfg.Defaults.Timeout)
	}
	if cfg.Defaults.Workers != DefaultWorkers {
		t.Errorf("Workers = %d, want the default %d", cfg.Defaults.Workers, DefaultWorkers)
	}

	wantPaths := []string{"/ok", "/broken", "/slow"}
	wantIntervals := []time.Duration{10 * time.Second, 10 * time.Second, 3 * time.Second}
	// The compose service name is what the container resolves; a revert to
	// localhost here would make every target fail inside the compose network.
	wantPrefix := "http://demo-targets:18081"
	if len(cfg.Targets) != len(wantPaths) {
		t.Fatalf("len(Targets) = %d, want %d demo targets", len(cfg.Targets), len(wantPaths))
	}
	for i, wantPath := range wantPaths {
		target := cfg.Targets[i]
		if target.Interval != wantIntervals[i] {
			t.Errorf("targets[%d].Interval = %v, want %v", i, target.Interval, wantIntervals[i])
		}
		if !strings.HasPrefix(target.URL, wantPrefix) {
			t.Errorf("targets[%d].URL = %q, want it to start with %q", i, target.URL, wantPrefix)
		}
		if target.Timeout != 5*time.Second {
			t.Errorf("targets[%d].Timeout = %v, want the default 5s", i, target.Timeout)
		}
		if got := target.URL; len(got) < len(wantPath) || got[len(got)-len(wantPath):] != wantPath {
			t.Errorf("targets[%d].URL = %q, want it to end with %q", i, got, wantPath)
		}
	}
}
