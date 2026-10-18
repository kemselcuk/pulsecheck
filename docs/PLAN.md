# PulseCheck — Implementation Plan

**Status:** ✅ Approved and implemented (2026-10-18) · Phases 0–6 complete; the single open item was CI activation, now resolved (see D29).
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
