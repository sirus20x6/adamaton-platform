# Mirror of skills/Dockerfile + reindex/Dockerfile: cross-compile on
# host, bake a tiny Alpine runtime image. Statically linked (CGO=0).
#
# Build with `make pi-dispatch-worker` (cross-compiles to bin/) then:
#   docker buildx build --platform linux/arm64 \
#     -t evo-dispatch-worker:dev --output type=docker \
#     -f dispatch/Dockerfile bin/

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY dispatch-worker.arm64 /usr/local/bin/dispatch-worker
ENTRYPOINT ["/usr/local/bin/dispatch-worker"]
