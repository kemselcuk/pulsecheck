# Decision Log

A chronological record of the technical decisions made in PulseCheck. Each entry: **context → decision → rationale**.
This log is CV/interview evidence: the *reason* each decision was made is traceable here.

---

## Decisions made during scoping (user-approved, 2026-10-18)

### D1 — Check interval: default 30s, per-target override
- **Context:** A demo that looks alive is nice; but the real monitoring cadence must also be sensible.
- **Decision:** Default `30s`; each target may override it via its own `interval` field.
- **Rationale:** The demo configuration drops it to 10s to show liveliness; the code does not change. A single fixed value cannot serve both needs.

### D2 — Request timeout: default 5s, per-target override
- **Context:** The timeout is the foundation of the "a delayed target never blocks the others" promise.
- **Decision:** Default `5s`; enforced with a per-request `context.WithTimeout`.
- **Rationale:** 2s is aggressive (slow-but-healthy targets would show false negatives), 10s stretches the demo. 5s is a realistic middle value; the demo `/slow` target is configured to exceed it (8s).

### D3 — History retention: 24 hours, background cleanup
- **Context:** Even without unbounded growth, unlimited history is unnecessary in a small project.
- **Decision:** `results` rows older than 24 hours are deleted periodically (every 10 min).
- **Rationale:** A single parameter with a predictable file size; the "last N records per target" alternative would complicate the deletion logic.

### D4 — Overlapping check: skip and record as `skipped`
- **Context:** When a check takes longer than the interval (a delayed target), the next round's time arrives while it is still running.
- **Decision:** Per-target in-flight lock; if busy, the round is skipped and recorded with `outcome=skipped` plus a reason.
- **Rationale:** (a) The delayed target never grows a queue or blocks anyone; (b) the behaviour becomes testable; (c) "cancel the pending one" conflates `down` with "cancelled"; (d) running in parallel would add unnecessary load against the same target.

### D5 — Decision transfer: a single decision log (this file)
- **Context:** The user wants to understand and explain every decision; an ADR folder / README section / code walkthrough were discussed.
- **Decision:** One chronological `docs/decisions.md`.
- **Rationale:** Lighter than ADRs but carries the same information; folder management is needless overhead in a small project.

### D6 — Documentation structure: three central files
- **Decision:** `README.md` (usage, integration, evidence) + `docs/decisions.md` + `docs/PLAN.md`. No per-task files.
- **Rationale:** A lot of information in few places for a small project; the trail lives in PLAN.md, the rationale here, the usage in the README.

### D7 — Subagent/model distribution
- **Context:** The coding is done by agents over OpenCode Go; identities were verified against the live catalog.
- **Decision:** Writer: `opencode-go/deepseek-v4.1-flash` · Independent reviewer: `opencode-go/glm-5.3-flash` · Orchestrator/acceptance: `glm-5.3-flash` (main session) · Final approval: the user.
- **Rationale:** The two Flash models are the cheapest option; independence is provided at the context level (separate session, zero shared context). Review with larger models would not pay off at this scale.

### D8 — Quality gate: evidence + reviewer + orchestrator
- **Decision:** Every task requires mandatory evidence (real output of `go vet` + `go test -race` + a checked acceptance list) → independent reviewer → the orchestrator's own verification → a synthesis for the user.
- **Rationale:** A "done" report is not trustworthy on its own; the evidence chain makes it verifiable.

### D9 — Where the evidence lives: GitHub Actions + README
- **Decision:** CI: `go vet`, `go test -race`, `go build` on every push/PR; badge + real output in the README.
- **Rationale:** Free on a public repo; concurrency claims are tied to independent, repeatable evidence.

### D10 — Demo video: MP4 assembled with ffmpeg from screenshots
- **Context:** There is no built-in live screen-recording tool; ffmpeg is installed (verified 2026-10-18).
- **Decision:** Scheduled screenshots of the status page → assembled into an MP4 with ffmpeg; fallback if ffmpeg is missing: a script plus a user-recorded capture.
- **Rationale:** The video is produced entirely locally (€0), reproducible with a script.

### D11 — Cost: infrastructure €0
- **Decision:** Local SQLite file, local demo target server, GitHub Actions free tier. The only variable is token cost.
- **Rationale:** The project stays within the "one application + one database" boundary; no design requiring an external service is chosen.

---

## Technical decisions made with the plan (plan approval, 2026-10-18)

### D12 — Config format: JSON
- **Context:** YAML reads easily but requires the `gopkg.in/yaml.v3` dependency.
- **Decision:** JSON config via `encoding/json`.
- **Rationale:** Zero extra dependencies; a perfect fit with stdlib testing; the structure is small, no readability loss.

### D13 — SQLite driver: `modernc.org/sqlite`
- **Context:** `mattn/go-sqlite3` requires CGO; `modernc.org/sqlite` is pure Go.
- **Decision:** `modernc.org/sqlite`.
- **Rationale:** CGO-free static builds → a simple, small Docker image and trouble-free cross-compilation; the performance difference is irrelevant at this scale.

### D14 — Demo targets: an embedded `cmd/demo-targets` server
- **Context:** Having the 200/500/delayed targets depend on the internet (httpstat.us etc.) breaks demo reliability.
- **Decision:** A small local server using stdlib only: `/ok`→200, `/broken`→500, `/slow`→8s delay.
- **Rationale:** Demo and tests are offline, reproducible, €0.

### D15 — Integration story: over the HTTP API
- **Context:** "Integrating into another project" can happen two ways: library import or API.
- **Decision:** Packages stay under `internal/` (closed to the outside); integration via the API + examples (curl, JS fetch).
- **Rationale:** Preserves the "one application" boundary; if you do not want a library surface, API versioning is clearer.

### D16 — Git workflow: a branch per phase, a commit per task
- **Decision:** Short-lived phase branches (`faz-0-iskelet` etc.), merged into `main` after acceptance; each task is its own commit. CI runs on `main` + PRs.
- **Rationale:** The base history stays readable; the PR history is evidence of review culture. (Simplified from the plan's original "a branch per task" to the phase scale — small tasks are tracked as separate commits on a single phase branch.)

---

## Decisions made during implementation

### D17 — Empty string config fields: fall back to defaults, do not error
- **Context:** A "minor" finding from the Phase 1 review: `server.addr` / `storage.db_path` fall back to defaults when empty or whitespace-only, while numeric fields error on an explicit 0.
- **Decision:** The behaviour stays for string fields (empty = treated as omitted); the behaviour is documented in the package doc and a code comment. Target `name` is the exception: it cannot be empty.
- **Rationale:** The "strict error" alternative punishes the user needlessly; making a semantic difference between not writing a field and writing `""` is confusing in a small configuration. Ambiguous behaviour is unacceptable — so it was resolved by documenting it. (Phase 1 review round, 2026-10-18.)

### D18 — Handling of review findings: minor/nit tracking
- **Decision:** A "minor" finding is either fixed in that round or recorded in the decision log; "nits" are consciously accepted, and the noteworthy ones are recorded: (a) a cancelled `/slow` request may appear as 200 in the log (cosmetic), (b) a bind failure plus a simultaneous signal can mask each other in a microsecond window (theoretical, accepted).
- **Rationale:** Nit-hunting slows delivery in a small project; but every accepted finding must have a reasoned record.

### D19 — Timing tests assert invariants, never force an interleaving
- **Context:** Testing the "worker pool saturation" branch in Phase 2, the first draft expected exactly 1 skip; a 200-run stress test showed it could be 2 skips ~17% of the time (a worker-vs-tick goroutine scheduling race). The first fix attempt forced the interleaving by poking private fields — rejected.
- **Decision:** The test asserts the behavioural invariant: exactly 3 records in total; saturation skips between 1 and 2; every accepted record is up/200; no other outcome. Private-state access is forbidden; `Run` owns startup inside the test itself. Both interleavings are exercised by `-race -count=200`.
- **Rationale:** The scheduler's obligation is "capacity-bounded acceptance + skip on overflow"; whether the worker takes a job from the buffer or directly from the channel is not part of the contract. Tests that force interleavings are brittle and read as defects on a CV. (Phase 2 follow-up round, 2026-10-18.)

### D20 — Checks cancelled at shutdown: recorded as `error`/"context canceled"
- **Context:** Graceful shutdown interrupts an in-flight check with a context cancellation; whether to persist that result was undecided.
- **Decision:** It is persisted (outcome `error`, Err "context canceled"); if shape-filtering is ever needed, it belongs in the store/UI layer. The checker does not hide information.
- **Rationale:** The record is truthful (the check genuinely got cut off); filtering inside the checker is an information loss and lowers testability. Phase 2 review decision.

### D21 — Retention misconfiguration: data destruction must never be silent
- **Context:** The Phase 3 review presented a "minor" finding: with `window <= 0` the cleaner would delete the whole table (the cutoff becomes ≥ now). Config validation blocks it in production, but programmatic use was exposed.
- **Decision:** `window <= 0` → retention fully disabled + a loud `Warn`; `deleteOlderThan` also returns a no-op (double safety). `period <= 0` already runs once + Warns — the asymmetry is gone. Two new tests pin it.
- **Rationale:** The failure mode of "delete" must never be "delete everything"; suppressed data loss is worse than a warned no-op. (Phase 3 review finding, orchestrator patch, 2026-10-18.)

### D22 — The `Record`/`RecordResult` split
- **Context:** The `checker.Recorder` interface has no error return; the store also wanted an error-reporting write function — Go has no overloading, so one signature cannot do both.
- **Decision:** `Record(Result)` satisfies the interface (logs persistence failures), `RecordResult(Result) error` serves callers that need the error. Compile-time assertion: `var _ checker.Recorder = (*Store)(nil)`.
- **Rationale:** Changing the Phase 2 interface now would needlessly shake `MemoryRecorder` + the scheduler; if a real need for the error channel appears, it changes then. The decision not to persist `r.URL` also belongs here: targets come from config and `target` is the natural key — persisting it would create a stale-copy problem when the config changes. (Phase 3 review decision.)

### D23 — Demo ports must not collide with common services on a developer machine
- **Context:** On this machine the user's Docker service (mongo-express) already held `127.0.0.1:8081`; `demo-targets` bound the IPv6 side (::1) so the first run worked, but once the process died requests fell through to Express over IPv4 — every target went `down`. An extra environmental lesson: with the default interval (10s) larger than the timeout (5s), the `skipped` behaviour never appears live.
- **Decision:** Demo port `18081` (away from common developer ports); in `config.demo.json` the `demo-slow` target takes an `interval: 3s` override (timeout 5s > interval → in-flight skips become visible live). The verification test pins the expected interval per target.
- **Rationale:** The "easy local start" promise includes not colliding with services already running on the user's machine; the demo video (K10) must also make the `skipped` records visible. (Learned during the Phase 4 live verification, 2026-10-18.)

### D24 — Shutdown: a single signal context owns the whole lifecycle
- **Context:** In Phase 5 the checker, retention and HTTP server merge into one `main`; who waits for whom at shutdown was undecided.
- **Decision:** The single context produced by `signal.NotifyContext(SIGINT, SIGTERM)` is the context of both the checker and retention. On signal: HTTP `Shutdown` drains first (5s deadline), then the checker's return is awaited with a 10s hard cap, then the store is closed and "shutdown complete" is logged. A wait timeout → an honest error + `exit 1`.
- **Rationale:** Creating a second mechanism for "cancel" (an extra cancel function) would create two sources of truth; the context is already the checker's cancellation path. The hard cap forbids the process from hanging forever on a bug — the "never hang" promise is guaranteed by a testable exit code. (Phase 5 implementation, 2026-10-18.)

### D25 — Docker: one Dockerfile, two binaries, two compose services
- **Context:** The service and the demo target are separate programs; "two images or one image" was an open question.
- **Decision:** A single multi-stage `Dockerfile` produces two static binaries (`/pulsecheck`, `/demo-targets`); a single final image carries `ENTRYPOINT ["/pulsecheck"]`. In compose, the `demo-targets` service overrides with `entrypoint: ["/demo-targets"]`; the `pulsecheck` service only appends arguments via `command: ["-config", "..."]`.
- **Rationale:** Two images double image management and build time; one image + two entrypoints is smaller and sufficient. Because Docker appends `command` AFTER the `ENTRYPOINT`, repeating the binary name in compose (the `command: /pulsecheck -config ...` form from the task text) would run `/pulsecheck /pulsecheck -config ...`; therefore `command` carries only the arguments. (Phase 5, a deliberate deviation from the task text, 2026-10-18.)

### D26 — Runtime image: alpine, not scratch
- **Context:** With a static binary, `scratch` gives the smallest image.
- **Decision:** Final image `alpine:3.21` + `ca-certificates`, running as the non-root `pulsecheck` user.
- **Rationale:** `scratch` contains neither `ca-certificates` nor a shell; `https://` targets (present in the example config) would silently fail with `x509` errors and debugging would be impossible. The few saved MB are irrelevant next to an image that works in the real world. (Phase 5, 2026-10-18.)

### D27 — Known, accepted behaviours: error paths and the second signal
- **Context:** The Phase 5 review presented two "minors": (1) on bind-failure / Shutdown-error paths the process returns without waiting for the checker — `st.Close()` can race with an in-flight write; (2) a second SIGINT during shutdown is swallowed; there is no forced exit.
- **Decision:** Both consciously accepted, not fixed. For (1): `database/sql`'s `Close` waits for queries that have already started and the process is exiting with a failure anyway; the happy path keeps the full "drain-then-close" discipline. For (2): the 10s checker hard cap bounds total unresponsiveness to ~15s; the classic "second signal = force exit" behaviour can be added later with a small `stop()` shift.
- **Rationale:** Refactoring for races that cannot be observed slows scope discipline in a small project; documenting the behaviour separates a "hidden defect" from a "conscious limitation". (Phase 5 review, 2026-10-18.)

### D28 — Demo video pipeline: Chrome headless + ffmpeg, entirely local
- **Context:** The T3 session browser tool was not connected to this session; there is no built-in screen recorder either.
- **Decision:** Live stack via `make demo` + 36 screenshots every 2s via Chrome's headless CLI (`--headless --screenshot --virtual-time-budget`) + ffmpeg H.264 (`-framerate 0.5`, crf 27, faststart) → a 72s MP4 (~64 KB). Frames are not kept in the repo (a temp directory); `assets/demo.mp4` + the static `assets/demo.png` enter the repo.
- **Rationale:** €0, reproducible with a script, embeddable in the README; no dependency on heavyweight GIF/screen-recording tooling. (Phase 6, 2026-10-18.)

### D29 — CI activation deferred: the file is parked locally, not in the repo
- **Context:** GitHub rejected a push containing the CI file from a token without the `workflow` scope; the user temporarily chose to push without the workflow. The Phase 7 final review misread this state (it assumed the working-tree file was "tracked" and returned CHANGES REQUESTED) — but its underlying point was right: the K9/D9 promise of "green CI + badge" was still an **unverified promise**.
- **Decision:** `ci.yml` was not in the repo (untracked in HEAD, proven by a GitHub API 404); the file was parked locally via `.git/info/exclude`. Once the user granted the scope via `gh auth refresh -h github.com -s workflow`: the file was re-added, pushed, and the first green run proved it with a README badge. Until then K9 was **partial**: the CI file was ready and valid, awaiting run evidence.
- **Rationale:** The decision-log discipline requires marking an unverified promise explicitly as "pending"; the final review's demand was in fact already satisfied — the record pins that with evidence. (Phase 7, 2026-10-18.)
- **Closure:** The user granted the scope; `ci.yml` was restored (commit `cd1bb22`) and the first run returned green (run `39141315004`, 2026-10-18: vet + `go test -race` + build). The badge was added to the README; K9 is now **full**.

---

## Decisions made during v1.1

### D30 — v1.1 scope lock and conscious deferrals
- **Context:** In the v1.1 scoping round the user selected 10 items: `/metrics`, uptime %, `/healthz`, a sparkline detail page, TLS certificate tracking, richer HTTP check definitions, config fuzzing, a benchmark package, a release pipeline + golangci-lint, and the `v1.0.0` tag.
- **Decision:** (a) The state-change webhook stays **out of scope** (the user confirmed again — the "not an alerting platform" boundary is preserved). (b) In the richer check definition, when `expected_status` is set the up rule is `status == expected`; unset, the 2xx rule applies — both judged on the FINAL response of the redirect chain. (c) The Phase 1 review's "minor" finding — in hand-built `config.Target` literals a `FollowRedirects bool` zero value means "do not follow" — is consciously deferred: the only consumer is the scheduler, always fed by `Parse` (default true); it becomes `*bool` when a second consumer appears. (d) `skipped` rounds are excluded from the uptime denominator: a round where no check ran is not an outage (implemented in Phase 2).
- **Rationale:** Reasoned records of deferrals carry the D18/D27 discipline of v1.0 into v1.1; a combined pin test for "expected_status + redirects" (orchestrator) closed the review's nit. (v1.1 Phase 1, 2026-10-18.)

### D31 — Metrics: pencere gauge'ı, `_total` counter yalanı ve canlı scrape dengesi
- **Bağlam:** `/metrics` tasarlanırken iki tuzak vardı: (1) retention eski satırları sildiği için "toplam sonuç" süreç-ömürlü counter olarak İFADE EDİLEMEZ — sayaç azalır, PromQL'de `rate()` yalan söyler; (2) scrape, veriyi nereden okuyacak?
- **Karar:** (1) `pulsecheck_check_results_24h` bir **gauge** olarak adlandırılır ve aile yorumunda "windowed, not a monotonic counter" gerekçesiyle belgelenir. (2) Scrape, canlı store sorgularıyla yanıtlanır (bu ölçekte her zaman veritabanıyla tutarlı; süreç-içi registry gelecekteki seçenektir — yorumda). Bilinmeyen değerler (süre/sertifika/timestamp) uydurulmuş 0 yerine **seri yok** olarak dışlanır; hiç checked edilmemiş hedef için `target_up 0`. (3) Label escaping (backslash/quote/newline, tek geçiş) doğruluk gerekliliğidir: kullanıcı config'inden gelen hedef adı ekstra seri enjekte edemesin.
- **Gerekçe:** Observability'nin bir numaralı kuralıdır: dışa açılan değer "ileri geri tutarlı" olmalı. Review, SQL'in `EXPLAIN QUERY PLAN` ile index kullandığını ve escaping'in tek-geçiş olduğunu empirik doğruladı; byte-stability testle pin'li. (v1.1 Faz 2, 2026-10-18.)
