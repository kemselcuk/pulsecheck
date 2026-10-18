package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// get fetches path from a fresh test server and returns the status, the
// Content-Type, and the body.
func get(t *testing.T, path string) (int, string, string) {
	t.Helper()

	srv := httptest.NewServer(New())
	defer srv.Close()

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

func TestIndexServed(t *testing.T) {
	status, contentType, body := get(t, "/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if contentType != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", contentType, "text/html; charset=utf-8")
	}
	for _, marker := range []string{"PulseCheck", "api/status"} {
		if !strings.Contains(body, marker) {
			t.Errorf("page does not contain %q", marker)
		}
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	for _, path := range []string{"/nope", "/index.html", "/api/status"} {
		status, _, _ := get(t, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}
}

// TestNoExternalResources pins the offline requirement: the page must not
// reference any resource served over the network.
func TestNoExternalResources(t *testing.T) {
	_, _, body := get(t, "/")

	for _, scheme := range []string{"http://", "https://"} {
		if strings.Contains(body, scheme) {
			t.Errorf("page contains %q; the screen must work fully offline", scheme)
		}
	}
	// The page is served as a single file, so referencing a src or href at all
	// would mean an external asset; assert there are none.
	if strings.Contains(body, "src=") || strings.Contains(body, "href=") {
		t.Errorf("page references an external attribute (src=/href=); it must be self-contained")
	}
}
