"""Internal: load the vendored generated stubs.

The generated files do ``from dr.plugin.v1 import ...`` — the cleanest
way to satisfy that without rewriting them is to put _gen/ on sys.path
and let them resolve via the vendored ``dr.plugin.v1`` package living
under _gen/. We do this *once*, lazily, before importing the stubs.

The vendored dr/ package's __init__ files are empty so it won't shadow
any top-level ``dr`` namespace package the user might also install.
"""

from __future__ import annotations

import sys
from pathlib import Path

_GEN_ROOT = Path(__file__).parent / "_gen"
if str(_GEN_ROOT) not in sys.path:
    # Prepend so we beat any other ``dr.plugin.v1`` package in site-packages.
    sys.path.insert(0, str(_GEN_ROOT))

from dr.plugin.v1 import plugin_pb2, plugin_pb2_grpc  # noqa: E402
from dr.plugin.v1 import host_pb2, host_pb2_grpc  # noqa: E402
from dr.plugin.v1 import types_pb2  # noqa: E402

__all__ = [
    "plugin_pb2",
    "plugin_pb2_grpc",
    "host_pb2",
    "host_pb2_grpc",
    "types_pb2",
]
