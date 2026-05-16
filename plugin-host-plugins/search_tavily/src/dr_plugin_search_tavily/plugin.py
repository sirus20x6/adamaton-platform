"""SDK-wrapped Tavily plugin class.

The decorators register two RPCs (SearchQuery, SearchFetch). At the
gRPC boundary we translate the adapter's legacy dataclasses to the
SDK's dataclasses (``dr_plugin_sdk.types``) which the server then
serializes to ``dr.plugin.v1`` protos.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

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
    SourceKind as LegacySourceKind,
    TavilyAdapter,
)


# Map legacy string enum -> SDK IntEnum so the proto field gets a valid value.
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
    # The legacy adapter may emit str or bytes for ``body``; the SDK proto wants bytes.
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
    """Tavily search plugin. Instantiated by the SDK inside the event loop."""

    def __init__(self) -> None:
        # Adapter constructs lazily reading env (TAVILY_API_KEY / _BASE_URL);
        # the Hello RPC also stashes the host-supplied config on ``_dr_config``
        # but we keep env as the source of truth to match the legacy contract.
        self._adapter = TavilyAdapter()

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
        # The host hands us an SDK SearchResult; rebuild a legacy one so the
        # adapter can run unchanged. ``raw`` carries the original Tavily item.
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
