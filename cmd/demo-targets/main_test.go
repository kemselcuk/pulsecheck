package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const slowDelay = 100 * time.Millisecond

// get performs a GET against base+path with the given context.
func get(ctx context.Context, t *testing.T, client *http.Client, base, path string) (*http.Response, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext(%s) error = %v", path, err)
	}

	return client.Do(req)
}

func TestNewMuxRoutes(t *testing.T) {
	srv := httptest.NewServer(newMux(slowDelay))
	t.Cleanup(srv.Close)
	client := srv.Client()

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/ok", wantStatus: http.StatusOK, wantBody: "ok"},
		{path: "/broken", wantStatus: http.StatusInternalServerError, wantBody: "broken"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := get(context.Background(), t, client, srv.URL, tt.path)
			if err != nil {
				t.Fatalf("GET %s error = %v", tt.path, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading %s body: %v", tt.path, err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("GET %s status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			}
			if got := strings.TrimSpace(string(body)); got != tt.wantBody {
				t.Errorf("GET %s body = %q, want %q", tt.path, got, tt.wantBody)
			}
		})
	}
}

func TestIndexRoute(t *testing.T) {
	srv := httptest.NewServer(newMux(slowDelay))
	t.Cleanup(srv.Close)

	resp, err := get(context.Background(), t, srv.Client(), srv.URL, "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading / body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	for _, route := range []string{"/ok", "/broken", "/slow"} {
		if !strings.Contains(string(body), route) {
			t.Errorf("GET / body = %q, want it to mention %s", body, route)
		}
	}

	missing, err := get(context.Background(), t, srv.Client(), srv.URL, "/nope")
	if err != nil {
		t.Fatalf("GET /nope error = %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want %d", missing.StatusCode, http.StatusNotFound)
	}
}

func TestSlowWaitsThenSucceeds(t *testing.T) {
	srv := httptest.NewServer(newMux(slowDelay))
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := get(context.Background(), t, srv.Client(), srv.URL, "/slow")
	if err != nil {
		t.Fatalf("GET /slow error = %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /slow body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /slow status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := strings.TrimSpace(string(body)); got != "slow" {
		t.Errorf("GET /slow body = %q, want %q", got, "slow")
	}
	if elapsed < slowDelay/2 {
		t.Errorf("GET /slow returned after %v, want at least ~%v", elapsed, slowDelay)
	}
	if elapsed > 2*time.Second {
		t.Errorf("GET /slow returned after %v, want it to finish promptly after ~%v", elapsed, slowDelay)
	}
}

func TestSlowHonorsContextCancellation(t *testing.T) {
	// The delay is far longer than the test's client timeout, so a prompt
	// return can only come from the handler observing r.Context().
	srv := httptest.NewServer(newMux(30 * time.Second))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	resp, err := get(ctx, t, srv.Client(), srv.URL, "/slow")
	elapsed := time.Since(start)
	cancel()
	<-done
	if resp != nil {
		resp.Body.Close()
	}

	if err == nil {
		t.Fatal("GET /slow error = nil, want a context cancellation error")
	}
	if elapsed >= time.Second {
		t.Errorf("GET /slow returned after %v, want < 1s after cancellation", elapsed)
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("GET /slow error = %q, want it to mention context cancellation", err)
	}
}

func TestLogRequests(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	handler := logRequests(logger, newMux(0))

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	logged := buf.String()
	for _, want := range []string{"method=GET", "path=/ok", "status=200", "duration="} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output %q, want it to contain %q", logged, want)
		}
	}
}
