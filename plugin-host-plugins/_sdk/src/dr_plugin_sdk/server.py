"""Plugin-side gRPC servicer.

Built from a plugin class decorated with @plugin. The class supplies a
registry mapping RPC method names ("Sync", "SearchQuery", ...) to method
names on the instance. The servicer translates between proto messages
and the Python-shaped dataclasses the user's handler expects.
"""

from __future__ import annotations

import asyncio
import inspect
from typing import Any, AsyncIterator, Callable

import grpc
from google.protobuf.json_format import MessageToDict

from . import _pb
from .decorators import _capabilities, _registry, _version
from .host_client import HostClient, make_host_client
from .types import (
    Collection,
    FetchedDoc,
    PluginItem,
    RunSummary,
    SearchPage,
    SearchResult,
    SourceKind,
    SyncArgs,
    SyncError,
    SyncEvent,
    SyncProgress,
    sync_event_to_proto,
)

_plugin = _pb.plugin_pb2
_plugin_grpc = _pb.plugin_pb2_grpc
_types = _pb.types_pb2


# ----- Sync emitter ---------------------------------------------------


class _SyncEmitter:
    """Push-style API the user's sync handler holds.

    Internally just a queue the servicer drains. Items pushed here are
    interleaved with whatever the handler yields (if it yields at all).
    """

    def __init__(self, queue: asyncio.Queue[SyncEvent | None]) -> None:
        self._q = queue

    def item(self, item: PluginItem) -> None:
        self._q.put_nowait(item)

    def progress(self, message: str = "", seen: int = 0) -> None:
        self._q.put_nowait(SyncProgress(message=message, seen=seen))

    def error(self, code: str, message: str = "", fatal: bool = False) -> None:
        self._q.put_nowait(SyncError(code=code, message=message, fatal=fatal))

    def summary(self, summary: RunSummary) -> None:
        # Optional — handler may also return one. Used when the handler
        # wants to send extra summaries early (rare).
        self._q.put_nowait(summary)


# ----- Servicer -------------------------------------------------------


class _Servicer(_plugin_grpc.PluginServicer):
    def __init__(self, instance: Any) -> None:
        self._inst = instance
        self._registry: dict[str, str] = _registry(instance)
        self._version: str = _version(instance) or "0.0.0"
        self._capabilities: list[str] = _capabilities(instance) or list(self._registry_caps())
        self.shutdown_event: asyncio.Event = asyncio.Event()

    def _registry_caps(self) -> list[str]:
        # Derive a coarse capability set from which RPCs are registered,
        # for plugins that forgot to declare capabilities in the manifest.
        caps: set[str] = set()
        if any(rpc in self._registry for rpc in ("Sync", "ListCollections")):
            caps.add("importer")
        if any(rpc in self._registry for rpc in ("SearchQuery", "SearchFetch")):
            caps.add("search")
        if any(rpc in self._registry for rpc in ("MarketplaceQuery", "MarketplaceFetchListing")):
            caps.add("marketplace")
        return sorted(caps)

    def _handler(self, rpc: str) -> Callable[..., Any] | None:
        name = self._registry.get(rpc)
        if not name:
            return None
        return getattr(self._inst, name)

    @staticmethod
    async def _maybe_await(value: Any) -> Any:
        if inspect.isawaitable(value):
            return await value
        return value

    # ----- Lifecycle ---------------------------------------------------

    async def Hello(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        # Forward to a registered Hello handler if any — but Hello isn't
        # something users normally tag. We just record config + work_dir on
        # the instance for handlers to read later.
        cfg = MessageToDict(request.config) if len(request.config.fields) else {}
        self._inst._dr_config = cfg  # noqa: SLF001 -- intentional handoff
        self._inst._dr_work_dir = request.work_dir  # noqa: SLF001
        self._inst._dr_host_version = request.host_version  # noqa: SLF001

        h = self._handler("Hello")
        if h is not None:
            await self._maybe_await(h(config=cfg, work_dir=request.work_dir))

        return _plugin.HelloResponse(
            plugin_version=self._version,
            capabilities=list(self._capabilities),
        )

    async def Ping(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        return _plugin.PingResponse()

    async def Shutdown(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        # Let any registered handler run cleanup before we flip the flag.
        h = self._handler("Shutdown")
        if h is not None:
            try:
                await self._maybe_await(h(grace_seconds=request.grace_seconds))
            except Exception:
                # Don't block shutdown on a misbehaving cleanup hook.
                pass
        self.shutdown_event.set()
        return _plugin.ShutdownResponse()

    # ----- Importer ---------------------------------------------------

    async def ListCollections(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        h = self._handler("ListCollections")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no ListCollections handler")
        opts = MessageToDict(request.options) if len(request.options.fields) else {}
        result = await self._maybe_await(h(options=opts))
        cols = [c if not isinstance(c, Collection) else c.to_proto() for c in result]
        # Allow returning raw protos or dataclasses; coerce dataclasses above.
        proto_cols = []
        for c in result:
            if isinstance(c, Collection):
                proto_cols.append(c.to_proto())
            elif isinstance(c, dict):
                proto_cols.append(Collection(**c).to_proto())
            else:
                proto_cols.append(c)
        return _plugin.ListCollectionsResponse(collections=proto_cols)

    async def Sync(
        self, request: Any, context: grpc.aio.ServicerContext
    ) -> AsyncIterator[Any]:
        h = self._handler("Sync")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no Sync handler")
            return

        args = SyncArgs.from_request(request)
        queue: asyncio.Queue[SyncEvent | None] = asyncio.Queue()
        emit = _SyncEmitter(queue)

        # The handler is called in the background; everything it pushes
        # onto the queue (or yields) ends up in `queue` and we drain it.
        host = make_host_client() if "DR_HOST_SOCK" in __import__("os").environ else None

        async def _runner() -> None:
            try:
                result = h(args, emit=emit, host=host)
                # The handler can be (a) an async generator yielding events,
                # (b) a coroutine returning a RunSummary, or (c) a coroutine
                # returning None (and relying on emit/summary).
                if inspect.isasyncgen(result):
                    async for ev in result:
                        queue.put_nowait(ev)
                else:
                    final = await self._maybe_await(result)
                    if isinstance(final, RunSummary):
                        queue.put_nowait(final)
                    elif final is None:
                        # Handler relied on emit; nothing else to push.
                        pass
                    else:
                        # Unknown return — treat as an error so we don't
                        # silently hang the host.
                        queue.put_nowait(
                            SyncError(
                                code="bad_return",
                                message=f"sync handler returned unsupported type {type(final).__name__}",
                                fatal=True,
                            )
                        )
            except Exception as e:
                queue.put_nowait(
                    SyncError(code="exception", message=repr(e), fatal=True)
                )
            finally:
                queue.put_nowait(None)  # sentinel: handler is done

        task = asyncio.create_task(_runner())
        sent_summary = False
        try:
            while True:
                ev = await queue.get()
                if ev is None:
                    break
                proto = sync_event_to_proto(ev)
                if isinstance(ev, RunSummary):
                    sent_summary = True
                yield proto
                if isinstance(ev, SyncError) and ev.fatal:
                    # Drain remaining items the handler may still push,
                    # but don't block forever. Cancel the task.
                    break
            if not sent_summary:
                # Wire contract requires a final summary; synthesize an empty one.
                yield sync_event_to_proto(RunSummary())
        finally:
            if not task.done():
                task.cancel()
            if host is not None:
                await host.close()

    # ----- Search ------------------------------------------------------

    async def SearchQuery(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        h = self._handler("SearchQuery")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no SearchQuery handler")
        result = await self._maybe_await(
            h(
                q=request.query,
                limit=request.limit or 10,
                cursor=request.cursor,
                since=request.since or None,
            )
        )
        page = _coerce_search_page(result)
        return _plugin.SearchQueryResponse(page=page.to_proto())

    async def SearchFetch(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        h = self._handler("SearchFetch")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no SearchFetch handler")
        sr = SearchResult.from_proto(request.result)
        result = await self._maybe_await(h(result=sr))
        if isinstance(result, FetchedDoc):
            doc = result
        elif isinstance(result, dict):
            doc = FetchedDoc(**result)
        else:
            doc = result  # assume proto
        proto_doc = doc.to_proto() if isinstance(doc, FetchedDoc) else doc
        return _plugin.SearchFetchResponse(doc=proto_doc)

    # ----- Marketplace (passthrough) -----------------------------------

    async def MarketplaceQuery(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        h = self._handler("MarketplaceQuery")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no MarketplaceQuery handler")
        result = await self._maybe_await(
            h(
                query=request.query,
                limit=request.limit or 10,
                cursor=request.cursor,
                filters=MessageToDict(request.filters) if len(request.filters.fields) else {},
            )
        )
        # We don't have first-class dataclasses for marketplace yet —
        # accept the raw proto from user code.
        return _plugin.MarketplaceQueryResponse(page=result)

    async def MarketplaceFetchListing(self, request: Any, context: grpc.aio.ServicerContext) -> Any:
        h = self._handler("MarketplaceFetchListing")
        if h is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "no MarketplaceFetchListing handler")
        result = await self._maybe_await(
            h(external_id=request.external_id, url=request.url)
        )
        return _plugin.MarketplaceFetchListingResponse(listing=result)


def _coerce_search_page(value: Any) -> SearchPage:
    if isinstance(value, SearchPage):
        return value
    if isinstance(value, dict):
        # Accept a dict shape: {results: [...], next_cursor: "", total_estimated: N}
        results = []
        for r in value.get("results", []):
            if isinstance(r, SearchResult):
                results.append(r)
            elif isinstance(r, dict):
                results.append(SearchResult(**r))
            else:
                # Already a proto?  Convert back through from_proto.
                results.append(SearchResult.from_proto(r))
        return SearchPage(
            results=results,
            next_cursor=value.get("next_cursor", ""),
            total_estimated=value.get("total_estimated", 0),
        )
    if isinstance(value, list):
        # Bare list of results — wrap.
        return SearchPage(results=[r if isinstance(r, SearchResult) else SearchResult(**r) for r in value])
    raise TypeError(f"search.query must return SearchPage/dict/list, got {type(value).__name__}")


__all__ = ["_Servicer", "_SyncEmitter"]
