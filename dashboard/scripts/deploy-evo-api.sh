#!/usr/bin/env bash
# Build the evo dashboard for arm64, package as a docker image, and
# load+restart the container on the Pi. Idempotent — re-run after every
# dashboard source change.
#
# Requires: Go toolchain locally, docker locally (buildx + tar pipe),
#           ssh deepresearch.local with docker access on the Pi.

set -euo pipefail
cd "$(dirname "$0")/../.."   # repo root

IMAGE="${IMAGE:-evo-api:dev}"
ARCH="${ARCH:-arm64}"
SSH_HOST="${SSH_HOST:-deepresearch.local}"

echo "==> cross-compiling dashboard/cmd/api for linux/$ARCH"
mkdir -p dashboard/bin
GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' \
    -o "dashboard/bin/api-$ARCH" ./dashboard/cmd/api

echo "==> baking image $IMAGE for linux/$ARCH"
docker buildx build \
    --platform "linux/$ARCH" \
    --output type=docker \
    -t "$IMAGE" \
    -f dashboard/Dockerfile \
    dashboard/

echo "==> shipping to $SSH_HOST"
docker save "$IMAGE" | ssh "$SSH_HOST" docker load

echo "==> restarting evo-api on $SSH_HOST"
ssh "$SSH_HOST" "cd ~/deepresearch && docker compose up -d evo-api"

echo "==> done. tailing recent logs:"
ssh "$SSH_HOST" "docker logs --tail 25 deepresearch-evo-api-1"
