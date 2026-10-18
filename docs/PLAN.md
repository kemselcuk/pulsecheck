# PulseCheck — Implementation Plan

**Status:** ✅ Approved and implemented (2026-10-18) · Phases 0–6 complete; the single open item was CI activation, now resolved (see D29). v1.1 (rich checks, observability, robustness, shipping, docs) is complete; the first release `v1.0.0` is live.
**Accountability:** User = final scope and implementation approval. Orchestrator (me) = project lead and technical acceptance. Code = `opencode-go/deepseek-v4.1-flash`, independent review = `opencode-go/glm-5.3-flash`.

---

## 1. Scope

**Goal:** A single Go application that checks HTTP targets from a configuration file at intervals, concurrently through a bounded worker pool; stores results in SQLite; serves a small API + a single-page status screen; and shuts down gracefully.

**Out of scope:** Distributed scheduling, alerting/notification platforms, authentication, multi-user support, a database server.

### Locked decisions (user-approved)

| # | Decision | Value |
|---|----------|-------|
| K1 | Check interval | Default **30s**, per-target override |
| K2 | Request timeout | Default **5s**, per-target override |
| K3 | History retention | **24 hours**, periodic background cleanup |
| K4 | Overlapping check | While a target's previous check is still running, the new round is **skipped** and recorded as `skipped` + reason (per-target lock) |
| K5 | Decision transfer | Decision log: `docs/decisions.md` (every meaningful decision: context → rationale) |
| K6 | Documentation structure | Three central files: `README.md`, `docs/decisions.md`, `docs/PLAN.md` |
| K7 | Subagent/model split | Writer: `deepseek-v4.1-flash` · Reviewer: `glm-5.3-flash` (independent session) · Orchestrator: `glm-5.3-flash` |
| K8 | Quality gate | Mandatory evidence (real `go vet` + `go test -race` output + a checked acceptance list) → reviewer → me → the user |
| K9 | Where the evidence lives | GitHub Actions (free) + real output/badge in the README |
| K10 | Demo | Dashboard screenshots + **MP4 via ffmpeg** (fallback: a script + a user-recorded capture) |
| K11 | Cost | Infrastructure **€0**; the only variable is token cost (Flash models) |

### Technical decisions settled with the plan approval (T1–T5)

| # | Proposal | Rationale |
|---|----------|-----------|
| T1 | Config format **JSON** (`encoding/json`) | Zero extra dependencies; fits the stated stack |
| T2 | SQLite driver **`modernc.org/sqlite`** (pure Go, no CGO) | Static build + a simple Docker image; `mattn/go-sqlite3` would require CGO |
| T3 | A bundled **`cmd/demo-targets`** server for the demo targets (stdlib only) | The 200 / 500 / delayed targets stay fully local, no network dependency |
| T4 | Integration story **over the HTTP API** (`internal/` packages are not a public library) | Preserves the "one application" boundary; curl + fetch examples in the README |
| T5 | Git flow: a branch per phase → merged into `main` after acceptance; CI on `main` + PRs | A readable history; PR history as review-culture evidence |

---

## 2. Working arrangement (the same flow for every task)

1. **Delegation:** The task brief + acceptance criteria go to `deepseek-v4.1-flash` (implementer) via `delegate_task`.
2. **Mandatory evidence:** The implementer's output includes real `go vet ./...` and `go test -race ./...` output plus the checked acceptance list.
3. **Independent review:** The same diff + evidence goes to `glm-5.3-flash` as a read-only reviewer.
4. **Orchestrator acceptance:** I run the tests myself and verify behaviour by executing it; I never trust a "done" report alone.
5. **User synthesis:** Each task ends with a short acceptance synthesis + an evidence link. Phase boundaries get the user's approval.
6. `docs/decisions.md` is updated with the technical decisions made in that task.

---

## 3. Phases and tasks

> The tasks are deliberately small: each is a single delegation with crisp acceptance criteria.

### Phase 0 — Skeleton and channel

> **Status:** ✅ Complete (2026-10-18) — commits `b5bf8c1`, `60fc004`, `1db5081`. Implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE); orchestrator verification: build/vet/test -race and a smoke test re-run, exit 0.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T0.1 Decision log** | Initial content of `docs/decisions.md`: K1–K11 + T1–T5 records | Every entry has context/decision/rationale; an exact match with the user's choices |
| **T0.2 Skeleton** | `go.mod`, folder layout: `cmd/pulsecheck`, `cmd/demo-targets`, `internal/{config,checker,store,api,web}`, `Makefile`, `.gitignore`, compilable empty packages | `go build ./...` and `make test` succeed; the layout is clear enough to be depicted in the README |
| **T0.3 CI** | `.github/workflows/ci.yml`: `go vet`, `go test -race`, `go build` | A green check on push; the badge lands in the README |

### Phase 1 — Configuration

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE, 1 minor → resolved as D17); orchestrator verification: build/vet/test -race + gofmt + a live smoke test (200/500/8.0s, SIGINT → exit 0).

| Task | Content | Acceptance criteria |
|---|---|---|
| **T1.1 Config loader** | JSON config: `targets[]` (name, url, optional interval/timeout), `defaults` (interval=30s, timeout=5s, workers=4), `storage` (db_path, retention=24h), `server` (addr) | Tests: a valid file parses; **defaults are applied** (30s/5s/24h/4); invalid URL/empty name rejected; a per-target override wins |
| **T1.2 Example configurations** | `config.example.json` (realistic targets) + `config.demo.json` (local demo targets only) + `cmd/demo-targets` (a stdlib server: `/ok`→200, `/broken`→500, `/slow`→8s delay) | `demo-targets` serves all three behaviours locally; `config.demo.json` points at them; a test verifies this |

### Phase 2 — Checker: HTTP + concurrency

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash` (1 main round + 2 follow-up rounds); reviewer: `glm-5.3-flash` (main round APPROVE + one minor → saturation test added → delta review APPROVE). Evidence: `-race -count=200` saturation stress 200/200; full suite 3× green (implementer + orchestrator + reviewer independently).

| Task | Content | Acceptance criteria |
|---|---|---|
| **T2.1 Single check** | A `net/http` request: per-request `context.WithTimeout`, duration measurement, classification: `up / down / timeout / error` (+status code, error reason) | With httptest: 200→`up`, 500→`down`, timeout→`timeout`, connection error→`error`; the duration is recorded; no goroutine leaks |
| **T2.2 Worker pool + scheduler** | A fixed N workers, round-based queue; a **per-target in-flight lock** (K4: if busy, the round is recorded as `skipped`); `SIGINT/SIGTERM` → the scheduler stops, in-flight checks are cancelled via context, a timeout-bounded drain, the store closes | Tests: (a) **concurrency ≤ workers** measured on the server side; (b) **the delayed target does not block the others** — while the slow target runs, the other targets record within the same round; (c) when check duration > interval, a `skipped` record + reason is created; (d) shutdown: the cancelled check ends cleanly with a `context` error and `runtime.NumGoroutine` returns to baseline |
| **T2.3 Race verification** | All Phase 2 tests run under `-race` | `go test -race ./internal/checker/...` green; the output is ready for the README evidence |

### Phase 3 — SQLite storage

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE; 1 minor → D21 patch + 2 pin tests, orchestrator). Evidence: `CGO_ENABLED=0` build, `-race` 5× (2 reviewer + 3 implementer), `go mod verify`, a 400-row concurrent write test.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T3.1 Schema + store** | A `results` table: `id, target, started_at, outcome, status_code, duration_ms, error`; a `(target, started_at)` index; WAL mode; insert + query functions | Tests: write/read roundtrip; all outcomes including `skipped` are persisted; concurrent writes are `-race` clean |
| **T3.2 Retention** | A background cleaner (e.g. every 10 min): deletes rows older than 24h; stops cleanly at shutdown | Test: 25h-old records are deleted, fresh ones survive; the cleaner is cancellable |

### Phase 4 — API + status screen

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE; 2 housekeeping minors → orchestrator: the smoke cmd was deleted, the port got pinned in a test + the Atoi overflow was fixed). Live verification: `demo-slow` emits a skipped/timeout mix while the other targets flow uninterrupted — D4 visible.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T4.1 JSON API** | `GET /api/status` (per-target latest + a 24h summary), `GET /api/history/{name}?limit=N`; stdlib-only `net/http` (Go 1.22 pattern routing), correct error codes | httptest: the `/api/status` shape is right; unknown target → 404; `limit` works; bad paths → 404/405 |
| **T4.2 Status screen** | A single HTML page (`embed.FS`), dependency-free vanilla JS, polls `/api/status` every 5s; per-target status badge + duration + a coloured indicator of the last N results | Test: the page returns 200 with the expected elements; no external resources in the JS/HTML (works offline); the README screenshot will come from here |

### Phase 5 — Integration, Docker, README

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash` (wrote D24–D26 itself); reviewer: `glm-5.3-flash` (APPROVE; 2 minors → documented acceptance as D27). Orchestrator verification: SIGINT → exit 0 with ordered logs; `make demo`; `docker compose up --build` live + non-root `uid=10001` + a clean compose down. The README honesty audit (3 commands run verbatim) passed.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T5.1 main + graceful shutdown** | All parts come together; structured logging with `slog`; the order: `http.Server.Shutdown` + checker drain + store close | End to end: three demo targets visible in the panel via `make run`; after `Ctrl+C` the process exits cleanly and the logs show the shutdown steps |
| **T5.2 Docker** | A multi-stage `Dockerfile` (CGO=0 static binary, minimal image) + a `compose.yaml` (app + demo-targets) | The full demo runs via `docker compose up`; the image builds |
| **T5.3 README** | Quickstart (local + Docker), a config reference, the API docs + **integration examples** (curl, JS fetch), a decisions summary → `docs/decisions.md`, a CI badge + real `go test -race` output, the embedded demo | A fresh clone → works with copy-paste commands; the integration example verified against the live API |

### Phase 6 — Demo video

> **Status:** ✅ Complete (2026-10-18) — orchestrator work: a live `make demo` stack + 36 Chrome headless frames + ffmpeg → `assets/demo.mp4` (72s, ~64KB) + `assets/demo.png`; embedded in the README (D28). The video clearly shows the three target behaviours and the skipped records.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T6.1 ffmpeg discovery** | Check `ffmpeg -version`; if missing, suggest an install or fall back | Settled **before** setup (no surprises) |
| **T6.2 Video production** | Demo config + the status screen; scheduled screenshots → an MP4 via ffmpeg; alternative: a 45–60s user-recorded capture (we provide the script + steps) | A 45–90s MP4, ≤ ~10 MB, embedded in the README; **visible in the video:** a 200-returning target, a 500-returning target, a delayed target hitting the timeout; the moments showing the delayed target not blocking the others (the others keep updating) and the `skipped` records appearing |

### Phase 7 — Final acceptance

> **Status:** ✅ Complete (2026-10-18) — final whole-repo review: `glm-5.3-flash` (first round CHANGES REQUESTED: the CI reconciliation → corrected with real evidence + a D29 record → delta review APPROVE). Reviewer verdict: "the codebase itself is finished, honest, and thoroughly pinned". CI activation: the user granted the scope, `ci.yml` returned, the **first run is green** (run `39141315004`), the badge is in the README — K9 **full**.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T7.1 Full review** | The reviewer + the orchestrator do a final pass over the entire repo; the whole acceptance list is checked | All tests green under `-race`; CI green; README/decisions/PLAN up to date; the user gives the final approval |

---

## 4. Demo evidence matrix (traceability)

| Promise | Evidence |
|---|---|
| 200 / 500 / delayed targets | `config.demo.json` + `cmd/demo-targets` + the demo video |
| The delayed target does not block | The T2.2(b) test + live in the video |
| The concurrency bound | The T2.2(a) test (`-race`) + CI |
| Graceful shutdown | The T2.2(d) test + end-to-end observation in T5.1 |
| Retention/cleanup | The T3.1–T3.2 tests |
| Integration | Verified API examples in the README |
| Test culture | CI badge + real output in the README |

## 5. Cost

- **Infrastructure: €0.** A local SQLite file, local demo targets, the GitHub Actions free tier, ffmpeg locally.
- **Tokens (the only variable):** 7 phases × ~2–3 delegations; with the Flash models a rough estimate stays under a few euros total. Real token usage is reported in the syntheses at the end of each phase; if it surprises us, we stop and talk.

## 6. Definition of done

All acceptance criteria above checked + the demo video embedded in the README + CI green + the user's approval. `docs/PLAN.md` is updated with per-phase progress markers as work proceeds (☐ → ✅).

---

# v1.1 Roadmap (user-approved scoping, 2026-10-18)

**Scope decisions:** /metrics + uptime% + /healthz + sparkline detail page · TLS certificate tracking + richer HTTP check definitions · config fuzzing + benchmark package · release pipeline + golangci-lint + the v1.0.0 tag. The state-change webhook stays **out of scope** (the user reconfirmed the original boundary). Working arrangement unchanged (same model split, same quality gate).

## v1.1 Phase 1 — Richer checks (schema first; the base for the rest)

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash`; reviewer: `glm-5.3-flash` (APPROVE; header-injection and method-token defense verified EMPIRICALLY with live probes; 1 minor → deferred consciously as D30c). Orchestrator: added the combined expected_status+redirect pin test (the review's nit), full suite green.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T1.1-R Rich HTTP checks** | `targets[]` += `method` (default GET), `expected_status` (unset → up = 2xx; set → up = that code), `headers` (map), `follow_redirects` (default true). `checker.Check` update | Tests: expected_status honored (200→up, 500 configured→up on 500), method sent, headers sent, redirects follow/no-follow both classified; config validation for them |
| **T1.2-R TLS cert tracking** | For `https://` targets the response's `resp.TLS.PeerCertificates[0].NotAfter` is read (zero extra dialing) and stored | A delayed/expiring certificate (< 30 days) is visible in the API + amber in the panel; a `http://` target stores NULL |
| **T1.3-R Schema migration** | `results` += `cert_expires_at INTEGER` (nullable), added via an idempotent `ALTER TABLE` guarded by `PRAGMA table_info` | Reopen on an old DB migrates cleanly; new DBs are born with the column; `-race` tests prove migration + roundtrip |

## v1.1 Phase 2 — Observability

> **Status:** ✅ Complete (2026-10-18) — implementer: `deepseek-v4.1-flash` (their own live smoke caught + fixed a /healthz mount bug, pinned by main_test.go); reviewer: `glm-5.3-flash` (APPROVE; EXPLAIN QUERY PLAN index proof, empirical escaping/XSS probes, byte-stability; findings minor/nit only — no code change requested). Orchestrator: live smoke reproduced; D31 recorded. (T2.1-R's task row names `pulsecheck_check_results_total`; the shipped family is `pulsecheck_check_results_24h` — see D31 for why a lifetime counter was rejected.)

| Task | Content | Acceptance criteria |
|---|---|---|
| **T2.1-R `/metrics`** | Prometheus text exposition, stdlib-only: `pulsecheck_target_up{target}` (gauge), `pulsecheck_check_duration_seconds{target}` (last), `pulsecheck_check_results_total{target,outcome}` (from the store), `pulsecheck_target_cert_expiry_days{target}`, `pulsecheck_last_check_timestamp_seconds{target}`, `pulsecheck_info` (version) | Format-valid per the Prometheus text spec; scrape answered from store queries (documented trade-off); tests assert lines for every target |
| **T2.2-R Uptime %** | SQL-computed over the window; **`skipped` excluded from the denominator** (no check ran → not a failure) — recorded as D30 | `uptime_24h`/`uptime_7d` in `/api/status` per target; panel shows the percentages; tests with seeded mixes match hand-computed values |
| **T2.3-R `/healthz`** | `{ok, version, uptime_seconds, last_write_age_seconds}` (max(started_at) from the store) | 200 + correct shape; last_write_age grows over time; version defaults to `dev` |
| **T2.4-R Sparkline detail page** | `/targets/{name}`: an embedded single page with an SVG polyline of the last N durations (History limit 200); linked from the dashboard cards | 200 + an SVG element with polyline points for a seeded target; 404 for unknown names; zero external resources |

## v1.1 Phase 3 — Robustness

> **Status:** ✅ Complete (2026-10-18) — implementation + review + acceptance. 60s fuzz: 3,555,538 execs, 0 crashers; 4 benchmarks; CI Fuzz+Bench steps; the review confirmed the evidence (its own 30s fuzz + metrics runs), flagged the docs rider (consciously absorbed: D30/D31/D32 records are the orchestrator's) and two nits — both closed (combined-test had already landed; two new metric seeds added). D30/D31/D32 recorded.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T3.1-R Config fuzzing** | `FuzzParse` via stdlib `testing.F`: random JSON must never panic and always produce either a config or a non-empty error; seed corpus = the bundled configs | `go test -fuzz=FuzzParse -fuzztime 60s` locally green with no crashers; the regression corpus is committed; normal `go test` runs the seeds |
| **T3.2-R Benchmarks** | `BenchmarkCheck` (local httptest), `BenchmarkRecord`, `BenchmarkHistory` in checker + store; run in CI as evidence (not regression gates) | `go test -bench` outputs recorded in the README with a machine-dependence caveat; `-race`-free fast paths |
| **T3.3-R CI fuzz step** | A time-boxed `go test -fuzz=FuzzParse -fuzztime 30s` step in the CI workflow | Green in CI; a crasher would fail the run |

## v1.1 Phase 4 — Shipping

> **Status:** ✅ Complete (2026-10-18) — version injection (`make build VERSION=...`, `dev` default; startup log + `/healthz` + `/metrics` all report it, injected value verified live); golangci-lint v2.15.0 pinned with a curated, uncapped `.golangci.yml` as a separate CI job (all 30 errcheck findings fixed in code — explicit `_ =` drops of unactionable errors, each audited); tag-triggered release workflow (static linux/amd64+arm64 and darwin/arm64 tarballs, sha256 checksums, `-race` pre-publish guard, `gh release create`). Independent review: APPROVE (2 nits fixed — checksum `./` prefix, lint comment accuracy; 4 observations, none actionable). T4.4-R: `v1.0.0` tagged and released — [release page](https://github.com/kemselcuk/pulsecheck/releases/tag/v1.0.0), release run `39149068013` green. All three assets + `checksums.txt` downloaded and verified (`shasum -a 256 -c` → OK); the darwin/arm64 binary from the release page ran and reported `v1.0.0` via `/healthz`, `/metrics` and the startup log; the linux binaries are statically linked (x86-64 / aarch64).

| Task | Content | Acceptance criteria |
|---|---|---|
| **T4.1-R Version injection** | `main.version` var set via `-ldflags "-X main.version=..."`; `make build` defaults to `dev`; /healthz + /metrics + the startup log report it | A `-ldflags`-built binary prints the injected version; tests for the healthz shape |
| **T4.2-R golangci-lint in CI** | A separate lint job with a curated `.golangci.yml` (staticcheck + a small set; the current code must pass or be fixed) | A green lint job on push/PR; zero complaints on `main` |
| **T4.3-R Release pipeline** | On pushing a `v*` tag: build static binaries for linux/amd64, linux/arm64, darwin/arm64 (+checksums) → publish a GitHub Release (€0) | A real tag produces a real release page with downloadable binaries; the pipeline file is in the repo |
| **T4.4-R v1.0.0 tag** | After everything above is green: tag `v1.0.0` (the first release carries all v1.1 features) | The release page live; the binaries are downloadable and run |

## v1.1 Phase 5 — Docs + final review

> **Status:** ✅ Complete (2026-10-18) — README rewritten for the full v1.1 surface (rich config fields, `/metrics`, `/healthz`, uptime, certificate tracking, detail pages, fuzzing/benchmarks, the `v1.0.0` release flow, local version injection); every console block verified verbatim against real runs. D36 records the acceptance reconciliation (cert_expires_at in the JSON API, uptime + certificate lines on the dashboard, amber below 30 days on both pages) — independently verified by the reviewer in a rendered DOM via headless Chrome, including all three threshold states. Final review: APPROVE (1 minor + 1 nit fixed: `method` added to the example config, sample-run note added; 2 observations closed: the T2.1-R naming footnote above and the exact CI trigger wording). The user's final approval is the last step.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T5.1-R Docs** | README: new config fields, /metrics, /healthz, uptime%, cert tracking, the detail page, fuzzing/benchmarks, the release; decisions D30+; PLAN markers | Every documented command is verified; no invented outputs |
| **T5.2-R Final review** | A whole-repo delta review (reviewer + orchestrator) | All v1.1 acceptance criteria checked; the user's final approval |

**Cost:** infrastructure €0 (GitHub Actions free tier, local everything). Tokens reported per phase.

---

# Post-v1.1 — Quick wins (user-approved 2026-10-18)

> **Status:** ✅ Complete (2026-10-18/19) — all eight items implemented and verified (live compose healthcheck, before/after second-signal evidence, dependency refresh with green gates, honesty-checked docs). Independent review: APPROVE (1 nit fixed — `start_interval` on the healthcheck, verified live: healthy in ~4 s; 2 observations, none actionable; every claimed number re-measured by the reviewer with its own runs).

| Item | Content | Evidence |
|---|---|---|
| Compose healthcheck | The `pulsecheck` service probes its own `/healthz` via `wget` (30 s interval, 5 s start interval, 3 retries) | `docker compose up --build -d` → container **healthy after ~5 s** (first live run; ~4 s after the review nit); the healthcheck log shows the real `/healthz` JSON |
| `-version` flag | `pulsecheck <version>` prints the injected build version and exits 0 | `make build VERSION=v1.0.0 && ./bin/pulsecheck -version` → `pulsecheck v1.0.0`; pinned by `TestVersionOutput` |
| Second-signal force exit (D27 closure) | `stop()` after the first signal restores the default disposition | slowloris-held drain: old binary exit 0 after 4.7 s vs. new binary killed (−2) 0.7 s after the first signal |
| Repo visibility | 9 GitHub topics added; private vulnerability reporting enabled | `gh api` reads back both |
| Dependency hygiene | Dependabot (gomod + actions, weekly); `go get -u` refreshed 4 modules in the SQLite driver's graph | `go.mod`/`go.sum` diff; full race suite green after the bump |
| Developer ergonomics | `make fmt`, `make cover`; the honest coverage figure in the README | `make cover` → **81.9%** total statements; `internal/config` 100% |
| Community files | `SECURITY.md` (private reporting), `CONTRIBUTING.md` (gates, evidence, decision-log expectations) | files in the repo |
| Demo media refresh | `demo.png` + the 72 s video regenerated on the v1.1 UI (uptime lines; the clip ends on `demo-slow`'s detail page with its sparkline) | 36 frames via the D28 pipeline; ffprobe: 72.0 s, ~110 KB |

---

# v1.2 Phase 1 — Body assertions + duration histogram (user-approved 2026-10-19)

> **Status:** ✅ Complete (2026-10-19) — implementation + independent review (APPROVE; the review live-reproduced every headline claim). Findings closed: 1 minor (the `Stats` doc comment now scopes the skipped-exclusion to the histogram/percentile fields — `AvgDurationMs` keeps its historical all-rows semantics), 1 nit (the metrics structural test now requires the declared base family to be a `histogram` before accepting `_bucket`/`_sum`/`_count` suffixes), 2 observations (the keep-alive comment is scoped to bodies within the cap; a TLS × body-mismatch pin proves the certificate is recorded on a failing assertion).

| Task | Content | Evidence |
|---|---|---|
| **T1.1 Body assertions** | `expect_body_contains` / `expect_body_regex` per target (mutually exclusive, regex pre-compiled, rejected on HEAD): the body rule is judged only on accepted statuses; a mismatch → `down` with a describing `error`; the first 64 KiB are read (D38) | Live run: `body-mismatch` → `down`/HTTP 200/`body does not contain "maintenance"`, regex match → `up`, regex mismatch → `down`/`body does not match regexp "^ok$"`; 5 checker tests (contains/regex/cap/read-error/read-timeout) + config table tests + fuzz seeds/invariants |
| **T1.2 Duration histogram + percentiles** | Windowed classic-bucket histogram of completed checks (skipped excluded) as `pulsecheck_check_duration_seconds_24h` (`_bucket`/`_sum`/`_count`); `p50_duration_ms`/`p95_duration_ms` bucket-ceiling estimates in `/api/status` (null when not expressible) (D39) | Live `/metrics` capture (`demo-slow`'s 4 timeouts in `le="10"`, sum 20.010 s) + `/api/status` (`p50: 10000, p95: 10000` for the timeout ceiling, `25/100` for the TLS target); store tests pin inclusive boundaries, cumulative counts, the sum, and both percentile paths incl. the open-bucket null; metrics/api tests updated |

---

# v1.2 Phase 2 — Batched status snapshot + scale proof (user-approved 2026-10-19)

> **Status:** ✅ Complete (2026-10-19) — implementation + independent review (APPROVE; the reviewer reproduced the benchmark shape, adversarially falsified the equivalence with boundary/odd seeds, and confirmed the index usage via `EXPLAIN QUERY PLAN`). Findings closed: 2 minor (the merged-aggregate phase is now a committed sub-benchmark so its profile stays reproducible, and three code comments no longer claim target-count independence) and 1 nit (the speedup band widened to ~1.5-1.8×).

| Task | Content | Evidence |
|---|---|---|
| **T2.1 Batched snapshot** | `store.StatusSnapshot`: one merged index-range scan per target for every window aggregate (counts, buckets, both uptimes, average with conditional-aggregate 24 h figures), plus bounded index reads for the newest result and the recent strip; `/api/status` and `/metrics` rewired to it (D40) | Equivalence with the per-target primitives pinned field by field (`TestStatusSnapshotMatchesPerTargetQueries`, plus the empty/zero-shape pin); the full suite and the metrics/api golden-style tests pass unchanged on the new path |
| **T2.2 Scale proof** | `BenchmarkStatusReadPaths` at 200 targets × 24 h of 30 s rows (~576k rows): original per-target path vs the snapshot vs the merged aggregate phase alone, with the measured-bad five-statement full-scan variant recorded (D40) | Benchmark, 2 samples each: **snapshot ~0.64-0.65 s vs original ~1.0 s**; the merged phase alone ~0.67-0.79 s at ~23k allocs; the naive full-scan batching measured ~1.8 s — the slower design is documented with its numbers instead of shipped |

---

# v1.2 Phase 3 — Hot reload (SIGHUP) (user-approved 2026-10-19)

> **Status:** ✅ Complete (2026-10-19) — implementation + independent review (APPROVE; the reviewer reproduced the full live scenario with its own process and pid checks, and re-ran the reload test under `-count=3 -race`). Findings closed: 2 nits (the reload test now also asserts the in-flight skip directly, and `TestReloadConfig` covers all four restart-required fields in a table) and 3 observations consciously kept (swap-before-schedule ordering, the documented theoretical `wg` window, and the per-request O(n) name lookups over config-sized lists).

| Task | Content | Evidence |
|---|---|---|
| **T3.1 Reload plumbing** | `config.Atomic` holder (read per request by api/metrics/web); the SIGHUP loop in the lifecycle select; strict policy — reloadable is the resolved target list, `server`/`storage`/`defaults.workers` and parse failures are rejected with the running configuration intact; the log summarizes added/removed/updated (D41) | Live: same-pid swap `[alpha,beta]` → `[alpha,gamma]` with `uptime_seconds` growing across it, the `config reloaded added=[gamma] removed=[beta] updated=[alpha]` line, and a rejected `workers` change while the service kept serving; `TestReloadConfig`, `TestDiffTargets`, `TestAtomic*` |
| **T3.2 Scheduler swap** | Name-keyed `targetRunner`s: removed targets' loops stop without interrupting their in-flight check, updated targets carry the in-flight flag onto the replacement loop, unchanged targets keep running, added targets check immediately (D41) | `TestSchedulerReload`: server-side max concurrency stays 1 across an in-flight update; added/removed/unchanged behaviour verified; full `-race` suite green |

---

# v1.2 Phase 4 — TLS options: custom CA and opt-out verification (user-approved 2026-10-19)

> **Status:** ✅ Implementation complete (2026-10-19); independent review and merge follow.

| Task | Content | Acceptance criteria |
|---|---|---|
| **T4.1 Per-target TLS** | `targets[].tls.ca_file` (PEM file, additive to the system roots, read once at load with a 1 MiB cap) and `targets[].tls.insecure_skip_verify` (bool, default false); the derived per-target client carries the TLS settings on both the redirecting and the non-following path | A target whose CA exists only in a local PEM verifies successfully; `insecure_skip_verify` connects to a self-signed host; without either option a self-signed host stays an error; a missing/invalid PEM file is rejected at load with a `config:` error; config PEM cases + scheduler client derivation tested; the existing TLS/certificate tracking is unaffected |

# v1.2 Phase 5 — pprof, opt-in (user-approved 2026-10-19)

> **Status:** ☐ Pending

| Task | Content | Acceptance criteria |
|---|---|---|
| **T5.1 pprof server** | A `-pprof <addr>` flag (default off) serving a DEDICATED `/debug/pprof` mux (not the app mux, not `DefaultServeMux`); starts with the process and drains on shutdown; a bind failure is a startup error; the startup log warns that the endpoint is unauthenticated | Disabled by default (no listener); when enabled, `/debug/pprof/` and the profile endpoints respond; the dedicated mux is unit-tested; live curl evidence; README documents the flag and the localhost recommendation (D43) |

# v1.2 Phase 6 — Release hardening: Windows + provenance (user-approved 2026-10-19)

> **Status:** ☐ Pending

| Task | Content | Acceptance criteria |
|---|---|---|
| **T6.1 Windows asset** | `windows/amd64` added to the release matrix (a `.exe` inside the archive) | The real release carries four platform archives + checksums |
| **T6.2 Provenance attestation** | `actions/attest-build-provenance` over the release artifacts, with the required `id-token`/`attestations` permissions | `gh attestation verify` succeeds for a downloaded asset from the real release |
| **T6.3 Next release (v1.1.0)** | Tag the accumulated feature set (body assertions, duration histogram, batched snapshot, hot reload) to exercise the hardened pipeline end to end | A real release page with four downloadable assets, verified checksums and a verified attestation |

# v1.2 Phase 7 — Nightly soak (user-approved 2026-10-19)

> **Status:** ☐ Pending

| Task | Content | Acceptance criteria |
|---|---|---|
| **T7.1 Soak test** | A `SOAK`-gated test (skipped in normal runs) that runs the real stack — scheduler + store + retention — for `SOAK_DURATION` (default 3m) with live reloads, slow targets and pruning, asserting: rows stay bounded by retention, goroutines return to baseline after shutdown, `-race` clean | Locally: a short run green, and the bound assertion demonstrably fails when retention is effectively off (a manual mutation run) |
| **T7.2 Nightly workflow** | `.github/workflows/soak.yml`: a nightly cron + manual dispatch, `SOAK_DURATION=3m go test -race -timeout 15m` | The workflow file is valid and a dispatch run is green |
