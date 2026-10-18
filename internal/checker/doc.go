// Package checker runs periodic HTTP checks against the configured targets.
//
// Each check sends the target's configured method and headers, and classifies
// the final response either against an explicit expected_status or against the
// default 2xx rule. Whether redirects are followed is chosen by the scheduler
// per target (it holds a redirecting client and a non-redirecting twin), so
// Check itself stays policy-agnostic. For HTTPS responses the leaf certificate
// expiry is captured on the result.
//
// Each request runs with its own context timeout, and work is dispatched to a
// bounded worker pool so the overall concurrency stays limited. A per-target
// in-flight lock records a "skipped" result when the previous check for the
// same target is still running. The scheduler supports graceful shutdown.
package checker
