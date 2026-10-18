// Package metrics exposes the Prometheus-compatible /metrics endpoint.
//
// The exposition body follows the CLASSIC Prometheus text format (version
// 0.0.4) rather than OpenMetrics. For the gauge-only series served here the two
// are nearly identical, but OpenMetrics requires a trailing "# EOF" line and a
// Content-Type that opts into it; the classic format has neither, so the body is
// served as text/plain; charset=utf-8 and deliberately ends without "# EOF".
// Prometheus accepts both, and the classic contract is the smaller and more
// widely understood one.
//
// Every series answers from live store queries at scrape time. At PulseCheck's
// scale — a handful of targets and a bounded retention window — that keeps the
// endpoint trivially consistent with the database and avoids a second source of
// truth; an in-process registry updated by the recorder is the natural
// evolution if the target count ever grows enough to make per-scrape queries
// expensive.
package metrics
