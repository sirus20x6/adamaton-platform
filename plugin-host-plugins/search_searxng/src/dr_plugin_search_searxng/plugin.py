"""SDK-wrapped SearXNG plugin class."""

from __future__ import annotations

from datetime import datetime

from dr_plugin_sdk import (
    FetchedDoc as SDKFetchedDoc,
    SearchPage as SDKSearchPage,
    SearchResult as SDKSearchResult,
    SourceKind as SDKSourceKind,
    plugin,
    search,
)

from .adapter import (
    FetchedDoc as LegacyFetchedDoc,
    SearchPage as LegacySearchPage,
    SearchResult as LegacySearchResult,
    SearxngAdapter,
    SourceKind as LegacySourceKind,
)


_SOURCE_KIND_MAP = {
    LegacySourceKind.JOURNAL: SDKSourceKind.JOURNAL,
    LegacySourceKind.PREPRINT: SDKSourceKind.PREPRINT,
    LegacySourceKind.REPO: SDKSourceKind.REPO,
    LegacySourceKind.FORUM: SDKSourceKind.FORUM,
    LegacySourceKind.WIKI: SDKSourceKind.WIKI,
    LegacySourceKind.WEB: SDKSourceKind.WEB,
}


def _to_sdk_result(r: LegacySearchResult) -> SDKSearchResult:
    return SDKSearchResult(
        adapter=r.adapter,
        external_id=r.external_id,
        title=r.title or "",
        url=r.url or "",
        abstract=r.abstract or "",
        authors=list(r.authors),
        published_at=r.published_at,
        venue=r.venue or "",
        citation_count=r.citation_count or 0,
        raw=dict(r.raw or {}),
        score=float(r.score) if r.score is not None else 0.0,
        source_kind=_SOURCE_KIND_MAP.get(r.source_kind, SDKSourceKind.WEB),
    )


def _to_sdk_page(p: LegacySearchPage) -> SDKSearchPage:
    return SDKSearchPage(
        results=[_to_sdk_result(r) for r in p.results],
        next_cursor=p.next_cursor or "",
        total_estimated=p.total_estimated or 0,
    )


def _to_sdk_doc(d: LegacyFetchedDoc) -> SDKFetchedDoc:
    body = d.body
    if isinstance(body, str):
        body = body.encode("utf-8")
    return SDKFetchedDoc(
        adapter=d.adapter,
        external_id=d.external_id,
        url=d.url or "",
        title=d.title or "",
        content_type=d.content_type or "",
        body=body or b"",
        source_tier=d.source_tier or "",
        metadata={k: v for k, v in (d.metadata or {}).items() if v is not None},
    )


@plugin(manifest="../../plugin.json")
class Plugin:
    """SearXNG search plugin."""

    def __init__(self) -> None:
        self._adapter = SearxngAdapter()

    @search.query
    async def query(
        self,
        *,
        q: str,
        limit: int = 10,
        cursor: str = "",
        since: str | None = None,
    ) -> SDKSearchPage:
        since_dt: datetime | None = None
        if since:
            try:
                since_dt = datetime.fromisoformat(since.replace("Z", "+00:00"))
            except ValueError:
                since_dt = None
        page = await self._adapter.search(
            q, limit=int(limit), cursor=cursor or None, since=since_dt
        )
        return _to_sdk_page(page)

    @search.fetch
    async def fetch(self, *, result: SDKSearchResult) -> SDKFetchedDoc:
        legacy = LegacySearchResult(
            adapter=result.adapter or self._adapter.name,
            external_id=result.external_id,
            title=result.title,
            url=result.url,
            abstract=result.abstract or None,
            authors=list(result.authors),
            published_at=result.published_at,
            venue=result.venue or None,
            citation_count=result.citation_count or None,
            raw=dict(result.raw or {}),
            score=result.score if result.score else None,
            source_kind=LegacySourceKind.WEB,
        )
        doc = await self._adapter.fetch(legacy)
        return _to_sdk_doc(doc)


__all__ = ["Plugin"]
