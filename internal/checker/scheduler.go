package checker

import (
	"context"
	"crypto/tls"
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

// job is one unit of work handed to a worker. It carries the runner rather than
// a position, so a reload can add and remove targets while jobs are queued.
type job struct {
	runner *targetRunner
}

// targetRunner is the scheduling state of one target: its current definition
// (immutable once stored — an update replaces the runner), the in-flight flag
// (decision D4), the carry-across stop channel of its ticker loop, and the
// client pair the target's checks use (the shared pair, or a TLS-adjusted
// derivative when the target configures per-target TLS trust — D42). The flag
// is a pointer so an update can carry it across, keeping "previous check still
// in flight" true while the target's definition changes (D41).
type targetRunner struct {
	target           config.Target
	inFlight         *atomic.Bool
	stop             chan struct{}
	client           *http.Client
	noRedirectClient *http.Client
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
//   - Reload replaces the target set at runtime (D41): added targets get a new
//     loop, removed targets' loops stop without interrupting an in-flight
//     check, and an updated target carries its in-flight flag across.
//
// The scheduler imports config only; persistence is behind Recorder.
type Scheduler struct {
	client *http.Client
	// noRedirectClient is the twin of client that stops at the first response
	// instead of following redirects. Redirect policy lives on the client, not
	// in Check, so the scheduler owns both variants and picks one per target
	// (see New and runJob).
	noRedirectClient *http.Client
	rec              Recorder
	logger           *slog.Logger

	jobs    chan job
	workers int

	// mu guards runners and ctx; the per-runner fields are either immutable
	// (target) or atomic (inFlight).
	mu      sync.Mutex
	ctx     context.Context
	runners map[string]*targetRunner
	wg      sync.WaitGroup
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

	s := &Scheduler{
		client:           client,
		noRedirectClient: withoutRedirects(client),
		rec:              rec,
		logger:           logger,
		jobs:             make(chan job, workers),
		workers:          workers,
		runners:          make(map[string]*targetRunner, len(cfg.Targets)),
	}
	for _, target := range cfg.Targets {
		s.runners[target.Name] = s.newRunner(target, nil)
	}
	return s
}

// newRunner builds the scheduling state of one target: the client pair derived
// from the target's TLS configuration (D42) and, when prev is non-nil (an
// update carrying state across a reload, D41), prev's in-flight flag.
func (s *Scheduler) newRunner(target config.Target, prev *targetRunner) *targetRunner {
	client, noRedirect := s.clientsFor(target)
	runner := &targetRunner{
		target:           target,
		inFlight:         &atomic.Bool{},
		stop:             make(chan struct{}),
		client:           client,
		noRedirectClient: noRedirect,
	}
	if prev != nil {
		runner.inFlight = prev.inFlight
	}
	return runner
}

// clientsFor derives the redirecting and non-following clients for one target.
// Without per-target TLS settings the shared pair is reused, so the common
// path allocates nothing; with them, a copy of the shared client gets a cloned
// transport whose TLS config carries the target's extra roots and/or the
// verification opt-out (D42). The copy preserves the shared CheckRedirect, Jar
// and Timeout, which is what lets an injected client (httptest's, say) keep
// working on both the following and the non-following path.
func (s *Scheduler) clientsFor(t config.Target) (client, noRedirect *http.Client) {
	if t.TLS.CAPool == nil && !t.TLS.InsecureSkipVerify {
		return s.client, s.noRedirectClient
	}

	base := *s.client
	base.Transport = transportWithTLS(s.client.Transport, t.TLS)
	twin := withoutRedirects(&base)
	return &base, twin
}

// closeIdleConnections releases the idle connections of a runner's DERIVED
// client pair when it is replaced or removed. The shared pair is left alone:
// other targets are still using it.
func (s *Scheduler) closeIdleConnections(runner *targetRunner) {
	if runner.client == s.client {
		return
	}
	runner.client.CloseIdleConnections()
	runner.noRedirectClient.CloseIdleConnections()
}

// transportWithTLS returns a transport that applies the target's TLS settings.
// A clone of the base *http.Transport is adjusted (TLS config cloned first, so
// a shared transport is never mutated); non-*http.Transport round trippers
// cannot be cloned, so the default transport is the honest fallback base.
func transportWithTLS(base http.RoundTripper, tlsConfig config.TLSConfig) http.RoundTripper {
	var transport *http.Transport
	if typed, ok := base.(*http.Transport); ok {
		transport = typed.Clone()
	} else {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}

	cfg := transport.TLSClientConfig
	if cfg == nil {
		cfg = &tls.Config{}
	} else {
		cfg = cfg.Clone()
	}
	if tlsConfig.CAPool != nil {
		cfg.RootCAs = tlsConfig.CAPool
	}
	if tlsConfig.InsecureSkipVerify {
		// Deliberate, configured opt-out (D42); documented loudly in the README.
		cfg.InsecureSkipVerify = true
	}
	transport.TLSClientConfig = cfg
	return transport
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
// Run must be called once per Scheduler; Reload may be called while it runs.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.logger.Info("scheduler started", "workers", s.workers, "targets", len(s.runners))
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, &s.wg)
	}
	for _, runner := range s.runners {
		s.startLoop(runner)
	}
	s.mu.Unlock()

	<-ctx.Done()
	s.logger.Info("scheduler shutdown: dispatch stopped")

	s.wg.Wait()
	s.logger.Info("scheduler shutdown complete")

	return nil
}

// Reload atomically replaces the target set with cfg.Targets (D41). It must be
// called while Run is active; calls after shutdown has begun are ignored (the
// only caller, the process lifecycle loop, never races this, but the guard
// keeps the contract explicit).
//
// The swap never interrupts work: an in-flight check of a removed target
// finishes and records normally, and an updated target keeps its in-flight
// flag — the replacement loop's first tick is then recorded as skipped rather
// than running a second check concurrently (D4 stays true across reloads). An
// added target behaves exactly like one at startup: an immediate check, then
// its interval.
//
// Only the target set is reloadable: defaults.workers sized the worker pool at
// New, and the caller is responsible for rejecting other configuration changes
// as restart-required (the reload policy in the wiring layer).
func (s *Scheduler) Reload(cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx == nil || s.ctx.Err() != nil {
		s.logger.Info("scheduler reload ignored: not running")
		return
	}

	seen := make(map[string]struct{}, len(cfg.Targets))
	for _, target := range cfg.Targets {
		seen[target.Name] = struct{}{}
		existing, ok := s.runners[target.Name]
		if ok && config.TargetEqual(existing.target, target) {
			continue
		}
		var prev *targetRunner
		if ok {
			s.closeIdleConnections(existing)
			close(existing.stop)
			// Carry state across the update so a check that is still running
			// keeps guarding its target until it records (D41).
			prev = existing
		}
		runner := s.newRunner(target, prev)
		s.runners[target.Name] = runner
		s.startLoop(runner)
	}
	for name, runner := range s.runners {
		if _, ok := seen[name]; !ok {
			s.closeIdleConnections(runner)
			close(runner.stop)
			delete(s.runners, name)
		}
	}
}

// startLoop launches the ticker loop of runner; the caller holds s.mu.
func (s *Scheduler) startLoop(runner *targetRunner) {
	s.wg.Add(1)
	go s.targetLoop(runner)
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
	target := j.runner.target

	// The per-request timeout is the only timeout mechanism; deriving it from
	// ctx means shutdown cancels the check immediately.
	checkCtx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()

	client := j.runner.client
	if !target.FollowRedirects {
		client = j.runner.noRedirectClient
	}

	res := Check(checkCtx, target, client)

	s.rec.Record(res)
	j.runner.inFlight.Store(false)
}

// targetLoop owns the cadence of a single target and never blocks on the pool.
// It exits on shutdown, or when a reload replaces or removes its target.
func (s *Scheduler) targetLoop(runner *targetRunner) {
	defer s.wg.Done()

	ticker := time.NewTicker(runner.target.Interval)
	defer ticker.Stop()

	// Check once immediately so a freshly started (or newly added) target
	// reports a first result without waiting a full interval, then follow the
	// cadence.
	s.tick(runner)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-runner.stop:
			return
		case <-ticker.C:
			s.tick(runner)
		}
	}
}

// tick attempts to dispatch one check for a target. It refuses when the
// previous check is still in flight and never blocks the calling ticker.
func (s *Scheduler) tick(runner *targetRunner) {
	if !runner.inFlight.CompareAndSwap(false, true) {
		s.recordSkip(runner.target, reasonInFlight)
		return
	}

	select {
	case <-s.ctx.Done():
		// Shutting down: release the flag we just took; no check will run.
		runner.inFlight.Store(false)
	case s.jobs <- job{runner: runner}:
		// A worker now owns the flag and clears it once the result is recorded.
	default:
		// Pool saturated: no queue slot was available. Release the flag
		// because no worker will clear it for us.
		runner.inFlight.Store(false)
		s.recordSkip(runner.target, reasonSaturated)
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
