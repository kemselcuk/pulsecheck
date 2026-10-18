.PHONY: build test vet lint fmt cover run demo clean

# VERSION is baked into the service binary with -ldflags and reported by
# /healthz, /metrics and the startup log. Override it for a release build:
#   make build VERSION=v1.0.0
VERSION ?= dev

# build compiles both binaries into bin/: the service (with VERSION injected)
# and the local demo server.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/pulsecheck ./cmd/pulsecheck
	go build -o bin/demo-targets ./cmd/demo-targets

# test runs the whole suite under the race detector.
test:
	go test -race ./...

vet:
	go vet ./...

# lint runs golangci-lint with the repository configuration (.golangci.yml).
# It uses whatever golangci-lint v2 is on PATH; CI is the authoritative gate
# and pins the version (v2.15.0) in its lint job.
lint:
	golangci-lint run

# fmt rewrites the tree with gofmt.
fmt:
	gofmt -w .

# cover runs the race suite with coverage and prints the statement total.
cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# run starts the service with the bundled demo configuration. The default
# -config path (config.demo.json) is relative to the working directory, so run
# this from the repository root.
run:
	go run ./cmd/pulsecheck

# demo is the one-command local demo: it builds both binaries, starts
# demo-targets in the background, then runs pulsecheck in the foreground. The
# EXIT/INT/TERM trap kills the background server when pulsecheck stops, so no
# stray process is left listening on 18081.
demo:
	go build -o bin/demo-targets ./cmd/demo-targets
	go build -o bin/pulsecheck ./cmd/pulsecheck
	./bin/demo-targets & PID=$$!; trap "kill $$PID" EXIT INT TERM; ./bin/pulsecheck

clean:
	rm -rf bin
