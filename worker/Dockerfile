# adamaton-worker multi-stage, multi-variant Dockerfile.
#
# Build context MUST be the umbrella root (bin/adam ship sets it to "."),
# because worker/go.mod uses replace directives like ../../core that
# point at sibling sub-repos. --platform=$BUILDPLATFORM pins the
# builder stage to the workstation's native arch (amd64); GOARCH=$TARGETARCH
# cross-compiles to whatever the runtime image will be.
#
# CGO note: the `reindex` queue pulls in r2g/chunking, which tokenizes via
# ztok (the Zig tokenizer at the ./ztok submodule) through its cgo binding.
# So this image is CGO_ENABLED=1 on debian/glibc — NOT the old alpine/musl
# CGO=0 static build. The `ztoklib` stage cross-compiles libztok.a with Zig
# and we static-link it into the worker; only libc is dynamic. Running on
# glibc also sidesteps the go-fitz->purego musl trap that motivated keeping
# the old worker cgo-free.
#
# Variants:
#
#   --build-arg GO_TAGS="skills reindex dispatch"          --target=runtime-light
#   --build-arg GO_TAGS="skills reindex dispatch evo"      --target=runtime-full
#   --build-arg GO_TAGS="evo"                              --target=runtime-gpu
#
# experiments note: the `experiments-train` launch queue (see
# cmd/adamaton-worker/reg_experiments.go) rides with `evo` — any GO_TAGS
# set that includes `evo` (the FULL/pi5 and GPU/blackwell variants) is
# automatically expanded to also compile in `experiments`, both for the
# go build tags and the s6 contents.d service set. Passing `experiments`
# explicitly in GO_TAGS also works. The queue still only RUNS where
# WORKER_QUEUES selects it and a Postgres DSN (EVO_POSTGRES_DSN or
# POSTGRES_DSN) is provided; setup fails fast otherwise.
#
# At runtime, WORKER_QUEUES (comma-separated) selects which of the
# compiled-in queues actually run — s6-overlay supervises one
# process per queue.

# --- ztoklib: cross-compile libztok.a with Zig --------------------------------
# Pinned to BUILDPLATFORM (amd64); Zig cross-compiles to the runtime arch,
# so we only need the amd64 Zig toolchain regardless of TARGETARCH.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS ztoklib
ARG TARGETARCH
ARG ZIG_VERSION=0.16.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz" \
      | tar -xJ -C /opt \
    && ln -s /opt/zig-x86_64-linux-${ZIG_VERSION}/zig /usr/local/bin/zig
WORKDIR /src
COPY ztok ./ztok
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) ZTARGET=x86_64-linux-gnu ;; \
      arm64) ZTARGET=aarch64-linux-gnu ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    cd ztok && zig build -Doptimize=ReleaseFast -Dtarget="${ZTARGET}" -p /opt/ztok; \
    rm -f /opt/ztok/lib/libztok.so*   # force the cgo link to pick libztok.a

# --- builder ----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
ARG GO_TAGS="skills reindex dispatch"
# C toolchains: native gcc (amd64) + the arm64 cross-compiler so cgo can
# link libztok.a for either runtime arch (mirrors knowledge/r2g/Dockerfile).
RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends build-essential crossbuild-essential-arm64 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY --from=ztoklib /opt/ztok /opt/ztok
COPY go.work go.work.sum* ./
COPY core ./core
COPY ztok ./ztok
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

# CGO_ENABLED=1; build the ztok binding with -tags nopkgconfig and point cgo
# at the cross-built libztok via CGO_CFLAGS/CGO_LDFLAGS. For arm64, drive the
# cross C compiler (the Go compiler itself still runs natively on amd64).
# experiments rides with evo (see header note): the TAGS expansion below
# adds `experiments` whenever `evo` is tagged, so the FULL/GPU variants
# compile in the experiments-train launch queue.
RUN set -eux; \
    CC=gcc; \
    if [ "${TARGETARCH}" = "arm64" ]; then CC=aarch64-linux-gnu-gcc; fi; \
    TAGS="${GO_TAGS}"; \
    case " ${TAGS} " in \
      *" experiments "*) ;; \
      *" evo "*) TAGS="${TAGS} experiments" ;; \
    esac; \
    CGO_ENABLED=1 GOOS=linux GOARCH=${TARGETARCH} CC="$CC" \
    CGO_CFLAGS="-I/opt/ztok/include" \
    CGO_LDFLAGS="-L/opt/ztok/lib" \
    go build -tags "nopkgconfig ${TAGS}" -trimpath -ldflags='-s -w' \
    -o /out/adamaton-worker ./platform/worker/cmd/adamaton-worker

# --- s6-base ----------------------------------------------------------------
# Common runtime base: debian + s6-overlay + the binary + the s6 service
# layout. The per-variant runtime stages below add (or skip) python3.
# Base is debian:trixie-slim (glibc 2.40) — matches the builder's libc, and
# the cgo binary is glibc-dynamic so it cannot run on alpine/musl.
FROM debian:trixie-slim AS s6-base
ARG TARGETARCH
# Pinned s6-overlay version. Bump deliberately — newer releases have
# changed behaviour around /var/run permissions in the past.
ARG S6_OVERLAY_VERSION=3.2.0.2
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata xz-utils wget \
    && rm -rf /var/lib/apt/lists/*
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
ARG GO_TAGS="skills reindex dispatch"
COPY platform/worker/s6/s6-rc.d/   /etc/s6-overlay/s6-rc.d/
COPY platform/worker/s6/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
# Bake the user-bundle contents.d entries at build time, one per queue
# implied by the GO_TAGS. s6-rc compiles /etc/s6-overlay/s6-rc.d/ once
# at /init time — too early for any runtime hook except the entrypoint
# script below. WORKER_QUEUES filtering happens in /entrypoint.sh
# BEFORE handing off to /init, by pruning contents.d/ entries the
# operator excluded.
# The TAGS expansion mirrors the builder stage's evo -> experiments tag
# expansion so the s6 service set matches what the binary compiled in.
RUN set -eux; \
    contents=/etc/s6-overlay/s6-rc.d/user/contents.d; \
    rm -f "$contents"/.gitkeep "$contents"/.keep; \
    TAGS="${GO_TAGS}"; \
    case " ${TAGS} " in \
      *" experiments "*) ;; \
      *" evo "*) TAGS="${TAGS} experiments" ;; \
    esac; \
    for tag in ${TAGS}; do \
      case "$tag" in \
        skills)      touch "$contents/skills" ;; \
        reindex)     touch "$contents/reindex-ingest" "$contents/reindex-extract" ;; \
        dispatch)    touch "$contents/dispatch" ;; \
        evo)         touch "$contents/evo" ;; \
        experiments) touch "$contents/experiments" ;; \
        *) echo "unknown GO_TAGS entry: $tag" >&2; exit 1 ;; \
      esac; \
    done; \
    ls -1 "$contents"
COPY --from=build /out/adamaton-worker /usr/local/bin/adamaton-worker
ENTRYPOINT ["/entrypoint.sh"]

# --- runtime-light: skills + reindex + dispatch, no python ------------------
FROM s6-base AS runtime-light

# --- runtime-full: + evo (needs python3 for the kernel evaluator) -----------
FROM s6-base AS runtime-full
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# --- runtime-gpu: evo-only, intended for nodes with nvidia-container-runtime
# CUDA libs are mounted from the host, not baked in here.
FROM s6-base AS runtime-gpu
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*
