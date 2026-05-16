"""Python dataclasses mirroring dr.plugin.v1 messages.

Plugin authors only ever see these — the SDK does the proto translation
on the wire boundary. Keep field defaults loose (empty strings / dicts)
so plugin code can omit anything it doesn't care about.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import IntEnum
from typing import Any

from google.protobuf import struct_pb2
from google.protobuf.json_format import MessageToDict, ParseDict
from google.protobuf.timestamp_pb2 import Timestamp

from . import _pb

_types = _pb.types_pb2
_host = _pb.host_pb2


# ----- helpers --------------------------------------------------------


def _struct_from_dict(d: dict[str, Any] | None) -> struct_pb2.Struct:
    s = struct_pb2.Struct()
    if d:
        # ParseDict is strict on non-JSON-safe types; convert via MessageToDict
        # round-trip philosophy: caller is expected to pass json-safe data.
        ParseDict(d, s)
    return s


def _struct_to_dict(s: struct_pb2.Struct | None) -> dict[str, Any]:
    if s is None:
        return {}
    return MessageToDict(s) if len(s.fields) else {}


def _ts_from_dt(dt: datetime | None) -> Timestamp | None:
    if dt is None:
        return None
    ts = Timestamp()
    # Timestamp.FromDatetime treats naive datetimes as UTC; normalize for clarity.
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    ts.FromDatetime(dt)
    return ts


def _ts_to_dt(ts: Timestamp | None) -> datetime | None:
    if ts is None or (ts.seconds == 0 and ts.nanos == 0):
        return None
    return ts.ToDatetime(tzinfo=timezone.utc)


# ----- SourceKind enum mirror -----------------------------------------


class SourceKind(IntEnum):
    UNSPECIFIED = 0
    JOURNAL = 1
    PREPRINT = 2
    REPO = 3
    FORUM = 4
    WIKI = 5
    WEB = 6


# ----- PluginItem -----------------------------------------------------


@dataclass(kw_only=True, slots=True)
class PluginItem:
    external_id: str
    plugin_id: str = ""
    external_url: str = ""
    title: str = ""
    markdown_body: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)
    attachments: list[str] = field(default_factory=list)

    def to_proto(self) -> Any:
        return _types.PluginItem(
            plugin_id=self.plugin_id,
            external_id=self.external_id,
            external_url=self.external_url,
            title=self.title,
            markdown_body=self.markdown_body,
            metadata=_struct_from_dict(self.metadata),
            attachments=list(self.attachments),
        )

    @classmethod
    def from_proto(cls, msg: Any) -> "PluginItem":
        return cls(
            plugin_id=msg.plugin_id,
            external_id=msg.external_id,
            external_url=msg.external_url,
            title=msg.title,
            markdown_body=msg.markdown_body,
            metadata=_struct_to_dict(msg.metadata),
            attachments=list(msg.attachments),
        )


# ----- Collection -----------------------------------------------------


@dataclass(kw_only=True, slots=True)
class Collection:
    id: str
    name: str = ""
    item_count: int = 0
    metadata: dict[str, Any] = field(default_factory=dict)

    def to_proto(self) -> Any:
        return _types.Collection(
            id=self.id,
            name=self.name,
            item_count=self.item_count,
            metadata=_struct_from_dict(self.metadata),
        )

    @classmethod
    def from_proto(cls, msg: Any) -> "Collection":
        return cls(
            id=msg.id,
            name=msg.name,
            item_count=msg.item_count,
            metadata=_struct_to_dict(msg.metadata),
        )


# ----- SearchResult / SearchPage --------------------------------------


@dataclass(kw_only=True, slots=True)
class SearchResult:
    external_id: str
    adapter: str = ""
    title: str = ""
    url: str = ""
    abstract: str = ""
    authors: list[str] = field(default_factory=list)
    published_at: datetime | None = None
    venue: str = ""
    citation_count: int = 0
    raw: dict[str, Any] = field(default_factory=dict)
    score: float = 0.0
    source_kind: SourceKind = SourceKind.UNSPECIFIED

    def to_proto(self) -> Any:
        msg = _types.SearchResult(
            adapter=self.adapter,
            external_id=self.external_id,
            title=self.title,
            url=self.url,
            abstract=self.abstract,
            authors=list(self.authors),
            venue=self.venue,
            citation_count=self.citation_count,
            raw=_struct_from_dict(self.raw),
            score=self.score,
            source_kind=int(self.source_kind),
        )
        ts = _ts_from_dt(self.published_at)
        if ts is not None:
            msg.published_at.CopyFrom(ts)
        return msg

    @classmethod
    def from_proto(cls, msg: Any) -> "SearchResult":
        return cls(
            adapter=msg.adapter,
            external_id=msg.external_id,
            title=msg.title,
            url=msg.url,
            abstract=msg.abstract,
            authors=list(msg.authors),
            published_at=_ts_to_dt(msg.published_at),
            venue=msg.venue,
            citation_count=msg.citation_count,
            raw=_struct_to_dict(msg.raw),
            score=msg.score,
            source_kind=SourceKind(msg.source_kind),
        )


@dataclass(kw_only=True, slots=True)
class SearchPage:
    results: list[SearchResult] = field(default_factory=list)
    next_cursor: str = ""
    total_estimated: int = 0

    def to_proto(self) -> Any:
        return _types.SearchPage(
            results=[r.to_proto() for r in self.results],
            next_cursor=self.next_cursor,
            total_estimated=self.total_estimated,
        )

    @classmethod
    def from_proto(cls, msg: Any) -> "SearchPage":
        return cls(
            results=[SearchResult.from_proto(r) for r in msg.results],
            next_cursor=msg.next_cursor,
            total_estimated=msg.total_estimated,
        )


# ----- FetchedDoc -----------------------------------------------------


@dataclass(kw_only=True, slots=True)
class FetchedDoc:
    external_id: str
    adapter: str = ""
    url: str = ""
    title: str = ""
    content_type: str = ""
    body: bytes = b""
    source_tier: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)

    def to_proto(self) -> Any:
        return _types.FetchedDoc(
            adapter=self.adapter,
            external_id=self.external_id,
            url=self.url,
            title=self.title,
            content_type=self.content_type,
            body=self.body,
            source_tier=self.source_tier,
            metadata=_struct_from_dict(self.metadata),
        )

    @classmethod
    def from_proto(cls, msg: Any) -> "FetchedDoc":
        return cls(
            adapter=msg.adapter,
            external_id=msg.external_id,
            url=msg.url,
            title=msg.title,
            content_type=msg.content_type,
            body=bytes(msg.body),
            source_tier=msg.source_tier,
            metadata=_struct_to_dict(msg.metadata),
        )


# ----- RunSummary -----------------------------------------------------


@dataclass(kw_only=True, slots=True)
class RunSummary:
    seen: int = 0
    new_items: int = 0
    fetched: int = 0
    deduped: int = 0
    queued: int = 0
    errored: int = 0
    skipped: int = 0

    def to_proto(self) -> Any:
        return _types.RunSummary(
            seen=self.seen,
            new_items=self.new_items,
            fetched=self.fetched,
            deduped=self.deduped,
            queued=self.queued,
            errored=self.errored,
            skipped=self.skipped,
        )

    @classmethod
    def from_proto(cls, msg: Any) -> "RunSummary":
        return cls(
            seen=msg.seen,
            new_items=msg.new_items,
            fetched=msg.fetched,
            deduped=msg.deduped,
            queued=msg.queued,
            errored=msg.errored,
            skipped=msg.skipped,
        )


# ----- SyncArgs / SyncEvent (event is internal, args is user-visible) --


@dataclass(kw_only=True, slots=True)
class SyncArgs:
    run_id: str
    collection_id: str | None = None
    since: str | None = None
    corpus_id: str | None = None
    options: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_request(cls, req: Any) -> "SyncArgs":
        return cls(
            run_id=req.run_id,
            collection_id=req.collection_id or None,
            since=req.since or None,
            corpus_id=req.corpus_id or None,
            options=_struct_to_dict(req.options),
        )


@dataclass(kw_only=True, slots=True)
class SyncProgress:
    message: str = ""
    seen: int = 0


@dataclass(kw_only=True, slots=True)
class SyncError:
    code: str
    message: str = ""
    fatal: bool = False


# Union-ish helper: the SDK turns one of these into a SyncEvent proto.
SyncEvent = PluginItem | SyncProgress | SyncError | RunSummary


def sync_event_to_proto(ev: SyncEvent) -> Any:
    if isinstance(ev, PluginItem):
        return _types.SyncEvent(item=ev.to_proto())
    if isinstance(ev, SyncProgress):
        return _types.SyncEvent(progress=_types.SyncProgress(message=ev.message, seen=ev.seen))
    if isinstance(ev, SyncError):
        return _types.SyncEvent(
            error=_types.SyncError(code=ev.code, message=ev.message, fatal=ev.fatal)
        )
    if isinstance(ev, RunSummary):
        return _types.SyncEvent(summary=ev.to_proto())
    raise TypeError(f"unknown SyncEvent payload: {type(ev)!r}")


__all__ = [
    "Collection",
    "FetchedDoc",
    "PluginItem",
    "RunSummary",
    "SearchPage",
    "SearchResult",
    "SourceKind",
    "SyncArgs",
    "SyncError",
    "SyncEvent",
    "SyncProgress",
    "sync_event_to_proto",
]
