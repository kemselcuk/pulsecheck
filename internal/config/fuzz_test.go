package config

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzParse treats Parse as the trust boundary for untrusted input: the
// configuration file is read from disk (or, in future, from a control plane)
// and every later stage — the scheduler, the checker, the HTTP client — assumes
// the returned Config is already fully resolved and valid. A panic or an
// unvalidated value escaping Parse would therefore propagate into every
// target rather than failing loudly at the boundary, which is exactly what this
// fuzz target is here to rule out.
//
// The properties below are checked against Parse's ACTUAL contract, pinned from
// the implementation rather than assumed:
//
//  1. Parse never panics (implicit: the fuzz engine reports a panic as a
//     crasher, and every helper below is written to be panic-free on any input).
//  2. Parse returns exactly one of a config or an error. Every failure path in
//     Parse returns (nil, err) and the success path returns (cfg, nil), so a
//     non-nil config together with a non-nil error is a bug, as is (nil, nil).
//     Returning both would invite callers to act on an unvalidated config.
//  3. On success the resolved config satisfies every promise the rest of the
//     program makes about it: positive durations, uniquely-named targets,
//     absolute http(s) URLs, method tokens that match ^[A-Z]+$, expected_status
//     inside 100..599 (when set), at most one body assertion and never one
//     combined with HEAD, and positive workers and retention.
//  4. On failure the error is non-empty and names the "config" subsystem, so an
//     operator can tell a configuration problem apart from an I/O one.
func FuzzParse(f *testing.F) {
	// Seed corpus. It spans the interesting shapes: the empty object, a fully
	// populated config exercising every field (including the rich check fields
	// and a per-target interval/timeout override), the documented minimal
	// target, and one seed per rejection class (malformed JSON, an unknown
	// field, an invalid duration, and a quote/backslash-heavy name).
	f.Add(`{}`)
	f.Add(`{
		"server":   {"addr": "127.0.0.1:9090"},
		"storage":  {"db_path": "data/pulsecheck.db", "retention": "48h"},
		"defaults": {"interval": "30s", "timeout": "5s", "workers": 8},
		"targets": [
			{
				"name": "api",
				"url": "https://example.com/health",
				"method": "post",
				"expected_status": 201,
				"headers": {"Authorization": "Bearer token", "X-Trace": ""},
				"follow_redirects": false,
				"interval": "10s",
				"timeout": "2s"
			},
			{
				"name": "site",
				"url": "http://example.org",
				"method": "HEAD",
				"expected_status": 200,
				"headers": {"Accept": "text/html"},
				"follow_redirects": true
			}
		]
	}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","interval":"1s"}]}`)
	f.Add(`{"targets": [`)
	f.Add(`{"targets": [], "unknown_field": true}`)
	f.Add(`{"defaults":{"interval":"not-a-duration"}}`)
	f.Add(`{"targets":[{"name":"a\"b\\c\\\"d\\","url":"https://example.com"}]}`)
	// Rejection paths added after the D32b review: a trimmed-key collision and
	// a lowercase method that must normalize rather than error.
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","headers":{"X-Trace":"1"," X-Trace":"2"}}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","method":"post"}]}`)
	// Body assertions (D38): one seed per accepted form and one per rejection
	// class (both together, HEAD, an invalid pattern, and an empty value).
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","expect_body_contains":"operational"}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","expect_body_regex":"^ok\\s*$"}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","expect_body_contains":"a","expect_body_regex":"b"}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","method":"HEAD","expect_body_contains":"a"}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","expect_body_regex":"["}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","expect_body_contains":""}]}`)
	// TLS options (D42): an accepting seed without file access plus the
	// rejection classes that need no real file to exist.
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","tls":{"insecure_skip_verify":true}}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","tls":{"ca_file":""}}]}`)
	f.Add(`{"targets":[{"name":"a","url":"https://example.com","tls":{"ca_file":"/nonexistent/ca.pem"}}]}`)

	f.Fuzz(func(t *testing.T, data string) {
		cfg, err := Parse(strings.NewReader(data))

		// Property 2: exactly one of config / error.
		if cfg == nil && err == nil {
			t.Fatalf("Parse(%q) = (nil, nil), want either a config or an error", data)
		}
		if cfg != nil && err != nil {
			t.Fatalf("Parse(%q) = (%+v, %v), want config XOR error", data, cfg, err)
		}

		if err != nil {
			// Property 4: a failure is legible and attributable.
			if err.Error() == "" {
				t.Fatalf("Parse(%q) returned an empty error message", data)
			}
			if !strings.Contains(err.Error(), "config") {
				t.Fatalf("Parse(%q) error %q does not mention %q", data, err.Error(), "config")
			}
			return
		}

		// Property 3: the resolved config is fully valid.
		if cfg.Storage.Retention <= 0 {
			t.Fatalf("Parse(%q) retention = %v, want > 0", data, cfg.Storage.Retention)
		}
		if cfg.Defaults.Workers <= 0 {
			t.Fatalf("Parse(%q) workers = %d, want > 0", data, cfg.Defaults.Workers)
		}
		if cfg.Defaults.Interval <= 0 {
			t.Fatalf("Parse(%q) default interval = %v, want > 0", data, cfg.Defaults.Interval)
		}
		if cfg.Defaults.Timeout <= 0 {
			t.Fatalf("Parse(%q) default timeout = %v, want > 0", data, cfg.Defaults.Timeout)
		}

		seen := make(map[string]struct{}, len(cfg.Targets))
		for i, tg := range cfg.Targets {
			if tg.Name == "" {
				t.Fatalf("Parse(%q) targets[%d].name is empty", data, i)
			}
			if trimmed := strings.TrimSpace(tg.Name); trimmed != tg.Name {
				t.Fatalf("Parse(%q) targets[%d].name = %q, want it trimmed to %q", data, i, tg.Name, trimmed)
			}
			if _, dup := seen[tg.Name]; dup {
				t.Fatalf("Parse(%q) targets[%d].name = %q is duplicated", data, i, tg.Name)
			}
			seen[tg.Name] = struct{}{}

			// Mirror Parse's own URL rule: it accepts exactly the values that
			// url.Parse reads as an absolute http(s) URL with a host.
			u, perr := url.Parse(tg.URL)
			if perr != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				t.Fatalf("Parse(%q) targets[%d].url = %q is not an absolute http(s) URL (err=%v)", data, i, tg.URL, perr)
			}
			if tg.Interval <= 0 {
				t.Fatalf("Parse(%q) targets[%d].interval = %v, want > 0", data, i, tg.Interval)
			}
			if tg.Timeout <= 0 {
				t.Fatalf("Parse(%q) targets[%d].timeout = %v, want > 0", data, i, tg.Timeout)
			}
			// The zero value means "no expected status, use the 2xx rule";
			// anything else must be a plausible HTTP status code.
			if tg.ExpectedStatus != 0 &&
				(tg.ExpectedStatus < ExpectedStatusMin || tg.ExpectedStatus > ExpectedStatusMax) {
				t.Fatalf("Parse(%q) targets[%d].expected_status = %d, want 0 or %d..%d",
					data, i, tg.ExpectedStatus, ExpectedStatusMin, ExpectedStatusMax)
			}
			if !isUpperAlphaToken(tg.Method) {
				t.Fatalf("Parse(%q) targets[%d].method = %q, want a token matching ^[A-Z]+$", data, i, tg.Method)
			}
			// Body assertions: at most one form, and never paired with HEAD
			// (whose responses have no body to match).
			if tg.ExpectBodyContains != "" && tg.ExpectBodyRegex != nil {
				t.Fatalf("Parse(%q) targets[%d] carries both body assertions", data, i)
			}
			if tg.Method == "HEAD" && (tg.ExpectBodyContains != "" || tg.ExpectBodyRegex != nil) {
				t.Fatalf("Parse(%q) targets[%d] pairs HEAD with a body assertion", data, i)
			}
			// TLS options: a configured CA path always resolved into a pool.
			if tg.TLS.CAFile != "" && tg.TLS.CAPool == nil {
				t.Fatalf("Parse(%q) targets[%d]: CAFile is set without a pool", data, i)
			}
		}
	})
}

// isUpperAlphaToken is a small, self-contained re-assertion of the method
// grammar (^[A-Z]+$). It deliberately does not call the package's own
// methodPattern: a predicate that shares the implementation cannot catch a
// broken pattern. It is panic-free for any input, including invalid UTF-8,
// because it walks bytes.
func isUpperAlphaToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
