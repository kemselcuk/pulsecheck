# syntax=docker/dockerfile:1

# --- Build stage -----------------------------------------------------------
# Pure-Go build: CGO is disabled because modernc.org/sqlite is a pure-Go
# driver, so both binaries are static and the runtime image needs no libc.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependency layer first, so editing source does not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Two binaries from one stage: the service and the local demo target server.
# -trimpath drops local paths from the binary; -s -w strip the symbol table and
# DWARF data, which is fine for a shipped binary and keeps the image small.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/pulsecheck ./cmd/pulsecheck
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/demo-targets ./cmd/demo-targets

# --- Runtime stage ---------------------------------------------------------
# alpine (not scratch) on purpose: the extra few megabytes buy a shell for
# debugging and — more importantly — ca-certificates, so https:// targets work.
# A scratch image would ship neither, and its smaller size is irrelevant here.
FROM alpine:3.21

RUN apk add --no-cache ca-certificates

# Non-root runtime user with an explicit high uid, so the intent survives even
# on a future base image that changes default account tooling.
RUN adduser -D -u 10001 pulsecheck

WORKDIR /app

# Both binaries live at the image root. Compose picks which one a service runs
# via entrypoint/command; the default ENTRYPOINT below runs the service.
COPY --from=build /out/pulsecheck /pulsecheck
COPY --from=build /out/demo-targets /demo-targets

# Configs: the demo one makes a bare `docker run` work, the docker one points
# the service at the compose network's demo-targets host.
COPY config.demo.json /app/config.demo.json
COPY config.docker.json /etc/pulsecheck/config.docker.json

# Writable data directory for the SQLite file. config.docker.json uses the
# relative path data/demo.db, resolved against the WORKDIR above.
RUN mkdir -p /app/data && chown -R pulsecheck /app

USER pulsecheck

EXPOSE 8080

# With no arguments the image starts the service against config.demo.json.
ENTRYPOINT ["/pulsecheck"]
