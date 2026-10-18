// Package checker runs periodic HTTP checks against the configured targets.
//
// Each request runs with its own context timeout, and work is dispatched to a
// bounded worker pool so the overall concurrency stays limited. A per-target
// in-flight lock records a "skipped" result when the previous check for the
// same target is still running. The scheduler supports graceful shutdown.
package checker
