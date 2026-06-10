# bge-embed — llama-server hosting BAAI/bge-m3 GGUF.
#
# First Python-elimination beachhead: replaces the embedding half of the
# legacy /thearray/git/deepresearch/platform/sidecar Python container with
# a single native binary. Reranker still Python until Qwen3-Reranker has
# a viable llama.cpp/native path.
#
# Pinned to the ggml-org server-cuda digest fetched 2026-05-17. Bump by
# repulling that tag and updating both lines below; the catalog YAML
# refers to this image via the registry tag, not the digest.

FROM ghcr.io/ggml-org/llama.cpp@sha256:ecc747ae68e35d2308d87603813bafa4a55f1ff1d75db6851f6801226d90c071

# BAAI/bge-m3 f16 GGUF from CompendiumLabs. The checksum is the
# HuggingFace LFS oid (which IS the file's SHA256 — HF LFS uses sha256
# as the content-address). Baking the model into the image lets a fresh
# `docker compose up` produce a serving container without an out-of-band
# model download — the right shape for a worker-types catalog entry that
# any deploy-agent can pull and run.
ADD --checksum=sha256:08e9d9f201d26c70c605e6ebbd30c06e686adf948c10faf27e1b333a945fb4f3 \
    https://huggingface.co/CompendiumLabs/bge-m3-gguf/resolve/main/bge-m3-f16.gguf \
    /models/bge-m3-f16.gguf

# Inherit ENTRYPOINT=llama-server from the upstream image. Compose's
# `command:` block in deploy/worker-types/bge-embed.yml supplies the
# model path + flags so this Dockerfile stays model-agnostic.
