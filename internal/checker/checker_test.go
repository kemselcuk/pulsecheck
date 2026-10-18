package checker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// TestCheckRichRequest proves the configured method and headers reach the
// server, and that a matching expected_status is classified as up.
func TestCheckRichRequest(t *testing.T) {
	type observed struct {
		method  string
		headers http.Header
	}
	seen := make(chan observed, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- observed{method: r.Method, headers: r.Header.Clone()}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	target := config.Target{
		Name:           "rich",
		URL:            srv.URL,
		Method:         "POST",
		Headers:        map[string]string{"Accept": "application/json", "X-Trace": "abc"},
		ExpectedStatus: http.StatusOK,
	}
	res := Check(context.Background(), target, client)
	if res.Outcome != OutcomeUp {
		t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
	}

	select {
	case got := <-seen:
		if got.method != "POST" {
			t.Errorf("server saw method %q, want %q", got.method, "POST")
		}
		if v := got.headers.Get("Accept"); v != "application/json" {
			t.Errorf("server saw Accept %q, want %q", v, "application/json")
		}
		if v := got.headers.Get("X-Trace"); v != "abc" {
			t.Errorf("server saw X-Trace %q, want %q", v, "abc")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the request")
	}
}

// TestCheckEmptyMethodDefaultsToGet pins the fail-safe for targets built in
// code without config defaults: an empty Method still sends a GET.
func TestCheckEmptyMethodDefaultsToGet(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	res := Check(context.Background(), config.Target{Name: "empty", URL: srv.URL}, client)
	if res.Outcome != OutcomeUp {
		t.Fatalf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
	}
	select {
	case got := <-seen:
		if got != http.MethodGet {
			t.Errorf("server saw method %q, want %q", got, http.MethodGet)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the request")
	}
}

// TestCheckExpectedStatus covers the richer classification rule: a configured
// expected_status of 500 makes a 500 response up, while leaving it unset keeps
// the 2xx rule and classifies the same response as down.
func TestCheckExpectedStatus(t *testing.T) {
	srv := httptest.NewServer(statusHandler(http.StatusInternalServerError))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	t.Run("configured 500 with a 500 response is up", func(t *testing.T) {
		res := Check(context.Background(), config.Target{
			Name: "expected", URL: srv.URL, ExpectedStatus: http.StatusInternalServerError,
		}, client)
		if res.Outcome != OutcomeUp {
			t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
		}
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusInternalServerError)
		}
	})

	t.Run("unset with a 500 response is down", func(t *testing.T) {
		res := Check(context.Background(), config.Target{Name: "unset", URL: srv.URL}, client)
		if res.Outcome != OutcomeDown {
			t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeDown)
		}
		if res.StatusCode != http.StatusInternalServerError {
			t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusInternalServerError)
		}
	})

	t.Run("configured 200 with a 500 response is down", func(t *testing.T) {
		res := Check(context.Background(), config.Target{
			Name: "wrong", URL: srv.URL, ExpectedStatus: http.StatusOK,
		}, client)
		if res.Outcome != OutcomeDown {
			t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeDown)
		}
	})
}

// TestCheckExpectedStatusWithRedirects pins the combined case the review noted
// as piecewise-covered only: expected_status is judged on the FINAL response
// of the chain, and with redirects disabled it is judged on the raw redirect
// response itself.
func TestCheckExpectedStatusWithRedirects(t *testing.T) {
	final := httptest.NewServer(okHandler())
	t.Cleanup(final.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/landing", http.StatusMovedPermanently)
	}))
	t.Cleanup(redirect.Close)

	client := redirect.Client()
	t.Cleanup(client.CloseIdleConnections)

	// The followRedirects decision lives in the scheduler's client choice, so
	// the "not following" case below passes the same twin the scheduler would
	// (withoutRedirects) rather than expecting Check to know about the field.
	noRedirect := withoutRedirects(client)

	// Following: the chain ends in a 200, so expected_status=200 is up.
	follow := config.Target{
		Name: "chain", URL: redirect.URL, ExpectedStatus: http.StatusOK, FollowRedirects: true,
	}
	if res := Check(context.Background(), follow, client); res.Outcome != OutcomeUp {
		t.Errorf("followed chain with expected 200: Outcome = %q (status %d), want %q", res.Outcome, res.StatusCode, OutcomeUp)
	}

	// Following but expecting the redirect code: the final 200 mismatches -> down.
	wrong := config.Target{
		Name: "chain-wrong", URL: redirect.URL, ExpectedStatus: http.StatusMovedPermanently, FollowRedirects: true,
	}
	if res := Check(context.Background(), wrong, client); res.Outcome != OutcomeDown {
		t.Errorf("followed chain expecting 301: Outcome = %q (status %d), want %q", res.Outcome, res.StatusCode, OutcomeDown)
	}

	// NOT following: the raw 301 is the final response, so expected_status=301 is up
	// and the landing page was never requested (the twin returns the first response).
	noFollow := config.Target{
		Name: "raw", URL: redirect.URL, ExpectedStatus: http.StatusMovedPermanently,
	}
	if res := Check(context.Background(), noFollow, noRedirect); res.Outcome != OutcomeUp || res.StatusCode != http.StatusMovedPermanently {
		t.Errorf("raw 301 expecting 301: Outcome = %q (status %d), want up/301", res.Outcome, res.StatusCode)
	}
}

// TestCheckBodyContains covers the substring assertion: a matching body stays
// up, while a missing needle turns an accepted status into down with a
// describing error, keeping the status code visible.
func TestCheckBodyContains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "all systems operational")
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	up := Check(context.Background(), config.Target{
		Name: "match", URL: srv.URL, ExpectBodyContains: "operational",
	}, client)
	if up.Outcome != OutcomeUp {
		t.Errorf("matching body: Outcome = %q (err=%q), want %q", up.Outcome, up.Err, OutcomeUp)
	}

	down := Check(context.Background(), config.Target{
		Name: "mismatch", URL: srv.URL, ExpectBodyContains: "welcome",
	}, client)
	if down.Outcome != OutcomeDown {
		t.Errorf("missing needle: Outcome = %q (err=%q), want %q", down.Outcome, down.Err, OutcomeDown)
	}
	if down.StatusCode != http.StatusOK {
		t.Errorf("missing needle: StatusCode = %d, want %d", down.StatusCode, http.StatusOK)
	}
	if !strings.Contains(down.Err, `body does not contain "welcome"`) {
		t.Errorf("missing needle: Err = %q, want the describing message", down.Err)
	}
}

// TestCheckBodyRegex covers the regexp form and its error wording.
func TestCheckBodyRegex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "build 2026.10.19 ready")
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	up := Check(context.Background(), config.Target{
		Name: "match", URL: srv.URL, ExpectBodyRegex: regexp.MustCompile(`build \d{4}\.\d{2}\.\d{2}`),
	}, client)
	if up.Outcome != OutcomeUp {
		t.Errorf("matching regexp: Outcome = %q (err=%q), want %q", up.Outcome, up.Err, OutcomeUp)
	}

	down := Check(context.Background(), config.Target{
		Name: "mismatch", URL: srv.URL, ExpectBodyRegex: regexp.MustCompile(`^ready`),
	}, client)
	if down.Outcome != OutcomeDown {
		t.Errorf("non-matching regexp: Outcome = %q (err=%q), want %q", down.Outcome, down.Err, OutcomeDown)
	}
	if !strings.Contains(down.Err, `body does not match regexp "^ready"`) {
		t.Errorf("non-matching regexp: Err = %q, want the describing message", down.Err)
	}
}

// TestCheckBodyTruncation pins the 64 KiB read cap: a needle inside the cap is
// found, the same needle beyond it is invisible and counts as a mismatch — the
// cap is part of the documented contract (D38).
func TestCheckBodyTruncation(t *testing.T) {
	prefix := strings.Repeat("a", maxBodyRead)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, prefix+"NEEDLE")
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	inside := Check(context.Background(), config.Target{
		Name: "inside", URL: srv.URL, ExpectBodyContains: strings.Repeat("a", 10),
	}, client)
	if inside.Outcome != OutcomeUp {
		t.Errorf("needle inside the cap: Outcome = %q (err=%q), want %q", inside.Outcome, inside.Err, OutcomeUp)
	}

	beyond := Check(context.Background(), config.Target{
		Name: "beyond", URL: srv.URL, ExpectBodyContains: "NEEDLE",
	}, client)
	if beyond.Outcome != OutcomeDown {
		t.Errorf("needle beyond the cap: Outcome = %q, want %q", beyond.Outcome, OutcomeDown)
	}
}

// TestCheckBodyReadError pins mid-body failures: a response whose body ends
// before its declared Content-Length is an error, not a silent pass.
func TestCheckBodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	res := Check(context.Background(), config.Target{
		Name: "truncated", URL: srv.URL, ExpectBodyContains: "short",
	}, client)
	if res.Outcome != OutcomeError {
		t.Errorf("truncated body: Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeError)
	}
	if res.Err == "" {
		t.Error("truncated body: Err is empty, want the read error")
	}
}

// TestCheckBodyReadTimeout pins the interaction with the request deadline: a
// server that stalls after the headers turns the body read past the deadline
// into a timeout, not a generic error.
func TestCheckBodyReadTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers out; the body is withheld
		<-r.Context().Done()     // block until the client gives up
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	res := Check(ctx, config.Target{Name: "stall", URL: srv.URL, ExpectBodyContains: "x"}, client)
	if res.Outcome != OutcomeTimeout {
		t.Errorf("stalled body: Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeTimeout)
	}
}

// TestCheckTLSCertificate proves certificate tracking: a TLS response records
// the leaf certificate's expiry, while a plain HTTP response records nothing.
// Status and duration behave exactly as before in both cases.
func TestCheckTLSCertificate(t *testing.T) {
	t.Run("https records a future expiry", func(t *testing.T) {
		srv := httptest.NewTLSServer(okHandler())
		t.Cleanup(srv.Close)

		client := srv.Client()
		t.Cleanup(client.CloseIdleConnections)

		res := Check(context.Background(), config.Target{Name: "tls", URL: srv.URL}, client)
		if res.Outcome != OutcomeUp {
			t.Fatalf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
		}
		if res.Duration <= 0 {
			t.Errorf("Duration = %v, want > 0", res.Duration)
		}
		if res.CertExpiresAt.IsZero() {
			t.Fatal("CertExpiresAt is zero, want the TLS leaf certificate expiry")
		}
		if !res.CertExpiresAt.After(time.Now()) {
			t.Errorf("CertExpiresAt = %v, want a time in the future", res.CertExpiresAt)
		}
	})

	t.Run("plain http records no expiry", func(t *testing.T) {
		srv := httptest.NewServer(okHandler())
		t.Cleanup(srv.Close)

		client := srv.Client()
		t.Cleanup(client.CloseIdleConnections)

		res := Check(context.Background(), config.Target{Name: "plain", URL: srv.URL}, client)
		if res.Outcome != OutcomeUp {
			t.Fatalf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("StatusCode = %d, want %d", res.StatusCode, http.StatusOK)
		}
		if res.Duration <= 0 {
			t.Errorf("Duration = %v, want > 0", res.Duration)
		}
		if !res.CertExpiresAt.IsZero() {
			t.Errorf("CertExpiresAt = %v, want the zero time for plain HTTP", res.CertExpiresAt)
		}
	})
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
