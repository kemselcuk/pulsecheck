// Package web serves the single-page status screen of PulseCheck.
//
// The HTML, CSS, and JavaScript assets are compiled into the binary via
// embed.FS, and the page polls /api/status with dependency-free vanilla
// JavaScript so the UI works without network access. index.html is the single
// page: its style and script are inline, and it references no external
// resources at all, so the screen renders fully offline.
package web
