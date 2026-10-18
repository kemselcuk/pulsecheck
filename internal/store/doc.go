// Package store persists check results in a local SQLite database.
//
// Every check is recorded with its outcome (up, down, timeout, error, or
// skipped), the HTTP status code, the measured duration, and — for HTTPS
// targets — the expiry of the leaf TLS certificate. A background cleanup task
// drops rows older than the configured 24 hour retention window.
//
// The schema is migrated additively on Open: a new database is created with
// every current column, while a database created by an older release has any
// missing columns added by an idempotent, guarded ALTER TABLE, so reopening an
// old database never loses data.
package store
