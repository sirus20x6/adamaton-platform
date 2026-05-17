# Multi-stage build: golang on trixie compiles the binary, trixie-slim
# runs it. We deliberately do NOT ship plugin runtimes inside this image
# -- plugin code is bind-mounted from PH_PLUGIN_PAYLOADS_DIR at runtime
# so we can ship/upgrade plugins without rebuilding the host image.

# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because plugin-host/go.mod uses replace directives like ../../core that
# point at sibling sub-repos. --platform=$BUILDPLATFORM pins this stage
# to the workstation's native arch (amd64); GOARCH=$TARGETARCH cross-
# compiles to whatever the runtime image will be. Without that pin every
# Go syscall runs through qemu-aarch64-static and the build takes 30+min
# instead of ~30s.
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
WORKDIR /src
# Copy every sub-repo + go.work so workspace mode resolves the union of
# all member go.mods. Without workspace mode dashboard/go.sum is missing
# entries for deps pulled in transitively by other modules.
COPY go.work go.work.sum* ./
COPY core ./core
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/plugin-host ./platform/plugin-host/cmd/plugin-host

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
