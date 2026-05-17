# Multi-stage build: golang on trixie compiles the binary, trixie-slim
# runs it. Runtime stage installs docker-ce-cli + the compose plugin so
# `docker compose pull/up` works inside the container against the host's
# socket (bind-mounted at /var/run/docker.sock).
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because the agent's `go:embed deploy/worker-types/*.yml` reads from
# the umbrella layout. The cross-compile pattern (--platform=$BUILDPLATFORM
# + GOARCH=$TARGETARCH) mirrors plugin-host's Dockerfile — without it
# arm64 builds run every Go syscall through qemu and take 30+ min instead
# of ~30s.
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
WORKDIR /src
COPY platform/deploy-agent/go.mod platform/deploy-agent/go.sum* ./
RUN go mod download
# Bring in deploy-agent source AND the umbrella's worker-types catalog
# so go:embed deploy/worker-types/*.yml resolves.
COPY platform/deploy-agent/. ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/deploy-agent ./cmd/deploy-agent

# Stage the worker-types catalog separately so it can be COPYed into
# the runtime stage at a known path (/etc/adamaton/worker-types) where
# the agent reads it at startup. Bind-mount over this path at runtime
# to override the catalog without rebuilding.
FROM scratch AS worker-types
COPY deploy/worker-types /etc/adamaton/worker-types

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates tzdata curl gnupg \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
 && chmod a+r /etc/apt/keyrings/docker.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian trixie stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
 && apt-get purge -y curl gnupg \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/deploy-agent /usr/local/bin/deploy-agent
COPY --from=worker-types /etc/adamaton/worker-types /etc/adamaton/worker-types
EXPOSE 9128
CMD ["/usr/local/bin/deploy-agent"]
