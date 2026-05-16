"""Public entry point. Spin up the plugin gRPC server on DR_PLUGIN_SOCK
and wait until Shutdown is called (or a SIGTERM arrives).
"""

from __future__ import annotations

import asyncio
import os
import signal
from typing import Any

import grpc

from . import _pb
from .server import _Servicer

_plugin_grpc = _pb.plugin_pb2_grpc


async def serve(plugin_cls: type, *, sock: str | None = None) -> None:
    """Bind to unix://$DR_PLUGIN_SOCK and serve until shutdown.

    The plugin class is instantiated here so its __init__ runs *inside*
    the running event loop — needed for any handlers that want to set
    up asyncio primitives at startup.
    """
    sock = sock or os.environ.get("DR_PLUGIN_SOCK")
    if not sock:
        raise RuntimeError("DR_PLUGIN_SOCK is not set; cannot bind plugin socket")

    instance = plugin_cls()
    servicer = _Servicer(instance)

    server = grpc.aio.server()
    _plugin_grpc.add_PluginServicer_to_server(servicer, server)

    # Remove a stale socket file from a previous run; the gRPC server
    # would otherwise fail to bind with EADDRINUSE.
    try:
        if os.path.exists(sock):
            os.unlink(sock)
    except OSError:
        pass

    server.add_insecure_port(f"unix://{sock}")
    await server.start()

    # SIGTERM from the host should be honored even if Shutdown RPC was
    # not called (e.g. host crashed and started cleanup).
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        try:
            loop.add_signal_handler(sig, servicer.shutdown_event.set)
        except NotImplementedError:
            # Windows / restricted envs; tests on Linux always have this.
            pass

    try:
        await servicer.shutdown_event.wait()
    finally:
        # 5s grace before we yank the socket.
        await server.stop(grace=5.0)


def main(plugin_cls: type) -> None:
    """asyncio.run wrapper. The convention is `if __name__ == '__main__': main(Cls)`."""
    asyncio.run(serve(plugin_cls))


__all__ = ["serve", "main"]
