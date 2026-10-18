// Package web implements the status screen described in doc.go.

package web

import (
	_ "embed"
	"net/http"
)

// contentHTML is the media type of the embedded page.
const contentHTML = "text/html; charset=utf-8"

// indexHTML is the single-page status screen, inlined into the binary so the
// binary is self-contained and the page needs no external assets.
//
//go:embed index.html
var indexHTML []byte

// New returns the HTTP handler that serves the status screen.
//
// Only GET / is served; every other path keeps the mux default 404. The handler
// is stateless and safe for concurrent use.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveIndex)
	return mux
}

// serveIndex writes the embedded page.
func serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", contentHTML)
	_, _ = w.Write(indexHTML)
}
