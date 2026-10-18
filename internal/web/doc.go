// Package web serves the single-page status screen of PulseCheck.
//
// The HTML, CSS, and JavaScript assets are compiled into the binary via
// embed.FS, and the page polls /api/status with dependency-free vanilla
// JavaScript so the UI works without network access.
package web
