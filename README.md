<img src="assets/icon.svg" width="64" height="64" alt="">

# PulseCheck

[![CI](https://github.com/kemselcuk/pulsecheck/actions/workflows/ci.yml/badge.svg)](https://github.com/kemselcuk/pulsecheck/actions/workflows/ci.yml)

PulseCheck is a single Go application that checks HTTP targets from a JSON configuration at configurable intervals, stores the results in a local SQLite file, and serves a small JSON API plus a single-page status dashboard. Checks run concurrently through a bounded worker pool; a delayed target never blocks the others, an overlapping round on the same target is recorded as `skipped`, and the process exits cleanly on `SIGINT`/`SIGTERM`. A Prometheus text endpoint (`/metrics`), a liveness probe (`/healthz`) with build metadata, and per-target detail pages (uptime, TLS certificate expiry, duration sparkline) round out the observability story.

The project was written to demonstrate **HTTP, concurrency and error handling** in Go through a real, small service: standard library only, plus a single pure-Go SQLite driver. Every meaningful decision is recorded with its rationale in `docs/decisions.md`.

## Features

- **Bounded worker pool:** the number of concurrent checks is structurally limited by `defaults.workers`.
- **Per-request context timeout:** every check is bounded by `context.WithTimeout`; there is exactly one timeout mechanism.
- **Overlapping rounds are `skipped`:** while the previous check of a target is still running, the new round is skipped and recorded together with its reason.
- **Rich check definitions:** per target — `method`, `expected_status` (only that code counts as `up`), request `headers`, and a `follow_redirects` switch.
- **Body assertions:** `expect_body_contains` / `expect_body_regex` — a status-200 error page is not a pass; a mismatch is classified `down` with the reason in `error`, and only the first 64 KiB of the body are searched (D38).
- **Duration histogram:** a windowed histogram of completed-check durations in `/metrics` (classic buckets, 5 ms to 10 s) plus `p50_duration_ms` / `p95_duration_ms` bucket-ceiling estimates in `/api/status` (D39).
- **24-hour retention:** old results are pruned periodically in the background (configurable).
- **Single-page dashboard:** embedded HTML/CSS/JS; no external resources, works offline, polls `/api/status` every 5 seconds; each card links to its target's detail page.
- **Per-target detail pages:** `/targets/{name}` — server-rendered, no JavaScript: latest outcome, 24 h/7 d uptime figures, TLS certificate expiry (amber below 30 days) and an inline SVG sparkline of the most recent durations.
- **Prometheus metrics:** `/metrics` in the classic text exposition format, answered from live store queries; windowed gauges, absent series for unknown values (decision D31).
- **Liveness endpoint:** `/healthz` reports `ok`, the build version, process uptime, and how fresh the newest stored check is.
- **Uptime percentages:** 24-hour and 7-day uptime per target; `skipped` rounds are excluded from the denominator (a round where no check ran is not an outage).
- **TLS certificate tracking:** for `https://` targets the leaf certificate's expiry is read from the response (`resp.TLS`) with no extra dialing and stored with every check.
- **Zero-downtime config reload:** `SIGHUP` re-reads the configuration file and swaps the target list in place — added, removed and updated targets, in-flight checks never interrupted; `server`, `storage` or `defaults.workers` changes are rejected with a clear log line and need a restart (D41).
- **Graceful shutdown:** `SIGINT`/`SIGTERM` → the checker stops, HTTP drains, the database closes; the log lines show the ordered steps.
- **Fuzzing and benchmarks:** the config parser has a native fuzz target; CI runs a 30 s fuzz step and a benchmark evidence step on every push.

## Quick start

### 1) Local (single command, no second terminal)

```console
$ make demo
```

The `demo` target builds both binaries, starts the `cmd/demo-targets` server in the background, runs `pulsecheck` in the foreground, and kills the background process via `trap` on exit. Then open **http://localhost:8080** in a browser: three targets appear —

- `demo-ok` → **up** (HTTP 200),
- `demo-broken` → **down** (HTTP 500),
- `demo-slow` → **timeout**/`skipped` (a target delayed by 8s, exceeding the 5s timeout).

Press `Ctrl+C` to stop.

To run only the service (the default config is `config.demo.json`; because the `-config` path is relative to the working directory, run from the repository root):

```console
$ go run ./cmd/pulsecheck
# or
$ make run
# with another configuration:
$ go run ./cmd/pulsecheck -config path/config.json
```

### 2) Docker Compose

```console
$ docker compose up --build
```

The same demo comes up in two containers: `demo-targets` (reachable only inside the compose network, `:18081`) and `pulsecheck` (`0.0.0.0:8080`). The dashboard is again at **http://localhost:8080**; the targets point at `http://demo-targets:18081/...`.

## Releases

Prebuilt binaries are published from git tags by [`release.yml`](.github/workflows/release.yml): static builds (`CGO_ENABLED=0`), `-trimpath`, the version injected from the tag name, with a sha256 `checksums.txt` alongside. The first release is **v1.0.0**:

| Asset | Platform |
|---|---|
| `pulsecheck_v1.0.0_linux_amd64.tar.gz` | Linux, x86-64 (static) |
| `pulsecheck_v1.0.0_linux_arm64.tar.gz` | Linux, arm64 (static) |
| `pulsecheck_v1.0.0_darwin_arm64.tar.gz` | macOS, Apple silicon |

```console
$ gh release download v1.0.0 --repo kemselcuk/pulsecheck --dir pulsecheck-v1.0.0
$ cd pulsecheck-v1.0.0
$ shasum -a 256 -c checksums.txt        # sha256sum on Linux
pulsecheck_v1.0.0_darwin_arm64.tar.gz: OK
pulsecheck_v1.0.0_linux_amd64.tar.gz: OK
pulsecheck_v1.0.0_linux_arm64.tar.gz: OK
$ tar -xzf pulsecheck_v1.0.0_darwin_arm64.tar.gz
$ ./pulsecheck -config path/to/config.json        # any PulseCheck config file
```

Local builds inject the version explicitly — `make build` defaults to `dev`, and the value flows into the startup log, `/healthz` and `/metrics`:

```console
$ make build VERSION=v1.0.0
$ ./bin/pulsecheck -config config.example.json &
$ curl -s localhost:8080/healthz
{"ok":true,"version":"v1.0.0","uptime_seconds":25.032,"last_write_age_seconds":0.031}
$ ./bin/pulsecheck -version
pulsecheck v1.0.0
```

## Configuration

Configuration is JSON, passed via the `-config` flag (default `config.demo.json`). Every field is optional; any omitted field falls back to its default, and every target carries concrete `interval`/`timeout` values after `Load`. Durations are Go duration strings (`"10s"`, `"500ms"`, `"24h"`). For a realistic example see [`config.example.json`](config.example.json).

| Field | Default | Description |
|---|---|---|
| `server.addr` | `":8080"` | HTTP listen address |
| `storage.db_path` | `"pulsecheck.db"` | SQLite file; missing parent directories are created |
| `storage.retention` | `"24h"` | Results older than this are pruned in the background |
| `defaults.interval` | `"30s"` | Default check interval |
| `defaults.timeout` | `"5s"` | Default request timeout |
| `defaults.workers` | `4` | Worker pool size (> 0) |
| `targets[].name` | — (**required**) | Target name; must be unique |
| `targets[].url` | — (**required**) | Absolute `http`/`https` URL |
| `targets[].interval` | `defaults.interval` | Per-target interval |
| `targets[].timeout` | `defaults.timeout` | Per-target timeout |
| `targets[].method` | `GET` | HTTP method (a valid method token) |
| `targets[].expected_status` | unset | When set, the target is `up` iff the *final* response has this code; unset → any 2xx is `up` |
| `targets[].headers` | `{}` | Extra request headers; names that collide after trimming are rejected (decision D32b) |
| `targets[].follow_redirects` | `true` | Follow the redirect chain before judging the final status |
| `targets[].expect_body_contains` | unset | The target is `up` only if the body (first 64 KiB) contains this substring; a mismatch is `down` with the reason in `error` |
| `targets[].expect_body_regex` | unset | Same, with a Go regular expression compiled at load time; mutually exclusive with `expect_body_contains`, and both are rejected on `HEAD` targets |

A richer definition (every field below is demonstrated in `config.example.json`):

```json
{
  "targets": [
    { "name": "httpbin-robots", "url": "https://httpbin.org/robots.txt",
      "expect_body_contains": "Disallow: /deny", "interval": "120s" },
    { "name": "httpbin-204", "url": "https://httpbin.org/status/204",
      "expected_status": 204, "interval": "60s" },
    { "name": "httpbin-post", "url": "https://httpbin.org/post",
      "method": "POST", "expected_status": 200, "interval": "120s" },
    { "name": "httpbin-legacy-redirect", "url": "https://httpbin.org/redirect-to?url=/status/200&status_code=301",
      "follow_redirects": false, "expected_status": 301 }
  ]
}
```

With `expected_status: 500` a maintenance endpoint that answers 500 counts as `up`; with `follow_redirects: false` the 301 itself is the final status; with a body assertion an accepted status must satisfy the body rule too — only the first 64 KiB of the body are searched, and a status-200 error page is classified `down` with `body does not contain "…"` in `error` (D38). For `https://` targets the leaf certificate's expiry is recorded with every check; `http://` targets store none.

Validation is strict: unknown fields, empty or duplicate target names, non-absolute URLs, non-positive durations/worker counts, invalid methods, and header names that collide after whitespace trimming are all rejected. Try it:

```console
$ go run ./cmd/pulsecheck -config missing.json
time=... level=ERROR msg="failed to load config" path=missing.json err="config: open missing.json: no such file or directory"
$ echo $?
1
```

### Reloading without a restart

Editing the file has no effect until the process is told to re-read it — send `SIGHUP`:

```console
$ kill -HUP $(pgrep -f 'pulsecheck$')

# the log reports exactly what changed:
time=... level=INFO msg="config reloaded" added=[gamma] removed=[beta] updated=[alpha]

# a change that needs a restart is rejected loudly; the running configuration stays:
time=... level=ERROR msg="config reload: rejected, keeping the running configuration" err="defaults.workers changed (4 -> 8): restart required"
```

The target list — including per-target intervals, timeouts and check definitions — is swapped in place: the process keeps its pid, in-flight checks are never interrupted, and a removed target's last check still records. `server`, `storage` and `defaults.workers` cannot change at runtime; restart for those (D41).

`config.docker.json` is an exact copy of `config.demo.json` except that the target URLs point at the compose service name (`http://demo-targets:18081/...`).

## API

Read-only endpoints; use only `net/http` (Go 1.22 pattern routing) and write nothing except through the store-backed handlers below.

| Endpoint | Description |
|---|---|
| `GET /api/status` | Per target: the latest result (`last`, with `cert_expires_at` for TLS targets), the recent outcome strip (`recent`), 24-hour counters (`stats`, including the `p50_duration_ms`/`p95_duration_ms` bucket-ceiling estimates), and `uptime_24h`/`uptime_7d` percentages (`null` when the window holds no completed check; `skipped` never counts against uptime) |
| `GET /api/history/{name}?limit=N` | A single target's most recent results, newest first; `limit` defaults to 50, capped at 500; unknown name → 404 |
| `GET /healthz` | Liveness: `ok`, `version`, `uptime_seconds`, `last_write_age_seconds` (`null` when no check is stored yet) |
| `GET /metrics` | Prometheus text exposition (see below) |
| `GET /` | The dashboard: the embedded single-page status screen; polls `/api/status` every 5 seconds and links every card to its detail page |
| `GET /targets/{name}` | Per-target detail page (server-rendered HTML): uptime figures, certificate expiry, duration sparkline; unknown name → 404 |

Examples from a real local run (the bundled demo config plus one ad-hoc `demo-tls` target pointing at `https://example.com` so the certificate surfaces are exercised):

```console
$ curl -s http://localhost:8080/api/status
{"checked_at":"2026-10-18T22:22:57Z","targets":[{"name":"demo-ok","url":"http://localhost:18081/ok","last":{"outcome":"up","status_code":200,"duration_ms":1,"at":"2026-10-18T22:22:57Z","error":"","cert_expires_at":null},"recent":["up","up","up","up","up","up"],"stats":{"up":6,"down":0,"timeout":0,"error":0,"skipped":0,"avg_duration_ms":1,"p50_duration_ms":5,"p95_duration_ms":5},"uptime_24h":100,"uptime_7d":100},{"name":"demo-broken","url":"http://localhost:18081/broken","last":{"outcome":"down","status_code":500,"duration_ms":0,"at":"2026-10-18T22:22:57Z","error":"","cert_expires_at":null},"recent":["down","down","down","down","down","down"],"stats":{"up":0,"down":6,"timeout":0,"error":0,"skipped":0,"avg_duration_ms":1,"p50_duration_ms":5,"p95_duration_ms":5},"uptime_24h":0,"uptime_7d":0},{"name":"demo-slow","url":"http://localhost:18081/slow","last":{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T22:22:53Z","error":"previous check still in flight","cert_expires_at":null},"recent":["skipped","timeout","skipped","timeout","skipped","timeout","skipped","timeout"],"stats":{"up":0,"down":0,"timeout":4,"error":0,"skipped":4,"avg_duration_ms":2501,"p50_duration_ms":10000,"p95_duration_ms":10000},"uptime_24h":0,"uptime_7d":0},{"name":"demo-tls","url":"https://example.com","last":{"outcome":"up","status_code":200,"duration_ms":18,"at":"2026-10-18T22:22:47Z","error":"","cert_expires_at":"2026-12-25T22:56:35Z"},"recent":["up","up"],"stats":{"up":2,"down":0,"timeout":0,"error":0,"skipped":0,"avg_duration_ms":53,"p50_duration_ms":25,"p95_duration_ms":100},"uptime_24h":100,"uptime_7d":100}]}
```

```console
$ curl -s "http://localhost:8080/api/history/demo-slow?limit=3"
[{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T17:47:40Z","error":"previous check still in flight","cert_expires_at":null},{"outcome":"timeout","status_code":0,"duration_ms":5000,"at":"2026-10-18T17:47:37Z","error":"Get \"http://localhost:18081/slow\": context deadline exceeded","cert_expires_at":null},{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T17:47:34Z","error":"previous check still in flight","cert_expires_at":null}]

$ curl -s -o /dev/null -w "%{http_code}\n" "http://localhost:8080/api/history/unknown"
404
```

### Observability endpoints

```console
$ curl -s http://localhost:8080/healthz
{"ok":true,"version":"v1.0.0","uptime_seconds":25.032,"last_write_age_seconds":0.031}
```

```console
$ curl -s http://localhost:8080/metrics | head -11
# HELP pulsecheck_info Build information for the running PulseCheck process; always 1.
# TYPE pulsecheck_info gauge
pulsecheck_info{version="v1.0.0"} 1
# HELP pulsecheck_target_up Whether the target's most recent check was up (1) or not (0); a target that has never been checked reports 0.
# TYPE pulsecheck_target_up gauge
pulsecheck_target_up{target="demo-ok"} 1
pulsecheck_target_up{target="demo-broken"} 0
pulsecheck_target_up{target="demo-slow"} 0
pulsecheck_target_up{target="demo-tls"} 1
# HELP pulsecheck_check_duration_seconds Duration of the target's most recent check in seconds; absent until the first check.

$ curl -s http://localhost:8080/metrics | grep '^pulsecheck_target_cert_expiry_days'
pulsecheck_target_cert_expiry_days{target="demo-tls"} 68
```

The full exposition carries seven metric families: `pulsecheck_info`, `pulsecheck_target_up`, `pulsecheck_check_duration_seconds`, `pulsecheck_check_results_24h` (a **windowed gauge**, not a counter — retention deletes rows, so a lifetime counter would decrease and mislead PromQL), `pulsecheck_target_cert_expiry_days`, `pulsecheck_last_check_timestamp_seconds` and `pulsecheck_check_duration_seconds_24h` — a **windowed histogram** of completed-check durations (classic cumulative buckets, 5 ms–10 s + `+Inf`, with `_sum`/`_count`; `skipped` rounds are excluded). Unknown values are absent series, never invented zeros; a target that has never been checked reports `target_up 0` (D31) — the histogram is the deliberate exception: a bucket count of 0 is a real measurement, so it is emitted with zeros.

```console
$ curl -s http://localhost:8080/metrics | grep '^pulsecheck_check_duration_seconds_24h' | grep 'target="demo-slow"'
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.005"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.01"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.025"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.05"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.1"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.25"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="0.5"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="1"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="2.5"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="5"} 0
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="10"} 4
pulsecheck_check_duration_seconds_24h_bucket{target="demo-slow",le="+Inf"} 4
pulsecheck_check_duration_seconds_24h_sum{target="demo-slow"} 20.010
pulsecheck_check_duration_seconds_24h_count{target="demo-slow"} 4
```

The `p50`/`p95` figures in `/api/status` are estimates derived from exactly these buckets: the upper bound of the bucket containing the rank — a documented ceiling, not an interpolated quantile (D39).

```console
$ curl -s http://localhost:8080/targets/demo-tls | grep -o 'cert expires in: <b>[^<]*'
cert expires in: <b>68 days
```

The detail page shows the same figures as the dashboard card plus the sparkline; below 30 days the certificate line (and the dashboard card's certificate text) turns amber. The pages are fully self-contained: they render offline, with zero external resources.

## Integrating with another project

The PulseCheck packages live under `internal/` (not a public library), so integration happens **over the HTTP API**.

**(a) Periodic reachability check with curl (cron example).** If `/api/status` is unreachable, PulseCheck is down:

```sh
# crontab: verify reachability every 5 minutes
*/5 * * * * curl -fsS http://localhost:8080/api/status >/dev/null || echo "PulseCheck unreachable" >> /var/log/pulsecheck-alarm.log
```

If `jq` is available, filtering only the failing targets is easy:

```sh
curl -fsS http://localhost:8080/api/status \
  | jq -r '.targets[] | select(.last.outcome != "up") | "\(.name) -> \(.last.outcome)"'
```

**(b) Inside a browser/JS app with `fetch`:**

```js
fetch('/api/status')
  .then((res) => res.json())
  .then(({ targets }) =>
    targets.forEach((t) => console.log(t.name, t.last ? t.last.outcome : 'pending')));
```

**(c) Response schema (`/api/status`):**

```json
{
  "checked_at": "2026-10-18T17:47:44Z",
  "targets": [
    {
      "name": "demo-ok",
      "url": "http://localhost:18081/ok",
      "last": {
        "outcome": "up",
        "status_code": 200,
        "duration_ms": 2,
        "at": "2026-10-18T17:47:44Z",
        "error": "",
        "cert_expires_at": null
      },
      "recent": ["up", "up", "up", "up", "up", "up"],
      "stats": { "up": 6, "down": 0, "timeout": 0, "error": 0, "skipped": 0, "avg_duration_ms": 1, "p50_duration_ms": 5, "p95_duration_ms": 5 },
      "uptime_24h": 100,
      "uptime_7d": 100
    }
  ]
}
```

`outcome` values: `up`, `down`, `timeout`, `error`, `skipped` — a failed body assertion is a `down` whose `error` explains the mismatch. `p50_duration_ms`/`p95_duration_ms` are the bucket-ceiling estimates described above (`null` when not expressible). `/api/history/{name}` returns an array carrying the same `outcome`/`status_code`/`duration_ms`/`at`/`error`/`cert_expires_at` fields (`at` is RFC3339 UTC; `cert_expires_at` is RFC3339 UTC or `null`).

## Architecture and decisions

### Package map

| Package | Responsibility |
|---|---|
| `cmd/pulsecheck` | The `main` that wires everything together; process lifecycle, graceful shutdown and version injection |
| `cmd/demo-targets` | Local demo server built on `net/http`: `/ok` → 200, `/broken` → 500, `/slow` → delayed |
| `internal/config` | Reads the JSON configuration, applies defaults, validates |
| `internal/checker` | Single HTTP check + scheduler with a bounded worker pool |
| `internal/store` | SQLite persistence (record, history, stats, uptime, certificate expiry) and periodic cleanup |
| `internal/api` | Read-only JSON API (`/api/status`, `/api/history/{name}`) and `/healthz` |
| `internal/metrics` | Prometheus text exposition at `/metrics` |
| `internal/web` | The single-page status dashboard and the per-target detail pages, embedded via `embed.FS` |

### Why these decisions?

- **One timeout mechanism — the context.** `checker.Check` never sets `http.Client.Timeout`; the only deadline comes from the per-request `context.WithTimeout`. "Check timed out" and "shutdown cancellation" are therefore the same mechanism: at shutdown, in-flight checks are aborted immediately and there is no confusing double-deadline interaction.
- **Per-target atomic in-flight lock.** Every round is claimed via `atomic.Bool.CompareAndSwap`; if the previous check is still running, the round is recorded as `skipped` with its reason instead of blocking. The queue never grows, and a delayed target never makes the others wait.
- **The `Recorder` seam.** The `checker` package does not depend on `internal/store`; it hands results to a `Recorder` interface. The store implements that interface (compile-time proof: `var _ checker.Recorder = (*store.Store)(nil)`), and tests use `MemoryRecorder`. This makes it possible to test the scheduler without a real database.
- **`modernc.org/sqlite` (pure Go).** No CGO required: it compiles statically with `CGO_ENABLED=0`, a small Alpine image is enough, and cross-compilation just works — the three release targets build on a single runner.
- **Metrics are windowed gauges, not counters.** Retention deletes old rows, so a process-lifetime `_total` counter would decrease and mislead PromQL; the 24-hour result counts are therefore a gauge, and unknown values are excluded as absent series instead of invented zeros (D31).
- **Poll cost is rows visited, not statement count.** The dashboard read path was benchmarked at 200 targets: batching statements into full-history scans measured SLOWER than index-range reads, so the shipped `StatusSnapshot` merges every window aggregate into one index-range scan per target (D40) — measurement first, micro-optimizations second.
- **The tag is the release.** The version is one `-ldflags` value baked from the git tag (local builds honestly say `dev`); the release pipeline stays on runner-provided tooling (`go`, `tar`, `gh`) and re-runs the `-race` suite as a pre-publish guard (D33–D35).
- **Dependency discipline.** The application code uses only the standard library plus a single pure-Go SQLite driver; JSON was chosen over YAML for configuration, so no extra dependency was born.

Full decision log: [`docs/decisions.md`](docs/decisions.md) · Implementation plan: [`docs/PLAN.md`](docs/PLAN.md).

## Tests

```console
$ make test        # go test -race ./...
$ make vet         # go vet ./...
$ make lint        # golangci-lint run with the repository's .golangci.yml
$ make fmt         # gofmt -w .
$ make cover       # race suite with coverage; prints the statement total
$ go test -fuzz=FuzzParse -fuzztime 60s ./internal/config/    # native fuzzing
$ go test -bench . -benchtime 200x -run '^$' ./internal/checker/ ./internal/store/ -benchmem
```

CI ([`ci.yml`](.github/workflows/ci.yml)) runs four gates on every push to `main` (and to `faz-*` phase branches) and every pull request: `go vet` + `go test -race`, a 30 s fuzz step, a benchmark evidence step, and a separate `lint` job (golangci-lint pinned to v2.15.0, uncapped reporting, zero complaints). The [`release.yml`](.github/workflows/release.yml) pipeline re-runs the race suite as a guard before publishing any tag. Statement coverage (measured with `make cover`, 2026-10-19): **81.9%** total — `internal/config` sits at 100%, while the two `cmd` packages stay lower on purpose: they are thin wiring around tested packages.

Which test proves which promise:

| Promise | Test |
|---|---|
| Concurrency ≤ `workers` | `TestSchedulerRespectsWorkerLimit` (counts concurrent requests on the server side) |
| A delayed target never blocks the others | `TestSlowTargetDoesNotBlockOthers` |
| Overlapping round recorded as `skipped` | `TestSkippedPersistedWhilePreviousInFlight`, `TestSchedulerSaturatedDispatch` |
| Graceful shutdown, no goroutine leak | `TestSchedulerGracefulShutdown` |
| Single-check classification (up/down/timeout/error) | `TestCheckClassification` |
| Rich checks: headers, method, expected_status, redirects | `TestCheckRichRequest`, `TestCheckExpectedStatus`, `TestCheckExpectedStatusWithRedirects`, `TestSchedulerFollowRedirectsSelectsClient` |
| TLS certificate capture + roundtrip | `TestCheckTLSCertificate`, `TestCertExpiresAtRoundtrip` |
| Certificate amber treatment | `TestDetailPageCertWarningThreshold` (detail page), `TestIndexShowsUptimeAndCert` (dashboard script) |
| Defaults/validation + bundled config files | `TestParse`, `TestExampleConfigLoads`, `TestDemoConfigLoads`, `TestDockerConfigLoads` |
| Retention/cleanup | `TestDeleteOlderThan`, `TestStartRetention` |
| Schema migration on an old database | `TestOpenMigratesOldSchema` |
| Uptime math (`skipped` excluded) | `TestUptime` + the seeded mixes in `TestStatusShape` and `TestDetailPageChart` |
| Metrics values, empty store, escaping, byte-stability | `TestMetricsBody`, `TestMetricsEmptyStore`, `TestMetricsEscaping`, `TestMetricsFormatIsStable` |
| Health endpoint shape | `TestHealthzWithData`, `TestHealthzNoData` |
| API schema/limits/404-405 | `TestStatusShape`, `TestHistory`, `TestMethodNotAllowed`, `TestUnknownPathIsNotFound` |
| Detail page + sparkline | `TestDetailPageChart`, `TestDetailPageCapsChartPoints`, `TestSparklinePoints` |
| Embedded pages + zero external resources | `TestIndexServed`, `TestNoExternalResources` |
| Body assertions (contains/regex, cap, read errors) | `TestCheckBodyContains`, `TestCheckBodyRegex`, `TestCheckBodyTruncation`, `TestCheckBodyReadError`, `TestCheckBodyReadTimeout` |
| Duration histogram + percentile estimation | `TestDurationHistogram`, `TestDurationPercentile`, `TestStats` |
| Batched snapshot equivalence + scale | `TestStatusSnapshotMatchesPerTargetQueries`, `BenchmarkStatusReadPaths` |
| Zero-downtime reload (swap, in-flight continuity, rejection policy) | `TestSchedulerReload`, `TestReloadConfig`, `TestDiffTargets`, `TestAtomicConcurrentSwapLoad` |
| Fuzz contract (no panic; config XOR error) | `FuzzParse` (seeds run with every `go test`; CI fuzzes 30 s) |

From a real `go test -race -count=1 -v ./internal/checker/` run (relevant lines only):

```text
=== RUN   TestSchedulerRespectsWorkerLimit
    scheduler_test.go:118: workers=2 targets=6: max observed concurrent checks = 2
--- PASS: TestSchedulerRespectsWorkerLimit (2.00s)
=== RUN   TestSlowTargetDoesNotBlockOthers
    scheduler_test.go:220: fastA Up = 13, fastB Up = 13, slow in-flight skips = 10, slow timeout/down = 3
--- PASS: TestSlowTargetDoesNotBlockOthers (2.50s)
=== RUN   TestSchedulerGracefulShutdown
    scheduler_test.go:310: goroutines: baseline=3 after shutdown=3
--- PASS: TestSchedulerGracefulShutdown (0.56s)
=== RUN   TestSkippedPersistedWhilePreviousInFlight
    scheduler_test.go:380: Up = 2, in-flight skips = 18
--- PASS: TestSkippedPersistedWhilePreviousInFlight (2.00s)
=== RUN   TestSchedulerSaturatedDispatch
--- PASS: TestSchedulerSaturatedDispatch (0.21s)
ok  	pulsecheck/internal/checker	8.728s
```

Benchmark evidence (the same command CI runs; numbers are from one local machine — evidence, not a regression gate):

```text
$ go test -bench '^(BenchmarkCheck|BenchmarkRecord|BenchmarkHistory|BenchmarkLatest)$' -benchtime 200x -run '^$' ./internal/checker/ ./internal/store/ -benchmem
BenchmarkCheck-8   	     200	     72996 ns/op	    5004 B/op	      55 allocs/op
BenchmarkRecord-8    	     200	    186913 ns/op	     464 B/op	      11 allocs/op
BenchmarkHistory-8   	     200	     68899 ns/op	   21498 B/op	     679 allocs/op
BenchmarkLatest-8    	     200	     10943 ns/op	    1224 B/op	      41 allocs/op
ok  	pulsecheck/internal/checker	0.449s
ok  	pulsecheck/internal/store	0.602s
```

(The benchmark names are spelled out — exactly what CI runs — because the `-bench .` wildcard would also pick up the scale benchmark below, which takes minutes by design.)

The scale benchmark behind the batched read path (D40) — 200 targets × 24 h of 30 s rows (~576k rows) in one database, seeded before the timer starts:

```text
$ go test -bench BenchmarkStatusReadPaths -benchtime 3s -count 2 -run '^$' ./internal/store/ -benchmem
BenchmarkStatusReadPaths/per-target-8         	3	1004106430 ns/op	4033637 B/op	121800 allocs/op
BenchmarkStatusReadPaths/per-target-8         	3	1007409986 ns/op	4033722 B/op	121800 allocs/op
BenchmarkStatusReadPaths/snapshot-8           	5	 642073783 ns/op	4036801 B/op	117456 allocs/op
BenchmarkStatusReadPaths/snapshot-8           	5	 648142350 ns/op	4036804 B/op	117456 allocs/op
BenchmarkStatusReadPaths/merged-aggregates-8  	4	 794109062 ns/op	 739356 B/op	 22849 allocs/op
BenchmarkStatusReadPaths/merged-aggregates-8  	5	 670376717 ns/op	 738508 B/op	 22846 allocs/op
```

The snapshot polls in ~0.65 s against ~1.0 s for the original per-target path; the `merged-aggregates` sub-run isolates the phase that dominates its cost, and the naive five-statement full-scan batching that lost the measurement (~1.8 s) is recorded in D40.

Fuzzing: the 60 s record from the robustness phase was **3,555,538 executions, 0 crashers**; a shorter re-run shows the shape:

```text
$ go test -fuzz=FuzzParse -fuzztime 10s ./internal/config/
fuzz: elapsed: 11s, execs: 443205 (9082/sec), new interesting: 12 (total: 400)
PASS
ok  	pulsecheck/internal/config	11.391s
```

## Docker

The `Dockerfile` is multi-stage: two static binaries are built with `CGO_ENABLED=0` inside `golang:1.27-alpine` and copied into an `alpine`-based final image (Alpine instead of scratch: it provides `ca-certificates` and a shell, so `https://` targets and debugging work). The image runs as the non-root `pulsecheck` user; `ENTRYPOINT ["/pulsecheck"]`. `compose.yaml` defines two services (`demo-targets` + `pulsecheck`), both using the same image and selecting which binary to run via `entrypoint`/`command`.

```console
$ docker compose up --build      # http://localhost:8080
$ docker compose down
```

`config.docker.json` is copied to `/etc/pulsecheck/config.docker.json` inside the container and passed via `command: ["-config", "/etc/pulsecheck/config.docker.json"]`.

## Demo video

A 72-second recording of a real run — three targets side by side:

[![PulseCheck status dashboard](assets/demo.png)](assets/demo.mp4)

▶ **[Watch the demo video: assets/demo.mp4](assets/demo.mp4)** (72 s, ~110 KB — visible in the video: `demo-ok` stays continuously `up`, `demo-broken` stays continuously `down`, and while `demo-slow` accumulates `skipped`/`timeout` it **never blocks the other targets**; in the dot strip the `skipped` reason shows as "previous check still in flight". The dashboard cards carry their uptime lines, and the clip ends on `demo-slow`'s detail page, where the duration sparkline shows the timeout/skip pattern directly.)

The video was produced entirely locally with a reproducible pipeline (€0): live stack via `make demo` + 36 screenshots every 2 s via Chrome headless CLI + ffmpeg H.264 assembly. (Decision log: D28.)

## License

MIT — Copyright (c) 2026 Kemal Selçuk. See [`LICENSE`](LICENSE) for details.
