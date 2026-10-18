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
	// OutcomeUp means the final HTTP status (after redirects) was 2xx.
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
}

// Check performs a single GET against t and classifies the outcome.
//
// The caller owns the deadline: ctx is passed to the request unchanged, so the
// scheduler can wrap it in a per-request context.WithTimeout while tests can
// supply their own. This is why Check never sets http.Client.Timeout — using a
// single timeout mechanism (the request context) keeps one source of truth,
// makes the behaviour testable with a plain context, and avoids confusing
// double-deadline interactions.
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		res.Outcome = OutcomeError
		res.Err = err.Error()
		return res
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
	// reuse the keep-alive connection instead of tearing it down.
	defer resp.Body.Close()
	io.CopyN(io.Discard, resp.Body, 512)

	res.StatusCode = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		res.Outcome = OutcomeUp
	} else {
		res.Outcome = OutcomeDown
	}
	return res
}
