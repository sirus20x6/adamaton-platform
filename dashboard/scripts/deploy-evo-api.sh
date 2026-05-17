#!/usr/bin/env bash
# Build the evo-api image (binary + workflow-node plugin catalog) and ship
# it to the Pi. Idempotent — re-run after any dashboard source change.
#
# Why this lives in the platform sub-repo but operates on the umbrella:
# the dashboard's go.mod uses replace directives (../../core, ../../evolve,
# etc.), so the docker build context MUST be the umbrella root for the
# build stage to see those sibling sub-repos. The Dockerfile is multi-stage
# and cross-compiles internally; no host-side `go build` needed.
#
# Image tag: defaults to umbrella's short SHA, matching what deploy/pi5/up.sh
# passes as IMAGE_TAG to docker compose. Override with IMAGE_TAG=<tag>.
#
# Requires: docker buildx locally, ssh to the target Pi with docker access.

set -euo pipefail

# Resolve to the umbrella root from anywhere inside the checkout.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UMBRELLA_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
[[ -f "$UMBRELLA_ROOT/go.work" ]] || {
    echo "ERROR: could not locate umbrella root from $SCRIPT_DIR" >&2
    exit 1
}

PLATFORM_ARCH="${ARCH:-arm64}"
SSH_HOST="${SSH_HOST:-pi5}"
IMAGE_TAG="${IMAGE_TAG:-$(git -C "$UMBRELLA_ROOT" rev-parse --short HEAD)}"
IMAGE="adamaton-evo-api:${IMAGE_TAG}"

cd "$UMBRELLA_ROOT"

echo "==> [1/2] building $IMAGE for linux/$PLATFORM_ARCH (umbrella context)"
docker buildx build \
    --platform "linux/$PLATFORM_ARCH" \
    --output type=docker \
    -t "$IMAGE" \
    -f platform/dashboard/Dockerfile \
    .

echo "==> [2/2] saving image -> ssh $SSH_HOST docker load"
docker save "$IMAGE" | ssh "$SSH_HOST" docker load

echo
echo "Done. $IMAGE is now loaded on $SSH_HOST."
echo "To bring it up:"
echo "    bin/adam deploy $SSH_HOST    # rsyncs deploy/pi5/ + docker compose up -d"
echo
echo "Or restart just evo-api with the existing compose:"
echo "    ssh $SSH_HOST \"cd Adamaton-deploy && IMAGE_TAG=$IMAGE_TAG docker compose up -d evo-api\""
