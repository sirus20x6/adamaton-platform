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

FROM golang:1.25-trixie AS build
WORKDIR /src
# Copy every sub-repo because dashboard/go.mod's replace directives
# reach across to ../../core, ../../knowledge/*, ../../delegator/*,
# ../../evolve/*.
COPY core ./core
COPY knowledge ./knowledge
COPY deepresearch ./deepresearch
COPY delegator ./delegator
COPY evolve ./evolve
COPY platform ./platform

WORKDIR /src/platform/dashboard
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags='-s -w' \
    -o /out/api ./cmd/api

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY --from=build /out/api /usr/local/bin/api
EXPOSE 9123
ENTRYPOINT ["/usr/local/bin/api"]
