"""SDK-decorated Plugin class for the Stack Exchange adapter."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import (
    FetchedDoc as SdkFetchedDoc,
)
from dr_plugin_sdk.types import (
    SearchPage as SdkSearchPage,
)
from dr_plugin_sdk.types import (
    SearchResult as SdkSearchResult,
)
from dr_plugin_sdk.types import (
    SourceKind as SdkSourceKind,
)

from . import adapter as _adapter

_SOURCE_KIND_MAP: dict[_adapter.SourceKind, SdkSourceKind] = {
    _adapter.SourceKind.JOURNAL: SdkSourceKind.JOURNAL,
    _adapter.SourceKind.PREPRINT: SdkSourceKind.PREPRINT,
    _adapter.SourceKind.REPO: SdkSourceKind.REPO,
    _adapter.SourceKind.FORUM: SdkSourceKind.FORUM,
    _adapter.SourceKind.WIKI: SdkSourceKind.WIKI,
    _adapter.SourceKind.WEB: SdkSourceKind.WEB,
}


def _to_sdk_result(r: _adapter.SearchResult) -> SdkSearchResult:
    return SdkSearchResult(
        adapter=r.adapter,
        external_id=r.external_id,
        title=r.title,
        url=r.url,
        abstract=r.abstract or "",
        authors=list(r.authors),
        published_at=r.published_at if isinstance(r.published_at, datetime) else None,
        venue=r.venue or "",
        citation_count=int(r.citation_count) if r.citation_count is not None else 0,
        raw=dict(r.raw),
        score=float(r.score) if r.score is not None else 0.0,
        source_kind=_SOURCE_KIND_MAP.get(r.source_kind, SdkSourceKind.UNSPECIFIED),
    )


def _to_sdk_page(p: _adapter.SearchPage) -> SdkSearchPage:
    return SdkSearchPage(
        results=[_to_sdk_result(r) for r in p.results],
        next_cursor=p.next_cursor or "",
        total_estimated=int(p.total_estimated) if p.total_estimated is not None else 0,
    )


def _from_sdk_result(r: SdkSearchResult) -> _adapter.SearchResult:
    inv = {v: k for k, v in _SOURCE_KIND_MAP.items()}
    return _adapter.SearchResult(
        adapter=r.adapter,
        external_id=r.external_id,
        title=r.title,
        url=r.url,
        abstract=r.abstract or None,
        authors=list(r.authors),
        published_at=r.published_at,
        venue=r.venue or None,
        citation_count=r.citation_count or None,
        raw=dict(r.raw),
        score=r.score or None,
        source_kind=inv.get(r.source_kind, _adapter.SourceKind.FORUM),
    )


def _to_sdk_doc(d: _adapter.FetchedDoc) -> SdkFetchedDoc:
    body: bytes
    if isinstance(d.body, str):
        body = d.body.encode("utf-8")
    else:
        body = bytes(d.body)
    md = dict(d.metadata)
    md["source_tier"] = d.source_tier
    return SdkFetchedDoc(
        adapter=d.adapter,
        external_id=d.external_id,
        url=d.url,
        title=d.title,
        content_type=d.content_type,
        body=body,
        source_tier=d.source_tier,
        metadata=md,
    )


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        self._adapter: _adapter.StackExchangeAdapter | None = None

    def _get(self) -> _adapter.StackExchangeAdapter:
        if self._adapter is None:
            self._adapter = _adapter.StackExchangeAdapter()
        return self._adapter

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: Any | None,
    ) -> SdkSearchPage:
        page = await self._get().search(q, limit=limit, cursor=cursor, since=since)
        return _to_sdk_page(page)

    @search.fetch
    async def fetch(self, result: SdkSearchResult) -> SdkFetchedDoc:
        legacy = _from_sdk_result(result)
        doc = await self._get().fetch(legacy)
        return _to_sdk_doc(doc)
