// Package api exposes the read-only JSON API of PulseCheck.
//
// Handlers are built on the standard library net/http package, using the Go
// 1.22 pattern routing of http.ServeMux, and serve
// GET /api/status for the current state of every target,
// GET /api/history/{name} for recent results of a single target, and
// GET /healthz for process liveness, the build version, and how fresh the stored
// data is.
//
// /api/status carries, per target, the newest result, the recent outcome strip,
// the 24h statistics, and uptime percentages for the last 24 hours and the last
// 7 days. An uptime figure is null when its window holds no completed check;
// skipped rounds are excluded from the percentage, so an interval in which no
// check ran is never scored as an outage.
//
// The API never persists anything and never mutates the store. The configured
// target list is the source of truth: it fixes the order of /api/status and
// decides whether a name in /api/history/{name} exists, because the store is
// keyed by target name and cannot know about targets that have not reported yet.
//
// Every response, including errors, is compact JSON. Internal failures are
// logged in full but reported to the client as a generic
// {"error":"internal error"} so store details never leak over the wire.
package api
