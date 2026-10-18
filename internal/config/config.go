package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Defaults applied when a configuration field is omitted.
const (
	DefaultAddr            = ":8080"
	DefaultDBPath          = "pulsecheck.db"
	DefaultRetention       = 24 * time.Hour
	DefaultInterval        = 30 * time.Second
	DefaultTimeout         = 5 * time.Second
	DefaultWorkers         = 4
	DefaultMethod          = "GET"
	DefaultFollowRedirects = true
)

// ExpectedStatusMin and ExpectedStatusMax bound an explicitly configured
// expected_status. The whole IANA HTTP status code range is allowed, not just
// the codes PulseCheck itself ever returns.
const (
	ExpectedStatusMin = 100
	ExpectedStatusMax = 599
)

// methodPattern is the accepted shape of an HTTP method after normalization.
// The configuration is a static file, so an unusual but valid token such as
// "M-SEARCH" is rejected rather than guessed at; ^[A-Z]+$ keeps the vocabulary
// predictable and still covers every common method (GET, POST, HEAD, ...).
var methodPattern = regexp.MustCompile(`^[A-Z]+$`)

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

// Target is a single monitored endpoint with concrete request, interval, and
// timeout values.
//
// The request shape mirrors the richer HTTP check definition:
//   - Method is the HTTP method, normalized to uppercase (default "GET").
//   - ExpectedStatus, when non-zero, is the only status code that counts as up;
//     when zero the 2xx rule applies (any 2xx is up).
//   - Headers are sent with every request; a nil map means no extra headers.
//   - FollowRedirects controls whether the HTTP redirect chain is followed
//     (default true).
//   - ExpectBodyContains, when non-empty, additionally requires the response
//     body to contain this substring for the target to be up.
//   - ExpectBodyRegex, when non-nil (compiled once at Parse time), requires the
//     response body to match it. At most one of the two body assertions can be
//     configured; both are rejected together with Method "HEAD" (a HEAD
//     response has no body).
type Target struct {
	Name               string
	URL                string
	Method             string
	ExpectedStatus     int
	Headers            map[string]string
	FollowRedirects    bool
	ExpectBodyContains string
	ExpectBodyRegex    *regexp.Regexp
	Interval           time.Duration
	Timeout            time.Duration
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
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Method             string            `json:"method"`
	ExpectedStatus     *int              `json:"expected_status"`
	Headers            map[string]string `json:"headers"`
	FollowRedirects    *bool             `json:"follow_redirects"`
	ExpectBodyContains *string           `json:"expect_body_contains"`
	ExpectBodyRegex    *string           `json:"expect_body_regex"`
	Interval           string            `json:"interval"`
	Timeout            string            `json:"timeout"`
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

		method, err := parseMethod(fmt.Sprintf("targets[%d].method", i), rt.Method)
		if err != nil {
			return nil, err
		}
		expectedStatus, err := parseExpectedStatus(fmt.Sprintf("targets[%d].expected_status", i), rt.ExpectedStatus)
		if err != nil {
			return nil, err
		}
		headers, err := parseHeaders(fmt.Sprintf("targets[%d].headers", i), rt.Headers)
		if err != nil {
			return nil, err
		}
		bodyContains, bodyRegex, err := parseBodyAssertions(fmt.Sprintf("targets[%d]", i), method, rt.ExpectBodyContains, rt.ExpectBodyRegex)
		if err != nil {
			return nil, err
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
			Name:               name,
			URL:                targetURL,
			Method:             method,
			ExpectedStatus:     expectedStatus,
			Headers:            headers,
			FollowRedirects:    parseFollowRedirects(rt.FollowRedirects),
			ExpectBodyContains: bodyContains,
			ExpectBodyRegex:    bodyRegex,
			Interval:           targetInterval,
			Timeout:            targetTimeout,
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
	// A read-only file's close error is not actionable, so it is an explicit
	// drop.
	defer func() { _ = f.Close() }()

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

// parseMethod normalizes an optional HTTP method: an empty (or
// whitespace-only) value falls back to DefaultMethod, and anything else is
// trimmed, upper-cased, and validated against methodPattern. Lower-case input
// is therefore accepted and normalized to its upper-case form.
func parseMethod(field, value string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if normalized == "" {
		return DefaultMethod, nil
	}
	if !methodPattern.MatchString(normalized) {
		return "", fmt.Errorf("config: %s %q is invalid, must match ^[A-Z]+$ after normalization", field, normalized)
	}
	return normalized, nil
}

// parseExpectedStatus resolves the optional expected_status. A nil pointer
// means "not configured" and yields 0 (the classifier then applies the 2xx
// rule). Because the field is a pointer, an explicit 0 is distinguishable from
// an omitted value and is rejected by the 100..599 range check instead of
// silently disabling the rule.
func parseExpectedStatus(field string, value *int) (int, error) {
	if value == nil {
		return 0, nil
	}
	if *value < ExpectedStatusMin || *value > ExpectedStatusMax {
		return 0, fmt.Errorf("config: %s must be between %d and %d, got %d",
			field, ExpectedStatusMin, ExpectedStatusMax, *value)
	}
	return *value, nil
}

// parseHeaders copies the configured headers, rejecting empty (or
// whitespace-only) header names. A nil map stays nil so "no headers" remains
// distinguishable from an empty set; values are passed through unchanged
// because an empty header value is legal.
func parseHeaders(field string, raw map[string]string) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	headers := make(map[string]string, len(raw))
	for name, value := range raw {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, fmt.Errorf("config: %s contains an empty header name", field)
		}
		if _, dup := headers[trimmed]; dup {
			// Two distinct raw keys such as "a" and " a" trim to the same name;
			// letting a map pick the winner would make Parse non-deterministic
			// (decision D32). Reject the configuration instead.
			return nil, fmt.Errorf("config: %s contains duplicate header name %q after trimming", field, trimmed)
		}
		headers[trimmed] = value
	}
	return headers, nil
}

// parseBodyAssertions resolves the optional body assertions. At most one of
// expect_body_contains / expect_body_regex may be configured; both use pointer
// fields so an explicitly empty value is rejected instead of silently
// disabling the rule, and the regex is compiled here so every later stage can
// rely on a valid pattern (the trust-boundary discipline of D32). A body
// assertion combined with method HEAD is rejected too: a HEAD response has no
// body, so such a target could never be up.
func parseBodyAssertions(field, method string, rawContains, rawRegex *string) (string, *regexp.Regexp, error) {
	if rawContains != nil && rawRegex != nil {
		return "", nil, fmt.Errorf("config: %s: expect_body_contains and expect_body_regex are mutually exclusive, set only one", field)
	}
	if method == "HEAD" && (rawContains != nil || rawRegex != nil) {
		return "", nil, fmt.Errorf("config: %s: body assertions cannot be combined with method HEAD, whose responses have no body", field)
	}
	if rawContains != nil {
		if *rawContains == "" {
			return "", nil, fmt.Errorf("config: %s.expect_body_contains must not be empty", field)
		}
		return *rawContains, nil, nil
	}
	if rawRegex != nil {
		if *rawRegex == "" {
			return "", nil, fmt.Errorf("config: %s.expect_body_regex must not be empty", field)
		}
		re, err := regexp.Compile(*rawRegex)
		if err != nil {
			return "", nil, fmt.Errorf("config: %s.expect_body_regex %q is invalid: %w", field, *rawRegex, err)
		}
		return "", re, nil
	}
	return "", nil, nil
}

// parseFollowRedirects resolves the optional follow_redirects flag, defaulting
// to DefaultFollowRedirects when it is omitted.
func parseFollowRedirects(value *bool) bool {
	if value == nil {
		return DefaultFollowRedirects
	}
	return *value
}
