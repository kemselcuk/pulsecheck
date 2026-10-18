package checker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"pulsecheck/internal/config"
)

// Outcome is the classified result of a single check.
type Outcome string

const (
	// OutcomeUp means the final HTTP status (after redirects) was accepted: it
	// equalled the target's expected_status when one was configured, or fell in
	// the 2xx range otherwise.
	OutcomeUp Outcome = "up"
	// OutcomeDown means a response was received with a non-2xx status.
	OutcomeDown Outcome = "down"
	// OutcomeTimeout means the request did not finish before its deadline.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeError means any other failure (connection error, bad URL, ...).
	OutcomeError Outcome = "error"
	// OutcomeSkipped means no check ran because the target was busy.
	OutcomeSkipped Outcome = "skipped"
)

// Result describes what happened during one check of a target.
//
// Err carries the reason only for the error, timeout, and skipped outcomes;
// the down outcome is described by StatusCode instead.
type Result struct {
	TargetName string
	URL        string
	StartedAt  time.Time
	Duration   time.Duration
	Outcome    Outcome
	StatusCode int
	Err        string
	// CertExpiresAt is the NotAfter instant of the leaf certificate of the
	// final TLS connection, or the zero time when the check did not complete a
	// TLS handshake (plain HTTP, a transport error, or a timeout).
	CertExpiresAt time.Time
}

// Check performs a single request against t and classifies the outcome.
//
// The request uses t.Method (defaulting to GET when empty, so programmatically
// built targets behave like parsed ones) and carries every entry of t.Headers.
//
// Classification follows the target's richer definition: when
// t.ExpectedStatus > 0 the check is up if and only if the final response status
// equals it, otherwise the up status is down; when t.ExpectedStatus is zero the
// 2xx rule applies (any 2xx is up, anything else is down). Both rules judge the
// FINAL response after any redirects the client followed.
//
// The caller owns the deadline: ctx is passed to the request unchanged, so the
// scheduler can wrap it in a per-request context.WithTimeout while tests can
// supply their own. This is why Check never sets http.Client.Timeout — using a
// single timeout mechanism (the request context) keeps one source of truth,
// makes the behaviour testable with a plain context, and avoids confusing
// double-deadline interactions.
//
// Redirect policy is owned by the caller, not by Check: whether redirects are
// followed is a property of the supplied *http.Client (the scheduler picks the
// matching client for t.FollowRedirects). Check therefore needs no knowledge of
// t.FollowRedirects.
//
// Check never panics; unrecognised situations fall back to OutcomeError.
func Check(ctx context.Context, t config.Target, client *http.Client) (res Result) {
	res.TargetName = t.Name
	res.URL = t.URL
	res.StartedAt = time.Now()
	// Duration is measured from just before the request is sent until Check
	// returns, on every path, so it is always greater than zero.
	defer func() { res.Duration = time.Since(res.StartedAt) }()

	if client == nil {
		// Fail safe: a nil client would panic inside Do.
		client = http.DefaultClient
	}

	method := t.Method
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, t.URL, nil)
	if err != nil {
		res.Outcome = OutcomeError
		res.Err = err.Error()
		return res
	}
	for name, value := range t.Headers {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		res.Err = err.Error()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			res.Outcome = OutcomeTimeout
		case errors.Is(err, context.Canceled):
			// A cancelled check is normally caused by shutdown (or an explicit
			// caller cancel); it is reported as an error and the scheduler
			// decides whether to persist it.
			res.Outcome = OutcomeError
			res.Err = "context canceled"
		default:
			res.Outcome = OutcomeError
		}
		return res
	}

	// Always close the body, and drain a little first so the transport can
	// reuse the keep-alive connection instead of tearing it down. On a
	// read-only body neither error is actionable, so both are explicit drops.
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)

	res.StatusCode = resp.StatusCode
	switch {
	case t.ExpectedStatus > 0:
		if resp.StatusCode == t.ExpectedStatus {
			res.Outcome = OutcomeUp
		} else {
			res.Outcome = OutcomeDown
		}
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		res.Outcome = OutcomeUp
	default:
		res.Outcome = OutcomeDown
	}

	// resp.TLS belongs to the FINAL response, i.e. the connection that produced
	// StatusCode after any redirects were followed; PeerCertificates[0] is that
	// connection's leaf certificate. A plain HTTP response has resp.TLS == nil,
	// leaving CertExpiresAt at its zero value.
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.CertExpiresAt = resp.TLS.PeerCertificates[0].NotAfter
	}
	return res
}
