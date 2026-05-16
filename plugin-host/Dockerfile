# Multi-stage build: golang on trixie compiles the binary, trixie-slim
# runs it. We deliberately do NOT ship plugin runtimes inside this image
# -- plugin code is bind-mounted from PH_PLUGIN_PAYLOADS_DIR at runtime
# so we can ship/upgrade plugins without rebuilding the host image.

FROM golang:1.25-trixie AS build
WORKDIR /src
# Copy go.work + every workspace member's go.mod first so dep download
# caches independently of source edits.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/plugin-host ./cmd/plugin-host

FROM debian:trixie-slim
# ca-certificates so plugin-host can dial outbound TLS (postgres SSL,
# whatever the operator points it at). tzdata so log timestamps line up
# with the host clock instead of UTC-by-default.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/plugin-host /usr/local/bin/plugin-host
EXPOSE 7375
CMD ["/usr/local/bin/plugin-host"]
