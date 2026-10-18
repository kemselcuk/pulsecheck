package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// checkFunc asserts details of a successfully parsed configuration.
type checkFunc func(t *testing.T, cfg *Config)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // substring that must appear in the error, empty means success
		check   checkFunc
	}{
		{
			name: "valid complete config",
			input: `{
				"server": {"addr": "127.0.0.1:9000"},
				"storage": {"db_path": "/tmp/x.db", "retention": "6h"},
				"defaults": {"interval": "45s", "timeout": "7s", "workers": 8},
				"targets": [
					{"name": "a", "url": "https://example.com", "interval": "10s", "timeout": "3s"},
					{"name": "b", "url": "http://localhost:8081/ok"}
				]
			}`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Server.Addr != "127.0.0.1:9000" {
					t.Errorf("Addr = %q, want %q", cfg.Server.Addr, "127.0.0.1:9000")
				}
				if cfg.Storage.DBPath != "/tmp/x.db" {
					t.Errorf("DBPath = %q, want %q", cfg.Storage.DBPath, "/tmp/x.db")
				}
				if cfg.Storage.Retention != 6*time.Hour {
					t.Errorf("Retention = %v, want %v", cfg.Storage.Retention, 6*time.Hour)
				}
				if cfg.Defaults.Interval != 45*time.Second {
					t.Errorf("Defaults.Interval = %v, want %v", cfg.Defaults.Interval, 45*time.Second)
				}
				if cfg.Defaults.Timeout != 7*time.Second {
					t.Errorf("Defaults.Timeout = %v, want %v", cfg.Defaults.Timeout, 7*time.Second)
				}
				if cfg.Defaults.Workers != 8 {
					t.Errorf("Defaults.Workers = %d, want 8", cfg.Defaults.Workers)
				}
				if len(cfg.Targets) != 2 {
					t.Fatalf("len(Targets) = %d, want 2", len(cfg.Targets))
				}
				if got := cfg.Targets[0]; got.Name != "a" || got.URL != "https://example.com" ||
					got.Interval != 10*time.Second || got.Timeout != 3*time.Second {
					t.Errorf("Targets[0] = %+v, want a/https://example.com/10s/3s", got)
				}
				if got := cfg.Targets[1]; got.Name != "b" || got.URL != "http://localhost:8081/ok" ||
					got.Interval != 45*time.Second || got.Timeout != 7*time.Second {
					t.Errorf("Targets[1] = %+v, want b/http://localhost:8081/ok/45s/7s", got)
				}
			},
		},
		{
			name:  "defaults applied when omitted",
			input: `{"targets": [{"name": "only", "url": "https://example.com"}]}`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Server.Addr != DefaultAddr {
					t.Errorf("Addr = %q, want %q", cfg.Server.Addr, DefaultAddr)
				}
				if cfg.Storage.DBPath != DefaultDBPath {
					t.Errorf("DBPath = %q, want %q", cfg.Storage.DBPath, DefaultDBPath)
				}
				if cfg.Storage.Retention != DefaultRetention {
					t.Errorf("Retention = %v, want %v", cfg.Storage.Retention, DefaultRetention)
				}
				if cfg.Defaults.Interval != DefaultInterval {
					t.Errorf("Defaults.Interval = %v, want %v", cfg.Defaults.Interval, DefaultInterval)
				}
				if cfg.Defaults.Timeout != DefaultTimeout {
					t.Errorf("Defaults.Timeout = %v, want %v", cfg.Defaults.Timeout, DefaultTimeout)
				}
				if cfg.Defaults.Workers != DefaultWorkers {
					t.Errorf("Defaults.Workers = %d, want %d", cfg.Defaults.Workers, DefaultWorkers)
				}
				if len(cfg.Targets) != 1 {
					t.Fatalf("len(Targets) = %d, want 1", len(cfg.Targets))
				}
				got := cfg.Targets[0]
				if got.Interval != 30*time.Second {
					t.Errorf("Targets[0].Interval = %v, want 30s", got.Interval)
				}
				if got.Timeout != 5*time.Second {
					t.Errorf("Targets[0].Timeout = %v, want 5s", got.Timeout)
				}
			},
		},
		{
			name:  "empty object yields all defaults and no targets",
			input: `{}`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Server.Addr != DefaultAddr ||
					cfg.Storage.DBPath != DefaultDBPath ||
					cfg.Storage.Retention != DefaultRetention ||
					cfg.Defaults.Interval != DefaultInterval ||
					cfg.Defaults.Timeout != DefaultTimeout ||
					cfg.Defaults.Workers != DefaultWorkers {
					t.Errorf("cfg = %+v, want all defaults", cfg)
				}
				if len(cfg.Targets) != 0 {
					t.Errorf("len(Targets) = %d, want 0", len(cfg.Targets))
				}
			},
		},
		{
			name: "per-target override wins",
			input: `{
				"defaults": {"interval": "30s", "timeout": "5s"},
				"targets": [
					{"name": "override", "url": "https://example.com", "interval": "10s", "timeout": "2s"},
					{"name": "inherits", "url": "https://example.com/x"}
				]
			}`,
			check: func(t *testing.T, cfg *Config) {
				if got := cfg.Targets[0]; got.Interval != 10*time.Second || got.Timeout != 2*time.Second {
					t.Errorf("Targets[0] = %+v, want interval 10s timeout 2s", got)
				}
				if got := cfg.Targets[1]; got.Interval != 30*time.Second || got.Timeout != 5*time.Second {
					t.Errorf("Targets[1] = %+v, want interval 30s timeout 5s", got)
				}
			},
		},
		{
			name: "partial override leaves the other value inherited",
			input: `{
				"defaults": {"interval": "30s", "timeout": "5s"},
				"targets": [{"name": "t", "url": "https://example.com", "interval": "1m"}]
			}`,
			check: func(t *testing.T, cfg *Config) {
				got := cfg.Targets[0]
				if got.Interval != time.Minute {
					t.Errorf("Interval = %v, want 1m", got.Interval)
				}
				if got.Timeout != 5*time.Second {
					t.Errorf("Timeout = %v, want 5s", got.Timeout)
				}
			},
		},
		{
			name:  "url with port and query is accepted",
			input: `{"targets": [{"name": "q", "url": "https://example.com:8443/health?deep=1"}]}`,
			check: func(t *testing.T, cfg *Config) {
				if got := cfg.Targets[0].URL; got != "https://example.com:8443/health?deep=1" {
					t.Errorf("URL = %q, want it preserved", got)
				}
			},
		},
		{
			name:    "malformed JSON",
			input:   `{"targets": [`,
			wantErr: "invalid JSON",
		},
		{
			name:    "top-level JSON is not an object",
			input:   `["nope"]`,
			wantErr: "invalid JSON",
		},
		{
			name:    "empty input",
			input:   ``,
			wantErr: "invalid JSON",
		},
		{
			name:    "trailing data after object",
			input:   `{} {}`,
			wantErr: "unexpected data",
		},
		{
			name:    "unknown top-level field",
			input:   `{"nope": true}`,
			wantErr: "unknown field",
		},
		{
			name:    "unknown nested field",
			input:   `{"server": {"addr": ":8080", "tls": true}}`,
			wantErr: "unknown field",
		},
		{
			name:    "unknown target field",
			input:   `{"targets": [{"name": "a", "url": "https://example.com", "expected_status": 200}]}`,
			wantErr: "unknown field",
		},
		{
			name:    "missing name",
			input:   `{"targets": [{"url": "https://example.com"}]}`,
			wantErr: "targets[0].name is required",
		},
		{
			name:    "empty name",
			input:   `{"targets": [{"name": "", "url": "https://example.com"}]}`,
			wantErr: "targets[0].name is required",
		},
		{
			name:    "whitespace-only name",
			input:   `{"targets": [{"name": "   ", "url": "https://example.com"}]}`,
			wantErr: "targets[0].name is required",
		},
		{
			name: "duplicate name",
			input: `{"targets": [
				{"name": "dup", "url": "https://example.com"},
				{"name": "dup", "url": "https://example.org"}
			]}`,
			wantErr: "duplicated",
		},
		{
			name: "duplicate name after trimming",
			input: `{"targets": [
				{"name": "dup", "url": "https://example.com"},
				{"name": " dup ", "url": "https://example.org"}
			]}`,
			wantErr: "duplicated",
		},
		{
			name:    "missing url",
			input:   `{"targets": [{"name": "a"}]}`,
			wantErr: "targets[0].url is required",
		},
		{
			name:    "relative url",
			input:   `{"targets": [{"name": "a", "url": "/health"}]}`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "url without scheme",
			input:   `{"targets": [{"name": "a", "url": "example.com"}]}`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "non-http scheme",
			input:   `{"targets": [{"name": "a", "url": "ftp://example.com"}]}`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "unparseable url",
			input:   `{"targets": [{"name": "a", "url": "http://exa mple.com"}]}`,
			wantErr: "targets[0].url",
		},
		{
			name:    "explicit zero workers",
			input:   `{"defaults": {"workers": 0}}`,
			wantErr: "defaults.workers must be greater than zero",
		},
		{
			name:    "explicit negative workers",
			input:   `{"defaults": {"workers": -1}}`,
			wantErr: "defaults.workers must be greater than zero",
		},
		{
			name:    "explicit zero timeout",
			input:   `{"defaults": {"timeout": "0s"}}`,
			wantErr: "defaults.timeout must be greater than zero",
		},
		{
			name:    "explicit negative timeout",
			input:   `{"defaults": {"timeout": "-1s"}}`,
			wantErr: "defaults.timeout must be greater than zero",
		},
		{
			name:    "explicit zero interval",
			input:   `{"defaults": {"interval": "0"}}`,
			wantErr: "defaults.interval must be greater than zero",
		},
		{
			name:    "explicit zero retention",
			input:   `{"storage": {"retention": "0s"}}`,
			wantErr: "storage.retention must be greater than zero",
		},
		{
			name:    "invalid duration string",
			input:   `{"defaults": {"interval": "soon"}}`,
			wantErr: "defaults.interval: invalid duration",
		},
		{
			name:    "target zero interval",
			input:   `{"targets": [{"name": "a", "url": "https://example.com", "interval": "0s"}]}`,
			wantErr: "targets[0].interval must be greater than zero",
		},
		{
			name:    "target negative timeout",
			input:   `{"targets": [{"name": "a", "url": "https://example.com", "timeout": "-3s"}]}`,
			wantErr: "targets[0].timeout must be greater than zero",
		},
		{
			name:    "target invalid duration",
			input:   `{"targets": [{"name": "a", "url": "https://example.com", "timeout": "2 seconds"}]}`,
			wantErr: "targets[0].timeout: invalid duration",
		},
		{
			name:  "names and urls are trimmed",
			input: `{"targets": [{"name": "  spaced  ", "url": "  https://example.com  "}]}`,
			check: func(t *testing.T, cfg *Config) {
				if got := cfg.Targets[0].Name; got != "spaced" {
					t.Errorf("Name = %q, want %q", got, "spaced")
				}
				if got := cfg.Targets[0].URL; got != "https://example.com" {
					t.Errorf("URL = %q, want %q", got, "https://example.com")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse() = %+v, want error containing %q", cfg, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if cfg == nil {
				t.Fatal("Parse() = nil, want non-nil config")
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	path := t.TempDir() + "/pulsecheck.json"
	if err := os.WriteFile(path, []byte(`{"targets": [{"name": "a", "url": "https://example.com"}]}`), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Defaults.Interval != DefaultInterval || cfg.Defaults.Timeout != DefaultTimeout {
		t.Errorf("defaults = %+v, want %v/%v", cfg.Defaults, DefaultInterval, DefaultTimeout)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Interval != DefaultInterval {
		t.Errorf("Targets = %+v, want one target with the default interval", cfg.Targets)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(t.TempDir() + "/does-not-exist.json")
	if err == nil {
		t.Fatal("Load() error = nil, want an error for a missing file")
	}
}
