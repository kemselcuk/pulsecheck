// Package api exposes the read-only JSON API of PulseCheck.
//
// Handlers are built on the standard library net/http package and serve
// GET /api/status for the current state of every target and
// GET /api/history/{name} for recent results of a single target.
package api
