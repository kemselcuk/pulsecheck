<img src="assets/icon.svg" width="64" height="64" alt="">

# PulseCheck

[![CI](https://github.com/kemselcuk/pulsecheck/actions/workflows/ci.yml/badge.svg)](https://github.com/kemselcuk/pulsecheck/actions/workflows/ci.yml)

PulseCheck is a single Go application that checks HTTP targets from a JSON configuration at configurable intervals, stores the results in a local SQLite file, and serves a small JSON API plus a single-page status dashboard. Checks run concurrently through a bounded worker pool; a delayed target never blocks the others, an overlapping round on the same target is recorded as `skipped`, and the process exits cleanly on `SIGINT`/`SIGTERM`.

The project was written to demonstrate **HTTP, concurrency and error handling** in Go through a real, small service: standard library only, plus a single pure-Go SQLite driver. Every meaningful decision is recorded with its rationale in `docs/decisions.md`.

## Features

- **Bounded worker pool:** the number of concurrent checks is structurally limited by `defaults.workers`.
- **Per-request context timeout:** every check is bounded by `context.WithTimeout`; there is exactly one timeout mechanism.
- **Overlapping rounds are `skipped`:** while the previous check of a target is still running, the new round is skipped and recorded together with its reason.
- **24-hour retention:** old results are pruned periodically in the background (configurable).
- **Single-page dashboard:** embedded HTML/CSS/JS; no external resources, works offline, polls `/api/status` every 5 seconds.
- **Graceful shutdown:** `SIGINT`/`SIGTERM` → the checker stops, HTTP drains, the database closes; the log lines show the ordered steps.

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

Validation is strict: unknown fields, empty or duplicate target names, non-absolute URLs, and non-positive durations/worker counts are rejected. Try it:

```console
$ go run ./cmd/pulsecheck -config missing.json
time=... level=ERROR msg="failed to load config" path=missing.json err="config: open missing.json: no such file or directory"
$ echo $?
1
```

`config.docker.json` is an exact copy of `config.demo.json` except that the target URLs point at the compose service name (`http://demo-targets:18081/...`).

## API

Read-only JSON API; uses only `net/http` (Go 1.22 pattern routing) and writes nothing.

| Endpoint | Description |
|---|---|
| `GET /api/status` | Per target: the latest result (`last`), a summary of the last 30 outcomes (`recent`), and 24-hour counters (`stats`) |
| `GET /api/history/{name}?limit=N` | A single target's most recent results, newest first; `limit` defaults to 50, capped at 500; unknown name → 404 |

Examples from a real local run:

```console
$ curl -s http://localhost:8080/api/status
{"checked_at":"2026-10-18T14:58:19Z","targets":[{"name":"demo-ok","url":"http://localhost:18081/ok","last":{"outcome":"up","status_code":200,"duration_ms":1,"at":"2026-10-18T14:58:15Z","error":""},"recent":["up","up"],"stats":{"up":2,"down":0,"timeout":0,"error":0,"skipped":0,"avg_duration_ms":2}},{"name":"demo-broken","url":"http://localhost:18081/broken","last":{"outcome":"down","status_code":500,"duration_ms":1,"at":"2026-10-18T14:58:15Z","error":""},"recent":["down","down"],"stats":{"up":0,"down":2,"timeout":0,"error":0,"skipped":0,"avg_duration_ms":2}},{"name":"demo-slow","url":"http://localhost:18081/slow","last":{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T14:58:14Z","error":"previous check still in flight"},"recent":["skipped","timeout","skipped","timeout"],"stats":{"up":0,"down":0,"timeout":2,"error":0,"skipped":2,"avg_duration_ms":2501}}]}
```

```console
$ curl -s "http://localhost:8080/api/history/demo-slow?limit=3"
[{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T14:58:14Z","error":"previous check still in flight"},{"outcome":"timeout","status_code":0,"duration_ms":5001,"at":"2026-10-18T14:58:11Z","error":"Get \"http://localhost:18081/slow\": context deadline exceeded"},{"outcome":"skipped","status_code":0,"duration_ms":0,"at":"2026-10-18T14:58:08Z","error":"previous check still in flight"}]

$ curl -s -o /dev/null -w "%{http_code}\n" "http://localhost:8080/api/history/unknown"
404
```

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
  "checked_at": "2026-10-18T14:58:19Z",
  "targets": [
    {
      "name": "demo-ok",
      "url": "http://localhost:18081/ok",
      "last": {
        "outcome": "up",
        "status_code": 200,
        "duration_ms": 1,
        "at": "2026-10-18T14:58:15Z",
        "error": ""
      },
      "recent": ["up", "up"],
      "stats": { "up": 2, "down": 0, "timeout": 0, "error": 0, "skipped": 0, "avg_duration_ms": 2 }
    }
  ]
}
```

`outcome` values: `up`, `down`, `timeout`, `error`, `skipped`. `/api/history/{name}` returns an array carrying the same `outcome`/`status_code`/`duration_ms`/`at`/`error` fields (`at` is RFC3339 UTC).

## Architecture and decisions

### Package map

| Package | Responsibility |
|---|---|
| `cmd/pulsecheck` | The `main` that wires everything together; process lifecycle and graceful shutdown |
| `cmd/demo-targets` | Local demo server built on `net/http`: `/ok` → 200, `/broken` → 500, `/slow` → delayed |
| `internal/config` | Reads the JSON configuration, applies defaults, validates |
| `internal/checker` | Single HTTP check + scheduler with a bounded worker pool |
| `internal/store` | SQLite persistence (record, history, stats) and periodic cleanup |
| `internal/api` | Read-only JSON API (`/api/status`, `/api/history/{name}`) |
| `internal/web` | Single-page status dashboard embedded via `embed.FS` |

### Why these decisions?

- **One timeout mechanism — the context.** `checker.Check` never sets `http.Client.Timeout`; the only deadline comes from the per-request `context.WithTimeout`. "Check timed out" and "shutdown cancellation" are therefore the same mechanism: at shutdown, in-flight checks are aborted immediately and there is no confusing double-deadline interaction.
- **Per-target atomic in-flight lock.** Every round is claimed via `atomic.Bool.CompareAndSwap`; if the previous check is still running, the round is recorded as `skipped` with its reason instead of blocking. The queue never grows, and a delayed target never makes the others wait.
- **The `Recorder` seam.** The `checker` package does not depend on `internal/store`; it hands results to a `Recorder` interface. The store implements that interface (compile-time proof: `var _ checker.Recorder = (*store.Store)(nil)`), and tests use `MemoryRecorder`. This makes it possible to test the scheduler without a real database.
- **`modernc.org/sqlite` (pure Go).** No CGO required: it compiles statically with `CGO_ENABLED=0`, a small Alpine image is enough, and cross-compilation just works. `mattn/go-sqlite3` would require CGO.
- **Dependency discipline.** The application code uses only the standard library plus a single pure-Go SQLite driver; JSON was chosen over YAML for configuration, so no extra dependency was born.

Full decision log: [`docs/decisions.md`](docs/decisions.md) · Implementation plan: [`docs/PLAN.md`](docs/PLAN.md).

## Tests

```console
$ make test        # go test -race ./...
$ make vet         # go vet ./...
```

Which test proves which promise:

| Promise | Test |
|---|---|
| Concurrency ≤ `workers` | `TestSchedulerRespectsWorkerLimit` (counts concurrent requests on the server side) |
| A delayed target never blocks the others | `TestSlowTargetDoesNotBlockOthers` |
| Overlapping round recorded as `skipped` | `TestSkippedPersistedWhilePreviousInFlight`, `TestSchedulerSaturatedDispatch` |
| Graceful shutdown, no goroutine leak | `TestSchedulerGracefulShutdown` |
| Single-check classification (up/down/timeout/error) | `TestCheckClassification` |
| Defaults/validation + bundled config files | `TestParse`, `TestExampleConfigLoads`, `TestDemoConfigLoads`, `TestDockerConfigLoads` |
| Retention/cleanup | `TestDeleteOlderThan`, `TestStartRetention` |
| API schema/limits/404-405 | `TestStatusShape`, `TestHistory`, `TestMethodNotAllowed`, `TestUnknownPathIsNotFound` |
| Embedded page + zero external resources | `TestIndexServed`, `TestNoExternalResources` |

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

▶ **[Watch the demo video: assets/demo.mp4](assets/demo.mp4)** (72 s, ~64 KB — visible in the video: `demo-ok` stays continuously `up`, `demo-broken` stays continuously `down`, and while `demo-slow` accumulates `skipped`/`timeout` it **never blocks the other targets**; in the dot strip the `skipped` reason shows as "previous check still in flight").

The video was produced entirely locally with a reproducible pipeline (€0): live stack via `make demo` + 36 screenshots every 2 s via Chrome headless CLI + ffmpeg H.264 assembly. (Decision log: D28.)

## License

MIT — Copyright (c) 2026 Kemal Selçuk. See [`LICENSE`](LICENSE) for details.
