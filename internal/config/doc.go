// Package config loads and validates the PulseCheck JSON configuration file.
//
// A configuration file has the following shape:
//
//	{
//	  "server":   { "addr": ":8080" },
//	  "storage":  { "db_path": "pulsecheck.db", "retention": "24h" },
//	  "defaults": { "interval": "30s", "timeout": "5s", "workers": 4 },
//	  "targets": [
//	    {
//	      "name": "example", "url": "https://example.com",
//	      "method": "GET", "expected_status": 200,
//	      "headers": { "Accept": "application/json" },
//	      "follow_redirects": true,
//	      "interval": "10s", "timeout": "3s"
//	    }
//	  ]
//	}
//
// Durations are strings parsed with time.ParseDuration. Any omitted field falls
// back to its default: the listen address ":8080", the database path
// "pulsecheck.db", a 24h retention window, a 30s interval, a 5s timeout, and 4
// workers. A target may override the default interval and timeout; after
// Load every target carries concrete values and no field is left unset.
//
// Each target also describes the request to send. method defaults to "GET" and
// is normalized to upper case; expected_status, when set, is the only status
// code classified as up (unset means the 2xx rule applies); headers are sent
// verbatim on every request; follow_redirects defaults to true.
//
// Parsing rejects malformed JSON, unknown fields, targets without a name or
// with a duplicate name, URLs that are not absolute http(s) URLs, methods that
// are not ^[A-Z]+$ after normalization, expected_status values outside
// 100..599, empty header names, and explicitly provided interval, timeout,
// retention, or workers values that are not strictly positive. Empty or
// whitespace-only string values (server.addr, storage.db_path, target names
// aside) are treated as omitted and fall back to their defaults; only target
// names must be non-empty.
//
// Parse reads a configuration from an io.Reader and Load reads one from a file
// on disk. Both return fully validated, defaulted values.
package config
