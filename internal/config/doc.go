// Package config loads and validates the PulseCheck JSON configuration file.
//
// It reads the list of HTTP targets, each with an optional per-target interval
// and timeout, and applies the defaults of 30s interval, 5s timeout, and 4
// workers when a value is omitted. It also exposes the storage settings
// (db_path and a 24h retention window) and the server listen address.
package config
