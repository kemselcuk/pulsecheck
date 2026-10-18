package checker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"pulsecheck/internal/config"
)

// testLogger discards scheduler logs so the test output stays readable.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// targetsConfig builds a config with n targets all pointing at the same URL.
func targetsConfig(url string, n, workers int, interval, timeout time.Duration) config.Config {
	cfg := config.Config{
		Defaults: config.DefaultsConfig{Workers: workers, Interval: interval, Timeout: timeout},
	}
	for i := 0; i < n; i++ {
		cfg.Targets = append(cfg.Targets, config.Target{
			Name:     fmt.Sprintf("target-%d", i),
			URL:      url,
			Interval: interval,
			Timeout:  timeout,
		})
	}
	return cfg
}

// waitForResults blocks until the recorder holds at least n results or timeout
// elapses. The 5ms poll is just a check granularity, not a timing assertion.
func waitForResults(t *testing.T, rec *MemoryRecorder, n int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(rec.Snapshot()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %d results, got %d", timeout, n, len(rec.Snapshot()))
}

// TestSchedulerRespectsWorkerLimit is the concurrency evidence test: the server
// measures how many checks run at the same time and it must never exceed the
// configured worker count.
func TestSchedulerRespectsWorkerLimit(t *testing.T) {
	const (
		// 2 workers against 6 targets means the bound is genuinely exercised;
		// a 100ms cadence with a 150ms handler guarantees queued overlap, and
		// 2s is ~20 cadences per target, far more than the one that shows the
		// peak.
		workers    = 2
		nTargets   = 6
		interval   = 100 * time.Millisecond
		handlerDur = 150 * time.Millisecond
		runFor     = 2 * time.Second
	)

	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()

		// 150ms of wall-clock time (not CPU), so overlap depends on two
		// workers being busy at once, not on machine speed.
		select {
		case <-time.After(handlerDur):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	cfg := targetsConfig(srv.URL, nTargets, workers, interval, 5*time.Second)
	rec := &MemoryRecorder{}
	s := New(cfg, client, rec, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	mu.Lock()
	got := maxInFlight
	mu.Unlock()

	t.Logf("workers=%d targets=%d: max observed concurrent checks = %d", workers, nTargets, got)

	// Hard requirement: the pool is structurally bounded.
	if got > workers {
		t.Errorf("max concurrent checks = %d, want <= %d", got, workers)
	}
	if got < 1 {
		t.Errorf("max concurrent checks = %d, want at least 1", got)
	}
	// Lower bound (generous): six targets fire immediately, the pool buffer
	// holds two jobs and each handler sleeps 150ms, so two workers overlap
	// deterministically at start-up. This demonstrates real parallelism
	// without relying on any tight timing margin.
	if got < 2 {
		t.Errorf("max concurrent checks = %d, want >= 2 to show overlap", got)
	}

	if results := rec.Snapshot(); len(results) == 0 {
		t.Error("no results recorded, want checks to have run")
	}
}

// TestSlowTargetDoesNotBlockOthers is the D4 evidence test: a target that keeps
// timing out must not stop other targets from being checked on their own
// cadence, and its own overlapping ticks must be recorded as skipped.
func TestSlowTargetDoesNotBlockOthers(t *testing.T) {
	const (
		workers = 2
		// 200ms cadence over 2.5s gives each target ~12 ticks. The slow
		// handler (1.2s) exceeds its 1s timeout by a 200ms margin, well above
		// any sub-50ms timing assumption, so the result is deterministic.
		interval     = 200 * time.Millisecond
		slowTimeout  = 1 * time.Second
		slowHandler  = 1200 * time.Millisecond
		runFor       = 2500 * time.Millisecond
		minFastUp    = 6
		minSlowSkips = 3
	)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(slowHandler):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)

	cfg := config.Config{
		Defaults: config.DefaultsConfig{Workers: workers, Interval: interval, Timeout: 5 * time.Second},
		Targets: []config.Target{
			{Name: "fastA", URL: fast.URL, Interval: interval, Timeout: 5 * time.Second},
			{Name: "fastB", URL: fast.URL, Interval: interval, Timeout: 5 * time.Second},
			{Name: "slow", URL: slow.URL, Interval: interval, Timeout: slowTimeout},
		},
	}

	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)

	rec := &MemoryRecorder{}
	s := New(cfg, client, rec, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	var (
		fastAUp, fastBUp int
		slowSkips        int
		slowVerdicts     int
	)
	for _, r := range rec.Snapshot() {
		switch r.TargetName {
		case "fastA":
			if r.Outcome == OutcomeUp {
				fastAUp++
			}
		case "fastB":
			if r.Outcome == OutcomeUp {
				fastBUp++
			}
		case "slow":
			if r.Outcome == OutcomeSkipped && strings.Contains(r.Err, "in flight") {
				slowSkips++
			}
			if r.Outcome == OutcomeTimeout || r.Outcome == OutcomeDown {
				slowVerdicts++
			}
		}
	}

	// Over ~2.5s at a 200ms cadence each fast target has ~12 chances; >= 6
	// leaves a wide margin for slow CI while proving each keeps running while
	// the slow target is stuck.
	t.Logf("fastA Up = %d, fastB Up = %d, slow in-flight skips = %d, slow timeout/down = %d",
		fastAUp, fastBUp, slowSkips, slowVerdicts)
	if fastAUp < minFastUp {
		t.Errorf("fastA recorded %d Up results, want >= %d", fastAUp, minFastUp)
	}
	if fastBUp < minFastUp {
		t.Errorf("fastB recorded %d Up results, want >= %d", fastBUp, minFastUp)
	}
	if slowSkips < minSlowSkips {
		t.Errorf("slow target recorded %d in-flight skips, want >= %d", slowSkips, minSlowSkips)
	}
	if slowVerdicts < 1 {
		t.Errorf("slow target recorded %d timeout/down verdicts, want >= 1", slowVerdicts)
	}
}

// TestSchedulerGracefulShutdown checks that Run stops promptly on cancellation,
// aborts in-flight checks, returns nil, and leaves no goroutines behind.
func TestSchedulerGracefulShutdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	// A short 50ms cadence lets the test observe a first result quickly; the
	// 5s handler/timeout keeps every check in flight until shutdown, which is
	// what the test wants to cancel.
	cfg := config.Config{
		Defaults: config.DefaultsConfig{Workers: 2, Interval: 50 * time.Millisecond, Timeout: 5 * time.Second},
		Targets: []config.Target{
			{Name: "hang-a", URL: srv.URL, Interval: 50 * time.Millisecond, Timeout: 5 * time.Second},
			{Name: "hang-b", URL: srv.URL, Interval: 50 * time.Millisecond, Timeout: 5 * time.Second},
		},
	}

	rec := &MemoryRecorder{}
	s := New(cfg, client, rec, testLogger())

	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	// Wait for a real in-flight check to be recorded before cancelling, so the
	// shutdown path actually has work to abort.
	waitForResults(t, rec, 1, 2*time.Second)

	start := time.Now()
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run() did not return within 2s of cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Run() returned after %v, want < 2s", elapsed)
	}

	// Every recorded result must be well-formed even for the aborted check.
	for _, r := range rec.Snapshot() {
		switch r.Outcome {
		case OutcomeUp, OutcomeDown, OutcomeTimeout, OutcomeError, OutcomeSkipped:
		default:
			t.Errorf("result %+v has an unknown outcome after shutdown", r)
		}
		if r.TargetName == "" {
			t.Errorf("result %+v has an empty target name after shutdown", r)
		}
		if r.Duration < 0 {
			t.Errorf("result %+v has a negative duration", r)
		}
	}

	client.CloseIdleConnections()
	// Give finished handlers/transports a moment to unwind; the tolerance is
	// +/-1 goroutine as accepted by the task.
	time.Sleep(500 * time.Millisecond)
	after := runtime.NumGoroutine()
	t.Logf("goroutines: baseline=%d after shutdown=%d", baseline, after)
	if after > baseline+1 {
		t.Errorf("goroutines after shutdown = %d, want <= baseline+1 = %d", after, baseline+1)
	}
}

// TestSkippedPersistedWhilePreviousInFlight proves the persistence shape of
// skips: while a slow check runs, ticks are recorded as in-flight skips, and
// successful checks appear between them.
func TestSkippedPersistedWhilePreviousInFlight(t *testing.T) {
	const (
		// A 100ms cadence against an 800ms handler yields ~7 skipped ticks per
		// running check. The 1s timeout leaves a 200ms cushion over the
		// handler, and 2s covers two full checks so Up results land between
		// runs of skips.
		interval     = 100 * time.Millisecond
		timeout      = 1 * time.Second
		handlerSleep = 800 * time.Millisecond
		runFor       = 2 * time.Second
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(handlerSleep):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{}
	t.Cleanup(client.CloseIdleConnections)

	cfg := config.Config{
		Defaults: config.DefaultsConfig{Workers: 1, Interval: interval, Timeout: timeout},
		Targets:  []config.Target{{Name: "slowish", URL: srv.URL, Interval: interval, Timeout: timeout}},
	}

	rec := &MemoryRecorder{}
	s := New(cfg, client, rec, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	results := rec.Snapshot()

	ups := 0
	firstUp := -1
	inFlightSkips := 0
	for i, r := range results {
		switch {
		case r.Outcome == OutcomeUp:
			ups++
			if firstUp == -1 {
				firstUp = i
			}
		case r.Outcome == OutcomeSkipped && r.Err == reasonInFlight:
			inFlightSkips++
		}
	}

	if ups == 0 {
		t.Fatalf("no Up results recorded, want the successful checks (%s)", summarize(results))
	}
	if inFlightSkips == 0 {
		t.Fatalf("no in-flight skipped results recorded (%s)", summarize(results))
	}
	t.Logf("Up = %d, in-flight skips = %d", ups, inFlightSkips)

	// Skipped records must appear on both sides of a successful check: the
	// run is long enough for at least two checks, each bracketed by skips.
	before, after := 0, 0
	for i, r := range results {
		if r.Outcome == OutcomeSkipped && r.Err == reasonInFlight {
			if i < firstUp {
				before++
			} else {
				after++
			}
		}
	}
	if before == 0 || after == 0 {
		t.Errorf("in-flight skips before/after first Up = %d/%d, want both > 0 (%s)", before, after, summarize(results))
	}
}

// TestNewToleratesNilDependenciesAndEmptyConfig documents the fail-safe
// behaviour of New and that Run returns promptly with no targets.
func TestNewToleratesNilDependenciesAndEmptyConfig(t *testing.T) {
	rec := &MemoryRecorder{}
	s := New(config.Config{}, nil, rec, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return for an already-cancelled context")
	}
}

// TestSchedulerSaturatedDispatch covers the non-blocking dispatch default
// branch in Scheduler.tick: when the pool has no free capacity the tick must be
// recorded as skipped with reasonSaturated instead of stalling the ticker.
//
// Scenario. workers = 1 sizes the shared jobs buffer to a single slot, so the
// pool can hold at most two accepted jobs at once: one check actually running
// plus one waiting in the buffer. Three targets on a 10s interval each offer
// exactly one immediate job during the test's sub-second window, so three jobs
// meet a pool that can accept at most two and at least one tick must be rejected
// as saturated.
//
// Invariant under test (why the assertion is a range, not an exact count). Run
// starts the single worker and the three target loops concurrently, and Go gives
// no ordering guarantee between the worker reaching the jobs channel and the
// three near-simultaneous immediate ticks:
//
//   - If the worker is already parked on the channel before the burst, tick 1 is
//     handed straight to it, tick 2 fills the one buffer slot, and tick 3 finds
//     no room -> one saturated skip.
//   - If the worker has not been scheduled yet, tick 1 fills the buffer and ticks
//     2 and 3 both find it full -> two saturated skips.
//
// The handler blocks on <-release for the whole burst, so the worker can never
// drain more than the single job it is running and no capacity frees up
// mid-burst; that is what keeps the pool state stable and the count bounded. The
// guarantee the scheduler owes is capacity-bounded acceptance plus
// skip-on-overflow, i.e. running + buffered <= 2 and every overflow tick
// recorded as a saturated skip. It does not owe a particular split of which of
// ticks 2/3 overflows, so this test asserts 1 <= saturatedSkips <= 2 rather than
// an exact number. Deliberately, it never mutates scheduler internals or
// pre-starts the worker to force an interleaving; Run owns startup, and
// -race -count=200 exercises both interleavings to show the invariant holds.
func TestSchedulerSaturatedDispatch(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the test releases the burst, so no accepted check can
		// finish and free a slot while the ticks are being dispatched.
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	t.Cleanup(client.CloseIdleConnections)

	// workers = 1 -> one check running plus a single buffered job.
	cfg := targetsConfig(srv.URL, 3, 1, 10*time.Second, 5*time.Second)

	rec := &MemoryRecorder{}
	s := New(cfg, client, rec, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Let the worker start and all three immediate ticks fire. The handler is
	// still blocked on <-release, so the pool holds at most two accepted jobs
	// (one running, one buffered) and any overflow tick is recorded as saturated.
	// The 10s interval guarantees no further tick can arrive in this window, so
	// the run is bounded to exactly one tick per target.
	time.Sleep(200 * time.Millisecond)

	// Release the handler so the accepted checks can complete. Every target
	// fire exactly once, so the total record count is exactly 3 (accepted Up
	// records plus saturated skips).
	close(release)
	waitForResults(t, rec, 3, 5*time.Second)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s of cancellation")
	}

	results := rec.Snapshot()
	t.Logf("results: %s", summarize(results))

	// 1. Exactly one immediate tick per target, and every tick yields exactly one
	// record (accepted -> Up, overflow -> skipped), so the total is exactly 3.
	if len(results) != 3 {
		t.Fatalf("recorded %d results, want exactly 3 (%s)", len(results), summarize(results))
	}

	var saturatedSkips int
	for _, r := range results {
		switch r.Outcome {
		case OutcomeSkipped:
			// 2/4. With one tick per target, no previous check is ever still in
			// flight, so the only skip reason that can occur is saturation.
			if r.Err != reasonSaturated {
				t.Errorf("skipped result has reason %q, want %q: %+v", r.Err, reasonSaturated, r)
			}
			saturatedSkips++
		case OutcomeUp:
			// 3. Every accepted check completes against the released 200 handler.
			if r.StatusCode != http.StatusOK {
				t.Errorf("target %s recorded Up with status %d, want %d", r.TargetName, r.StatusCode, http.StatusOK)
			}
		default:
			// 4. No timeout/error/down/loss: the handler never fails and no
			// in-flight check is cancelled before it is recorded.
			t.Errorf("unexpected outcome %q for target %s (%s)", r.Outcome, r.TargetName, r.Err)
		}
	}

	// 2. Capacity-bounded acceptance: 1 <= saturatedSkips <= 2. The exact split
	// depends on worker-vs-tick goroutine scheduling; see the test comment.
	if saturatedSkips < 1 || saturatedSkips > 2 {
		t.Errorf("recorded %d saturated skips, want 1 or 2 (%s)", saturatedSkips, summarize(results))
	}
}

// TestSchedulerFollowRedirectsSelectsClient proves the scheduler owns redirect
// policy: two runs against the same redirecting URL, one following redirects
// and one not, must be classified from the status of the response the client
// actually returned (200 after following vs the raw 301).
func TestSchedulerFollowRedirectsSelectsClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// run starts a scheduler with a single target on a one-hour cadence, waits
	// for the immediate first check, then shuts down and returns that result.
	// The long interval guarantees exactly one recorded result.
	run := func(t *testing.T, follow bool) Result {
		t.Helper()

		client := srv.Client()
		t.Cleanup(client.CloseIdleConnections)

		cfg := config.Config{
			Defaults: config.DefaultsConfig{Workers: 1, Interval: time.Hour, Timeout: 5 * time.Second},
			Targets: []config.Target{{
				Name:            "redirect",
				URL:             srv.URL + "/start",
				FollowRedirects: follow,
				Interval:        time.Hour,
				Timeout:         5 * time.Second,
			}},
		}

		rec := &MemoryRecorder{}
		s := New(cfg, client, rec, testLogger())

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		waitForResults(t, rec, 1, 3*time.Second)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}

		results := rec.Snapshot()
		if len(results) != 1 {
			t.Fatalf("recorded %d results, want exactly 1", len(results))
		}
		return results[0]
	}

	t.Run("follow_redirects true reaches the final 200", func(t *testing.T) {
		res := run(t, true)
		if res.Outcome != OutcomeUp {
			t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeUp)
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("StatusCode = %d, want %d after following the redirect", res.StatusCode, http.StatusOK)
		}
	})

	t.Run("follow_redirects false records the raw 301 as down", func(t *testing.T) {
		res := run(t, false)
		if res.Outcome != OutcomeDown {
			t.Errorf("Outcome = %q (err=%q), want %q", res.Outcome, res.Err, OutcomeDown)
		}
		if res.StatusCode != http.StatusMovedPermanently {
			t.Errorf("StatusCode = %d, want the raw %d", res.StatusCode, http.StatusMovedPermanently)
		}
	})
}

func summarize(results []Result) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "%s=%s(%s)", r.TargetName, r.Outcome, r.Err)
	}
	return b.String()
}
