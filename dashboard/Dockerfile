# Two-stage strategy: cross-compile the Go binary on the host, then
# bake a tiny runtime image that just copies it in. This avoids
# multi-arch buildx complexity — the host runs the Go cross-compile,
# the image is single-arch (arm64 for the Pi) and tiny.
#
# Build with the wrapper script:
#   ./dashboard/scripts/deploy-evo-api.sh
# Or manually (run from the repo root):
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
#     go build -trimpath -ldflags='-s -w' \
#     -o dashboard/bin/api-arm64 ./dashboard/cmd/api
#   docker buildx build --platform linux/arm64 \
#     -t evo-api:dev --output type=docker \
#     -f dashboard/Dockerfile dashboard/

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata
COPY bin/api-arm64 /usr/local/bin/api
EXPOSE 9123
ENTRYPOINT ["/usr/local/bin/api"]
