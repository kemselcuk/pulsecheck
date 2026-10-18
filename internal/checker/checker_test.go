package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pulsecheck/internal/config"
)

func TestOutcomeConstants(t *testing.T) {
	// The wire/storage values are part of the public contract.
	cases := map[Outcome]string{
		OutcomeUp:      "up",
		OutcomeDown:    "down",
		OutcomeTimeout: "timeout",
		OutcomeError:   "error",
		OutcomeSkipped: "skipped",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("Outcome = %q, want %q", got, want)
		}
	}
}

func TestCheckClassification(t *testing.T) {
	redirectMux := http.NewServeMux()
	redirectMux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	redirectMux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name            string
		path            string
		handler         http.Handler
		timeout         time.Duration // 0 means a plain cancellable context
		cancelBefore    bool
		closeServer     bool
		wantOutcome     Outcome
		wantStatus      int
		wantErrContains string
	}{
		{
			name:        "200 is up",
			handler:     okHandler(),
			wantOutcome: OutcomeUp,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "500 is down",
			handler:     statusHandler(http.StatusInternalServerError),
			wantOutcome: OutcomeDown,
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:        "404 is down",
			handler:     statusHandler(http.StatusNotFound),
			wantOutcome: OutcomeDown,
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "redirect chain ending in 200 is up",
			path:        "/start",
			handler:     redirectMux,
			wantOutcome: OutcomeUp,
			wantStatus:  http.StatusOK,
		},
		{
			// 500ms handler vs 100ms deadline: a wide 400ms margin, far above
			// the "no sub-50ms timing margins" rule.
			name:            "deadline exceeded is timeout",
			handler:         sleepHandler(500 * time.Millisecond),
			timeout:         100 * time.Millisecond,
			wantOutcome:     OutcomeTimeout,
			wantStatus:      0,
			wantErrContains: "deadline",
		},
		{
			name:        "closed port is error",
			closeServer: true,
			wantOutcome: OutcomeError,
			wantStatus:  0,
		},
		{
			name:            "cancelled context is error",
			handler:         okHandler(),
			cancelBefore:    true,
			wantOutcome:     OutcomeError,
			wantStatus:      0,
			wantErrContains: "context canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := tt.handler
			if handler == nil {
				handler = http.NotFoundHandler()
			}
			srv := httptest.NewServer(handler)
			if tt.closeServer {
				srv.Close() // the port is now free -> connection refused
			} else {
				t.Cleanup(srv.Close)
			}
			url := srv.URL + tt.path

			client := srv.Client()
			t.Cleanup(client.CloseIdleConnections)

			ctx, cancel := context.WithCancel(context.Background())
			if tt.timeout > 0 {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), tt.timeout)
			}
			defer cancel()
			if tt.cancelBefore {
				cancel()
			}

			target := config.Target{
				Name:     tt.name,
				URL:      url,
				Interval: time.Second,
				Timeout:  5 * time.Second,
			}

			res := Check(ctx, target, client)

			if res.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, tt.wantOutcome)
			}
			if res.StatusCode != tt.wantStatus {
				t.Errorf("StatusCode = %d, want %d", res.StatusCode, tt.wantStatus)
			}
			if tt.wantErrContains != "" && !strings.Contains(res.Err, tt.wantErrContains) {
				t.Errorf("Err = %q, want it to contain %q", res.Err, tt.wantErrContains)
			}
			if res.Outcome == OutcomeError && res.Err == "" {
				t.Error("Err = empty, want a reason for an error outcome")
			}
			if res.TargetName != tt.name {
				t.Errorf("TargetName = %q, want %q", res.TargetName, tt.name)
			}
			if res.URL != url {
				t.Errorf("URL = %q, want %q", res.URL, url)
			}
			if res.StartedAt.IsZero() {
				t.Error("StartedAt is zero, want the check start time")
			}
			if res.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", res.Duration)
			}
		})
	}
}

// TestCheckNilClientTolerated documents that Check fails safe instead of
// panicking when no client is supplied.
func TestCheckNilClientTolerated(t *testing.T) {
	srv := httptest.NewServer(okHandler())
	t.Cleanup(srv.Close)

	res := Check(context.Background(), config.Target{Name: "x", URL: srv.URL}, nil)
	if res.Outcome != OutcomeUp {
		t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	})
}

// sleepHandler waits for d but returns early if the client goes away, so no
// handler goroutine outlives a timed-out check.
func sleepHandler(d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	})
}
