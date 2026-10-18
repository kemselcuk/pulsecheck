package checker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// the 2xx range otherwise. When the target also configures a body assertion,
	// the body must satisfy it as well.
	OutcomeUp Outcome = "up"
	// OutcomeDown means a response was received but failed the acceptance rules:
	// a status mismatch, or a configured body assertion that did not hold.
	OutcomeDown Outcome = "down"
	// OutcomeTimeout means the request (or the body read of a configured body
	// assertion) did not finish before its deadline.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeError means any other failure (connection error, bad URL, ...).
	OutcomeError Outcome = "error"
	// OutcomeSkipped means no check ran because the target was busy.
	OutcomeSkipped Outcome = "skipped"
)

// maxBodyRead bounds how much of a response body is read when the target
// configures a body assertion: only the first 64 KiB are searched, so a
// pathological or endless body can neither grow the checker's memory nor hold
// the request open. A match that exists only beyond the cap cannot be seen and
// counts as a mismatch; the cap is part of the documented contract (D38).
const maxBodyRead = 64 << 10

// Result describes what happened during one check of a target.
//
// Err carries the reason for the error, timeout, and skipped outcomes, and for
// a down outcome that failed a body assertion; a plain status mismatch is
// described by StatusCode instead.
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
// When t.ExpectBodyContains or t.ExpectBodyRegex is configured, an accepted
// status must additionally satisfy the body rule — an endpoint answering "200
// OK" with an error page is then classified down with a describing Err. Only
// the first maxBodyRead bytes of the body are searched (decision D38).
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
		res.Outcome, res.Err = classifyTransportError(err)
		return res
	}

	// Always close the body; how much of it is read depends on whether a body
	// assertion is configured (see below). On a read-only body the close error
	// is not actionable, so it is an explicit drop.
	defer func() { _ = resp.Body.Close() }()

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

	// The certificate is captured for every response — including one that fails
	// a body assertion: the TLS handshake completed either way. resp.TLS belongs
	// to the FINAL response, i.e. the connection that produced StatusCode after
	// any redirects were followed; PeerCertificates[0] is that connection's leaf
	// certificate. A plain HTTP response has resp.TLS == nil, leaving
	// CertExpiresAt at its zero value.
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.CertExpiresAt = resp.TLS.PeerCertificates[0].NotAfter
	}

	// Body handling. With a configured assertion the first maxBodyRead bytes of
	// the body become part of the acceptance rules; reading them also serves
	// keep-alive reuse. Without one the body is not needed, so only a small
	// drain happens so the transport can reuse the connection instead of
	// tearing it down.
	if res.Outcome == OutcomeUp && (t.ExpectBodyContains != "" || t.ExpectBodyRegex != nil) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
		if err != nil {
			res.Outcome, res.Err = classifyTransportError(err)
			return res
		}
		switch {
		case t.ExpectBodyContains != "" && !bytes.Contains(body, []byte(t.ExpectBodyContains)):
			res.Outcome = OutcomeDown
			res.Err = fmt.Sprintf("body does not contain %q", t.ExpectBodyContains)
		case t.ExpectBodyRegex != nil && !t.ExpectBodyRegex.Match(body):
			res.Outcome = OutcomeDown
			res.Err = fmt.Sprintf("body does not match regexp %q", t.ExpectBodyRegex.String())
		}
		return res
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	return res
}

// classifyTransportError maps a transport-level error — from sending the
// request or from reading the response body — to its outcome and message: a
// deadline overflow is a timeout, an explicit cancel is reported as a
// shutdown-side error, and anything else keeps the raw message.
func classifyTransportError(err error) (Outcome, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout, err.Error()
	case errors.Is(err, context.Canceled):
		return OutcomeError, "context canceled"
	default:
		return OutcomeError, err.Error()
	}
}
