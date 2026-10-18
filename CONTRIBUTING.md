# Contributing

PulseCheck is a small, deliberately scoped project. Issues and pull requests
are welcome — please keep changes aligned with its character: standard library
first, a single pure-Go dependency, and every meaningful decision recorded.

## Development

Requires Go 1.27+ (and `golangci-lint` v2 for `make lint`).

```console
$ make test     # go test -race ./...
$ make vet      # go vet ./...
$ make lint     # golangci-lint run with .golangci.yml
$ make fmt      # gofmt -w .
$ make cover    # race suite with coverage; prints the statement total
$ make demo     # local demo stack at http://localhost:8080
```

## Expectations for a change

- **Tests:** new behaviour needs a test that would fail without it.
  Timing-sensitive tests assert invariants, never forced interleavings (D19).
- **Evidence:** `make vet`, `make test` and `make lint` stay green; CI runs the
  same gates plus a 30 s fuzz step and a benchmark evidence step.
- **Decisions:** meaningful design choices get an entry in
  `docs/decisions.md` (context → decision → rationale).
- **Style:** conventional commit subjects; one logical change per commit where
  practical.

## Releases

Releases are cut from `v*` tags by `.github/workflows/release.yml`; a tag is
pushed only after CI is green on `main`.
