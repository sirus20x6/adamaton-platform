"""OpenReview search plugin — gRPC façade around the legacy adapter."""

from __future__ import annotations

from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import (
    FetchedDoc,
    SearchPage,
    SearchResult,
    SourceKind,
)

from .adapter import FetchedDoc as LegacyDoc
from .adapter import OpenReviewAdapter
from .adapter import SearchResult as LegacySearchResult
from .adapter import SourceKind as LegacySourceKind


def _source_kind(lk: LegacySourceKind) -> SourceKind:
    return SourceKind[lk.name]


def _legacy_to_sdk_result(hit: LegacySearchResult) -> SearchResult:
    return SearchResult(
        adapter=hit.adapter,
        external_id=hit.external_id,
        title=hit.title,
        url=hit.url,
        abstract=hit.abstract or "",
        authors=list(hit.authors),
        published_at=hit.published_at,
        venue=hit.venue or "",
        citation_count=hit.citation_count or 0,
        raw=hit.raw or {},
        score=hit.score or 0.0,
        source_kind=_source_kind(hit.source_kind),
    )


def _legacy_to_sdk_doc(doc: LegacyDoc) -> FetchedDoc:
    body = doc.body if isinstance(doc.body, bytes) else doc.body.encode("utf-8")
    return FetchedDoc(
        adapter=doc.adapter,
        external_id=doc.external_id,
        url=doc.url,
        title=doc.title,
        content_type=doc.content_type,
        body=body,
        source_tier=doc.source_tier,
        metadata=doc.metadata or {},
    )


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        self._adapter = OpenReviewAdapter()

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        legacy_page = await self._adapter.search(
            q, limit=limit, cursor=cursor, since=since
        )
        return SearchPage(
            results=[_legacy_to_sdk_result(h) for h in legacy_page.results],
            next_cursor=legacy_page.next_cursor or "",
            total_estimated=legacy_page.total_estimated or 0,
        )

    @search.fetch
    async def fetch(self, result: SearchResult) -> FetchedDoc:
        legacy = LegacySearchResult(
            adapter=result.adapter,
            external_id=result.external_id,
            title=result.title,
            url=result.url,
            abstract=result.abstract or None,
            venue=result.venue or None,
        )
        doc = await self._adapter.fetch(legacy)
        return _legacy_to_sdk_doc(doc)
