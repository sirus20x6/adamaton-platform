# adamaton-worker multi-stage, multi-variant Dockerfile.
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because worker/go.mod uses replace directives like ../../core that
# point at sibling sub-repos. --platform=$BUILDPLATFORM pins the
# builder stage to the workstation's native arch (amd64); GOARCH=$TARGETARCH
# cross-compiles to whatever the runtime image will be.
#
# Variants:
#
#   --build-arg GO_TAGS="skills reindex dispatch"          --target=runtime-light
#   --build-arg GO_TAGS="skills reindex dispatch evo"      --target=runtime-full
#   --build-arg GO_TAGS="evo"                              --target=runtime-gpu
#
# At runtime, WORKER_QUEUES (comma-separated) selects which of the
# compiled-in queues actually run — s6-overlay supervises one
# process per queue.

# --- builder ----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
ARG GO_TAGS="skills reindex dispatch"
WORKDIR /src
COPY go.work go.work.sum* ./
COPY core ./core
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -tags "${GO_TAGS}" -trimpath -ldflags='-s -w' \
    -o /out/adamaton-worker ./platform/worker/cmd/adamaton-worker

# --- s6-base ----------------------------------------------------------------
# Common runtime base: alpine + s6-overlay + the binary + the s6 service
# layout. The per-variant runtime stages below add (or skip) python3.
FROM alpine:3.21 AS s6-base
ARG TARGETARCH
# Pinned s6-overlay version. Bump deliberately — newer releases have
# changed behaviour around /var/run permissions in the past.
ARG S6_OVERLAY_VERSION=3.2.0.2
RUN apk --no-cache add ca-certificates tzdata xz
# Map docker's TARGETARCH (amd64|arm64) to s6's arch name.
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) S6_ARCH=x86_64 ;; \
      arm64) S6_ARCH=aarch64 ;; \
      arm)   S6_ARCH=armhf ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    wget -qO /tmp/s6-overlay-noarch.tar.xz \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz"; \
    wget -qO /tmp/s6-overlay-arch.tar.xz \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6_ARCH}.tar.xz"; \
    tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-overlay-arch.tar.xz; \
    rm /tmp/s6-overlay-*.tar.xz
COPY platform/worker/s6/cont-init.d/ /etc/cont-init.d/
COPY platform/worker/s6/s6-rc.d/     /etc/s6-overlay/s6-rc.d/
COPY --from=build /out/adamaton-worker /usr/local/bin/adamaton-worker
ENTRYPOINT ["/init"]

# --- runtime-light: skills + reindex + dispatch, no python ------------------
FROM s6-base AS runtime-light

# --- runtime-full: + evo (needs python3 for the kernel evaluator) -----------
FROM s6-base AS runtime-full
RUN apk --no-cache add python3 py3-pip

# --- runtime-gpu: evo-only, intended for nodes with nvidia-container-runtime
# CUDA libs are mounted from the host, not baked in here.
FROM s6-base AS runtime-gpu
RUN apk --no-cache add python3 py3-pip
