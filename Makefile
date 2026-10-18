.PHONY: build test vet run demo clean

# build compiles both binaries into bin/: the service and the local demo server.
build:
	go build -o bin/ ./cmd/pulsecheck ./cmd/demo-targets

# test runs the whole suite under the race detector.
test:
	go test -race ./...

vet:
	go vet ./...

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
