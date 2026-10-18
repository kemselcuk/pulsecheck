// Package config loads and validates the PulseCheck JSON configuration file.
//
// A configuration file has the following shape:
//
//	{
//	  "server":   { "addr": ":8080" },
//	  "storage":  { "db_path": "pulsecheck.db", "retention": "24h" },
//	  "defaults": { "interval": "30s", "timeout": "5s", "workers": 4 },
//	  "targets": [
//	    { "name": "example", "url": "https://example.com", "interval": "10s", "timeout": "3s" }
//	  ]
//	}
//
// Durations are strings parsed with time.ParseDuration. Any omitted field falls
// back to its default: the listen address ":8080", the database path
// "pulsecheck.db", a 24h retention window, a 30s interval, a 5s timeout, and 4
// workers. A target may override the default interval and timeout; after
// Load every target carries concrete values and no field is left unset.
//
// Parsing rejects malformed JSON, unknown fields, targets without a name or
// with a duplicate name, URLs that are not absolute http(s) URLs, and
// explicitly provided interval, timeout, retention, or workers values that are
// not strictly positive. Empty or whitespace-only string values (server.addr,
// storage.db_path, target names aside) are treated as omitted and fall back to
// their defaults; only target names must be non-empty.
//
// Parse reads a configuration from an io.Reader and Load reads one from a file
// on disk. Both return fully validated, defaulted values.
package config
