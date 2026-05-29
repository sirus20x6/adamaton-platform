# Multi-stage build for evo-api (dashboard binary served at :9123).
#
# Build context MUST be the umbrella root, because dashboard/go.mod uses
# replace directives like `../../core` that point at sibling sub-repos.
# Those are unreachable with a per-sub-repo context.
#
#   docker buildx build --platform linux/arm64 \
#     -f platform/dashboard/Dockerfile \
#     -t 10.0.4.37:5000/adamaton-evo-api:<tag> --push .
#
# bin/adam ship pi5 evo-api handles this; ship_service_spec sets context=".".

# Cross-compile in a native-platform stage so we don't pay the qemu
# tax for an arm64 image built on amd64. --platform=$BUILDPLATFORM pins
# this stage to whatever the BUILDER is (the workstation = amd64); the
# Go toolchain handles GOARCH=$TARGETARCH for us. A full qemu-emulated
# `go build` of the dashboard takes 30+ minutes; cross-compile takes
# ~30 seconds.
FROM --platform=$BUILDPLATFORM golang:1.25-trixie AS build
ARG TARGETARCH
WORKDIR /src
# Copy every sub-repo because dashboard/go.mod's replace directives
# reach across to ../../core, ../../knowledge/*, ../../delegator/*,
# ../../evolve/*. Also copy go.work so the build runs in workspace
# mode; that's the only way Go reconciles dep versions across the
# union of every sub-repo's go.mod (without it the dashboard's go.sum
# is missing entries for deps pulled in transitively by other modules).
COPY go.work go.work.sum* ./
COPY core ./core
# ztok is copied too: the dashboard imports ztok-go (github.com/sirus20x6/ztok-go)
# transitively, whose go.work replace target is ./ztok/bindings/go — absent it,
# the build fails reading /src/ztok/bindings/go/go.mod. (CGO_ENABLED=0 here, so
# we only need the pure-Go binding source, not the compiled libztok.a.)
COPY ztok ./ztok
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

WORKDIR /src
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/api ./platform/dashboard/cmd/api

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=build /out/api /usr/local/bin/api
# Workflow-node catalog. The apiserver's resolvePluginDirs() looks for
# ${EVO_HOME}/plugins/{builtin,community,n8n}; baking the source tree in
# rather than bind-mounting at compose time keeps the deploy self-contained.
# An EVO_PLUGIN_DIRS env override still wins if an operator wants to point
# at a host mount instead. Source tree lives in the evolve sub-repo;
# accessible here because the build context is the umbrella root.
#
# Only `builtin` exists on disk today. mkdir creates the community/n8n
# siblings so the loader can enumerate them as soon as someone drops
# YAMLs in (via `go run ./cmd/n8n-import <pkg>` or a manual add) and
# the image is rebuilt — keeping the catalog purely image-shipped.
COPY --from=build /src/evolve/workflow-builder/plugins/builtin /opt/evo/plugins/builtin
RUN mkdir -p /opt/evo/plugins/community /opt/evo/plugins/n8n
ENV EVO_HOME=/opt/evo
WORKDIR /opt/evo
EXPOSE 9123
ENTRYPOINT ["/usr/local/bin/api"]
