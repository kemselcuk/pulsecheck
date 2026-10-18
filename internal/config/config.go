package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// Defaults applied when a configuration field is omitted.
const (
	DefaultAddr      = ":8080"
	DefaultDBPath    = "pulsecheck.db"
	DefaultRetention = 24 * time.Hour
	DefaultInterval  = 30 * time.Second
	DefaultTimeout   = 5 * time.Second
	DefaultWorkers   = 4
)

// Config is a fully resolved and validated PulseCheck configuration.
type Config struct {
	Server   ServerConfig
	Storage  StorageConfig
	Defaults DefaultsConfig
	Targets  []Target
}

// ServerConfig holds the HTTP listener settings.
type ServerConfig struct {
	Addr string
}

// StorageConfig holds the storage settings.
type StorageConfig struct {
	DBPath    string
	Retention time.Duration
}

// DefaultsConfig holds the per-target defaults applied by target overrides.
type DefaultsConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	Workers  int
}

// Target is a single monitored endpoint with concrete interval and timeout.
type Target struct {
	Name     string
	URL      string
	Interval time.Duration
	Timeout  time.Duration
}

// rawConfig mirrors the on-disk JSON shape before defaults are applied.
type rawConfig struct {
	Server   rawServer   `json:"server"`
	Storage  rawStorage  `json:"storage"`
	Defaults rawDefaults `json:"defaults"`
	Targets  []rawTarget `json:"targets"`
}

type rawServer struct {
	Addr string `json:"addr"`
}

type rawStorage struct {
	DBPath    string `json:"db_path"`
	Retention string `json:"retention"`
}

type rawDefaults struct {
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
	Workers  *int   `json:"workers"`
}

type rawTarget struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
}

// Parse decodes, defaults, and validates a configuration from r.
func Parse(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: invalid JSON: %w", err)
	}
	// Reject trailing content after the first JSON value.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: unexpected data after the top-level JSON object")
	}

	cfg := &Config{
		Server: ServerConfig{Addr: strings.TrimSpace(raw.Server.Addr)},
		Storage: StorageConfig{
			DBPath: strings.TrimSpace(raw.Storage.DBPath),
		},
	}
	// Empty string values are treated as omitted and fall back to the
	// defaults (documented in the package comment).
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = DefaultAddr
	}
	if cfg.Storage.DBPath == "" {
		cfg.Storage.DBPath = DefaultDBPath
	}

	retention, err := parseDuration("storage.retention", raw.Storage.Retention, DefaultRetention)
	if err != nil {
		return nil, err
	}
	cfg.Storage.Retention = retention

	interval, err := parseDuration("defaults.interval", raw.Defaults.Interval, DefaultInterval)
	if err != nil {
		return nil, err
	}
	timeout, err := parseDuration("defaults.timeout", raw.Defaults.Timeout, DefaultTimeout)
	if err != nil {
		return nil, err
	}
	workers := DefaultWorkers
	if raw.Defaults.Workers != nil {
		if *raw.Defaults.Workers <= 0 {
			return nil, fmt.Errorf("config: defaults.workers must be greater than zero, got %d", *raw.Defaults.Workers)
		}
		workers = *raw.Defaults.Workers
	}
	cfg.Defaults = DefaultsConfig{Interval: interval, Timeout: timeout, Workers: workers}

	seen := make(map[string]struct{}, len(raw.Targets))
	for i, rt := range raw.Targets {
		name := strings.TrimSpace(rt.Name)
		if name == "" {
			return nil, fmt.Errorf("config: targets[%d].name is required", i)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("config: targets[%d].name %q is duplicated, target names must be unique", i, name)
		}
		seen[name] = struct{}{}

		targetURL := strings.TrimSpace(rt.URL)
		if targetURL == "" {
			return nil, fmt.Errorf("config: targets[%d].url is required", i)
		}
		u, err := url.Parse(targetURL)
		if err != nil {
			return nil, fmt.Errorf("config: targets[%d].url %q is not a valid URL: %w", i, rt.URL, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("config: targets[%d].url %q must be an absolute http or https URL", i, rt.URL)
		}

		targetInterval, err := parseDuration(fmt.Sprintf("targets[%d].interval", i), rt.Interval, interval)
		if err != nil {
			return nil, err
		}
		targetTimeout, err := parseDuration(fmt.Sprintf("targets[%d].timeout", i), rt.Timeout, timeout)
		if err != nil {
			return nil, err
		}

		cfg.Targets = append(cfg.Targets, Target{
			Name:     name,
			URL:      targetURL,
			Interval: targetInterval,
			Timeout:  targetTimeout,
		})
	}

	return cfg, nil
}

// Load reads and parses the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	return Parse(f)
}

// parseDuration parses an optional duration string, returning def when the
// value is omitted and an error naming field when it is invalid or not
// strictly positive.
func parseDuration(field, value string, def time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return def, nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("config: %s: invalid duration %q: %w", field, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be greater than zero, got %q", field, value)
	}

	return d, nil
}
