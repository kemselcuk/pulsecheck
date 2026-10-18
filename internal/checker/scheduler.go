package checker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"pulsecheck/internal/config"
)

// Reasons recorded on a skipped result.
const (
	reasonInFlight  = "previous check still in flight"
	reasonSaturated = "worker pool saturated"
)

// Recorder receives the result of every check and skip the scheduler produces.
//
// The checker package deliberately does not depend on internal/store: the
// persistence layer implements this interface, and tests use MemoryRecorder.
// Implementations must be safe for concurrent use.
type Recorder interface {
	Record(Result)
}

// MemoryRecorder is a thread-safe in-memory Recorder, used by tests.
type MemoryRecorder struct {
	mu      sync.Mutex
	results []Result
}

// Record appends r. Safe for concurrent use.
func (m *MemoryRecorder) Record(r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, r)
}

// Snapshot returns a copy of the results recorded so far.
func (m *MemoryRecorder) Snapshot() []Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Result, len(m.results))
	copy(out, m.results)
	return out
}

// job is one unit of work handed to a worker.
type job struct {
	index  int // position in Scheduler.cfg.Targets, indexes the in-flight flag
	target config.Target
}

// Scheduler runs periodic checks against every configured target using a fixed
// bounded worker pool.
//
// Architecture:
//   - One goroutine per target owns a time.Ticker(target.Interval), so each
//     target keeps its own cadence for free.
//   - A shared, small jobs channel feeds a fixed pool of N = defaults.workers
//     worker goroutines. Bounded concurrency is structural: at most N checks
//     can run at any instant.
//   - Each target has an atomic in-flight flag. A tick flips it with
//     CompareAndSwap; when the previous check is still running the tick is
//     recorded as skipped instead of queued (decision D4).
//   - Dispatch is non-blocking: if the pool is saturated the tick is recorded
//     as skipped rather than stalling the ticker.
//
// The scheduler imports config only; persistence is behind Recorder.
type Scheduler struct {
	cfg    config.Config
	client *http.Client
	// noRedirectClient is the twin of client that stops at the first response
	// instead of following redirects. Redirect policy lives on the client, not
	// in Check, so the scheduler owns both variants and picks one per target
	// (see New and runJob).
	noRedirectClient *http.Client
	rec              Recorder
	logger           *slog.Logger

	jobs     chan job
	inFlight []atomic.Bool
	workers  int
}

// New builds a Scheduler. A nil client falls back to &http.Client{} and a nil
// logger to one that discards all output, so callers cannot accidentally panic
// on a missing dependency (the wiring layer passes both explicitly). rec is
// required: a missing recorder is a wiring bug and must not be hidden.
//
// The injected client is the REDIRECTING default. A non-redirecting twin is
// derived from it (same Transport and Jar, but a CheckRedirect returning
// http.ErrUseLastResponse) so that a caller-supplied client such as
// httptest's srv.Client() is honored for TLS and connection settings on both
// paths; only the redirect policy differs.
func New(cfg config.Config, client *http.Client, rec Recorder, logger *slog.Logger) *Scheduler {
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	workers := cfg.Defaults.Workers
	if workers <= 0 {
		// Config validation forbids this, but a non-positive pool would mean
		// nothing ever drains the queue; fall back to a single worker.
		workers = 1
	}

	return &Scheduler{
		cfg:              cfg,
		client:           client,
		noRedirectClient: withoutRedirects(client),
		rec:              rec,
		logger:           logger,
		jobs:             make(chan job, workers),
		inFlight:         make([]atomic.Bool, len(cfg.Targets)),
		workers:          workers,
	}
}

// withoutRedirects returns a copy of src that does not follow redirects: the
// first response is returned as-is. The copy inherits src's Transport, Jar, and
// Timeout, so TLS roots and connection pooling are unchanged. Returning the
// last response (rather than an error) is what lets Check classify the
// redirect status itself.
func withoutRedirects(src *http.Client) *http.Client {
	twin := *src
	twin.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &twin
}

// Run starts the worker pool and one loop per target, then blocks until ctx is
// cancelled. On cancellation it stops dispatching immediately and waits for
// in-flight checks to finish: each check runs with a context derived from ctx,
// so cancellation aborts them at once. Run returns nil after a clean shutdown.
//
// Run must be called once per Scheduler.
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("scheduler started", "workers", s.workers, "targets", len(s.cfg.Targets))

	var wg sync.WaitGroup

	for i := 0; i < s.workers; i++ {
		wg.Add(1)
		go s.worker(ctx, &wg)
	}
	for i, target := range s.cfg.Targets {
		wg.Add(1)
		go s.targetLoop(ctx, &wg, i, target)
	}

	<-ctx.Done()
	s.logger.Info("scheduler shutdown: dispatch stopped")

	wg.Wait()
	s.logger.Info("scheduler shutdown complete")

	return nil
}

// worker pulls jobs from the shared queue until the scheduler context is done.
func (s *Scheduler) worker(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.jobs:
			s.runJob(ctx, j)
		}
	}
}

// runJob performs one check and records its result. The in-flight flag is
// cleared only after the result is recorded, so the persist window is covered
// by "in flight" too: a tick arriving mid-persist is skipped, not duplicated.
//
// Redirect policy is chosen here, per target: the redirecting client is the
// default, and the non-redirecting twin is used when the target asked for
// follow_redirects = false. Check itself is policy-agnostic.
func (s *Scheduler) runJob(ctx context.Context, j job) {
	// The per-request timeout is the only timeout mechanism; deriving it from
	// ctx means shutdown cancels the check immediately.
	checkCtx, cancel := context.WithTimeout(ctx, j.target.Timeout)
	defer cancel()

	client := s.client
	if !j.target.FollowRedirects {
		client = s.noRedirectClient
	}

	res := Check(checkCtx, j.target, client)

	s.rec.Record(res)
	s.inFlight[j.index].Store(false)
}

// targetLoop owns the cadence of a single target and never blocks on the pool.
func (s *Scheduler) targetLoop(ctx context.Context, wg *sync.WaitGroup, index int, target config.Target) {
	defer wg.Done()

	ticker := time.NewTicker(target.Interval)
	defer ticker.Stop()

	// Check once immediately so a freshly started scheduler reports a first
	// result without waiting a full interval, then follow the cadence.
	s.tick(ctx, index, target)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx, index, target)
		}
	}
}

// tick attempts to dispatch one check for a target. It refuses when the
// previous check is still in flight and never blocks the calling ticker.
func (s *Scheduler) tick(ctx context.Context, index int, target config.Target) {
	flag := &s.inFlight[index]
	if !flag.CompareAndSwap(false, true) {
		s.recordSkip(target, reasonInFlight)
		return
	}

	select {
	case <-ctx.Done():
		// Shutting down: release the flag we just took; no check will run.
		flag.Store(false)
	case s.jobs <- job{index: index, target: target}:
		// A worker now owns the flag and clears it once the result is recorded.
	default:
		// Pool saturated: no queue slot was available. Release the flag
		// because no worker will clear it for us.
		flag.Store(false)
		s.recordSkip(target, reasonSaturated)
	}
}

// recordSkip persists a skipped result and logs the event.
func (s *Scheduler) recordSkip(target config.Target, reason string) {
	s.rec.Record(Result{
		TargetName: target.Name,
		URL:        target.URL,
		StartedAt:  time.Now(),
		Outcome:    OutcomeSkipped,
		Err:        reason,
	})
	s.logger.Info("check skipped", "target", target.Name, "reason", reason)
}
