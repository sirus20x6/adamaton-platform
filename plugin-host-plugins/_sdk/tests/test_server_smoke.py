"""Smoke test: stand up _Servicer in-process and hit Hello/Ping/Shutdown,
verify defaults + UNIMPLEMENTED for unregistered RPCs.
"""

from __future__ import annotations

import asyncio

import grpc
import pytest

from dr_plugin_sdk import importer, plugin
from dr_plugin_sdk import _pb
from dr_plugin_sdk.server import _Servicer
from dr_plugin_sdk.types import PluginItem, RunSummary


_plugin_pb = _pb.plugin_pb2
_plugin_grpc = _pb.plugin_pb2_grpc


@plugin(version="9.9.9", capabilities=["importer"])
class _DemoPlugin:
    @importer.sync
    async def sync(self, args, *, emit, host):
        emit.progress("starting", seen=0)
        emit.item(PluginItem(external_id="A", title="alpha"))
        emit.item(PluginItem(external_id="B", title="beta"))
        return RunSummary(seen=2, new_items=2)


@pytest.fixture
async def server_and_stub():
    """Spin up _Servicer behind a tcp gRPC server bound to an ephemeral port."""
    inst = _DemoPlugin()
    servicer = _Servicer(inst)

    server = grpc.aio.server()
    _plugin_grpc.add_PluginServicer_to_server(servicer, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()

    channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
    stub = _plugin_grpc.PluginStub(channel)
    try:
        yield servicer, stub
    finally:
        await channel.close()
        await server.stop(grace=0)


async def test_ping(server_and_stub) -> None:
    _, stub = server_and_stub
    resp = await stub.Ping(_plugin_pb.PingRequest())
    assert resp is not None


async def test_hello_returns_version_and_caps(server_and_stub) -> None:
    _, stub = server_and_stub
    resp = await stub.Hello(_plugin_pb.HelloRequest(host_version="1.0.0", work_dir="/tmp"))
    assert resp.plugin_version == "9.9.9"
    assert "importer" in list(resp.capabilities)


async def test_shutdown_flips_event(server_and_stub) -> None:
    servicer, stub = server_and_stub
    assert not servicer.shutdown_event.is_set()
    await stub.Shutdown(_plugin_pb.ShutdownRequest(grace_seconds=1))
    assert servicer.shutdown_event.is_set()


async def test_unregistered_rpc_returns_unimplemented(server_and_stub) -> None:
    _, stub = server_and_stub
    # SearchQuery has no handler on _DemoPlugin.
    with pytest.raises(grpc.aio.AioRpcError) as ei:
        await stub.SearchQuery(_plugin_pb.SearchQueryRequest(query="x"))
    assert ei.value.code() == grpc.StatusCode.UNIMPLEMENTED


async def test_sync_stream_emits_items_and_summary(server_and_stub) -> None:
    _, stub = server_and_stub
    events = []
    async for ev in stub.Sync(_plugin_pb.SyncRequest(run_id="r1")):
        events.append(ev)

    # progress, item, item, summary — order matters within categories.
    kinds = [ev.WhichOneof("event") for ev in events]
    assert kinds[0] == "progress"
    assert kinds.count("item") == 2
    assert kinds[-1] == "summary"
    assert events[-1].summary.seen == 2
    assert events[-1].summary.new_items == 2


async def test_servicer_sync_synthesizes_summary_when_handler_omits_it() -> None:
    @plugin(version="1.0.0", capabilities=["importer"])
    class _NoSummary:
        @importer.sync
        async def sync(self, args, *, emit, host):
            emit.item(PluginItem(external_id="X"))
            # No return, no emit.summary -- SDK should synthesize one.
            return None

    servicer = _Servicer(_NoSummary())
    server = grpc.aio.server()
    _plugin_grpc.add_PluginServicer_to_server(servicer, server)
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    chan = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
    stub = _plugin_grpc.PluginStub(chan)
    try:
        events = []
        async for ev in stub.Sync(_plugin_pb.SyncRequest(run_id="r")):
            events.append(ev)
        assert events[-1].WhichOneof("event") == "summary"
    finally:
        await chan.close()
        await server.stop(grace=0)
