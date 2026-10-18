// Package store persists check results in a local SQLite database.
//
// Every check is recorded with its outcome (up, down, timeout, error, or
// skipped), the HTTP status code, and the measured duration. A background
// cleanup task drops rows older than the configured 24 hour retention window.
package store
