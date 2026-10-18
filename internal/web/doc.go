// Package web serves the status screen and the per-target detail pages of
// PulseCheck.
//
// The HTML, CSS, and JavaScript assets are compiled into the binary via embed,
// and the dashboard polls /api/status with dependency-free vanilla JavaScript so
// the UI works without network access. index.html is the dashboard: its style
// and script are inline, it references no external resources, and each card
// links to that target's detail page.
//
// detail.html is the per-target page served at GET /targets/{name}. It is
// rendered on the server with html/template — no JavaScript at all — and shows
// the latest outcome, uptime for 24 hours and 7 days, the certificate expiry
// when one is known, and an inline SVG sparkline of the most recent check
// durations, oldest to newest. A target with no stored results renders the card
// without a chart. Both pages are fully self-contained, so the UI renders
// offline.
package web
