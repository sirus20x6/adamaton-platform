"""SDK-decorated plugin class wrapping :class:`WikiCrawlerAdapter`.

The adapter speaks legacy types (its own local SearchResult / SearchPage
classes). This module is the thin translation layer between those and
the SDK's dataclasses, which the SDK serializes onto the gRPC wire.
"""

from __future__ import annotations

import os

from dr_plugin_sdk import (
    FetchedDoc as SdkFetchedDoc,
    SearchPage as SdkSearchPage,
    SearchResult as SdkSearchResult,
    SourceKind as SdkSourceKind,
    plugin,
    search,
)

from .adapter import WikiCrawlerAdapter


def _to_sdk_result(r) -> SdkSearchResult:
    return SdkSearchResult(
        adapter=r.adapter,
        external_id=r.external_id,
        title=r.title or "",
        url=r.url or "",
        abstract=r.abstract or "",
        authors=list(r.authors or []),
        published_at=r.published_at,
        venue=r.venue or "",
        citation_count=int(r.citation_count or 0),
        raw=dict(r.raw or {}),
        score=float(r.score or 0.0),
        source_kind=SdkSourceKind.WIKI,
    )


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        # Config arrives via Hello (stashed on self._dr_config by the SDK);
        # we read env vars as a fallback so unit tests don't need Hello.
        ua = os.environ.get("WIKI_CRAWLER_USER_AGENT") or None
        try:
            max_pages = int(os.environ.get("WIKI_CRAWLER_MAX_PAGES", "25"))
        except ValueError:
            max_pages = 25
        self._adapter = WikiCrawlerAdapter(
            user_agent=ua,
            max_pages_per_query=max_pages,
        )

    async def _apply_config(self) -> None:
        # If Hello delivered config, re-init the adapter to honor it.
        cfg = getattr(self, "_dr_config", None) or {}
        if not cfg:
            return
        ua = cfg.get("user_agent") or self._adapter._user_agent
        max_pages = int(cfg.get("max_pages_per_query") or self._adapter._max_pages_per_query)
        self._adapter = WikiCrawlerAdapter(user_agent=ua, max_pages_per_query=max_pages)

    @search.query
    async def search_query(
        self,
        *,
        q: str,
        limit: int = 10,
        cursor: str = "",
        since: str = "",
    ) -> SdkSearchPage:
        await self._apply_config()
        page = await self._adapter.search(
            q,
            limit=limit,
            cursor=cursor or None,
            since=None,  # legacy adapter accepts datetime; wiki crawl doesn't filter by date
        )
        return SdkSearchPage(
            results=[_to_sdk_result(r) for r in page.results],
            next_cursor=page.next_cursor or "",
            total_estimated=int(page.total_estimated or 0),
        )

    @search.fetch
    async def search_fetch(self, *, result) -> SdkFetchedDoc:
        await self._apply_config()
        # SDK delivers an SdkSearchResult dataclass; the adapter's own
        # SearchResult class has the same field layout so we re-wrap with
        # a duck-typed pass-through.
        from .adapter import SearchResult as LegacySearchResult, SourceKind as LegacyKind

        legacy = LegacySearchResult(
            adapter=result.adapter or "wiki_crawler",
            external_id=result.external_id,
            title=result.title,
            url=result.url,
            abstract=result.abstract or None,
            authors=list(result.authors),
            published_at=result.published_at,
            venue=result.venue or None,
            citation_count=result.citation_count or None,
            raw=dict(result.raw or {}),
            score=result.score or None,
            source_kind=LegacyKind.WIKI,
        )
        doc = await self._adapter.fetch(legacy)
        body = doc.body if isinstance(doc.body, (bytes, bytearray)) else str(doc.body).encode("utf-8")
        return SdkFetchedDoc(
            adapter=doc.adapter,
            external_id=doc.external_id,
            url=doc.url,
            title=doc.title,
            content_type=doc.content_type,
            body=bytes(body),
            source_tier=doc.source_tier,
            metadata=dict(doc.metadata),
        )
