.PHONY: build test vet run clean

build:
	go build -o bin/ ./cmd/pulsecheck ./cmd/demo-targets

test:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/pulsecheck

clean:
	rm -rf bin
