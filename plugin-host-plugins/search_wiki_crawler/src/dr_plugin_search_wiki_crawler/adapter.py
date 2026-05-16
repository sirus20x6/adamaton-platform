"""Adapter shim that exposes the three BFS crawlers as a SearchAdapter.

The legacy ``app/adapters/wiki_crawler.py`` is a crawler, not a search
adapter — it yields ``WikiPage`` objects from a BFS. To plug it into the
search.query / search.fetch RPC contract we map:

  query "<source>:<slug>"  →  BFS-crawl that source starting from <slug>,
                              capped at ``limit`` pages
  search.fetch(result)     →  re-fetch the single URL on the result and
                              return its body_markdown as FetchedDoc.body

The crawler internals (``LexicanumCrawler``, etc.) are imported verbatim
from the colocated ``wiki_crawler/`` subpackage — straight port, no
behavioral change.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, AsyncIterator

from .wiki_crawler import (
    FandomCrawler,
    LexicanumCrawler,
    TVTropesCrawler,
    WikiPage,
)


# ----- Local dataclasses (mirror app.adapters.protocol so the plugin is
#       self-contained and never imports from app.*). ---------------------


class SourceKind(str, Enum):
    JOURNAL = "journal"
    PREPRINT = "preprint"
    REPO = "repo"
    FORUM = "forum"
    WIKI = "wiki"
    WEB = "web"


@dataclass(kw_only=True, slots=True)
class SearchResult:
    adapter: str
    external_id: str
    title: str
    url: str
    abstract: str | None = None
    authors: list[str] = field(default_factory=list)
    published_at: datetime | None = None
    venue: str | None = None
    citation_count: int | None = None
    raw: dict[str, Any] = field(default_factory=dict)
    score: float | None = None
    source_kind: SourceKind = SourceKind.WIKI


@dataclass(kw_only=True, slots=True)
class SearchPage:
    results: list[SearchResult]
    next_cursor: str | None = None
    total_estimated: int | None = None


@dataclass(kw_only=True, slots=True)
class FetchedDoc:
    adapter: str
    external_id: str
    url: str
    title: str
    content_type: str
    body: bytes | str
    source_tier: str
    metadata: dict[str, Any] = field(default_factory=dict)


# ----- Query parsing ---------------------------------------------------


_SOURCES = ("lexicanum", "tvtropes", "fandom")
_DEFAULT_SOURCE = "lexicanum"


def _parse_query(query: str) -> tuple[str, str]:
    """Split ``<source>:<slug>`` syntax. Returns (source, slug)."""
    if ":" in query:
        head, _, tail = query.partition(":")
        head = head.strip().lower()
        if head in _SOURCES:
            return head, tail.strip()
    return _DEFAULT_SOURCE, query.strip()


# ----- WikiPage → SearchResult adaptor ---------------------------------


def _wikipage_to_result(page: WikiPage, adapter_name: str) -> SearchResult:
    # external_id: slug from metadata if available, else trailing URL component.
    ext_id = (
        page.metadata.get("slug")
        or page.metadata.get("path")
        or page.url.rstrip("/").split("/")[-1]
    )
    # Abstract: first non-empty paragraph or first ~300 chars of the body.
    body = page.body_markdown or ""
    abstract: str | None = None
    if body:
        for chunk in body.split("\n\n"):
            stripped = chunk.strip()
            if stripped and not stripped.startswith("#"):
                abstract = stripped[:500]
                break
        if not abstract:
            abstract = body[:300]
    return SearchResult(
        adapter=adapter_name,
        external_id=str(ext_id),
        title=page.title or str(ext_id),
        url=page.url,
        abstract=abstract,
        authors=[],
        published_at=None,
        venue=page.source,
        citation_count=None,
        raw={
            "source": page.source,
            "aliases": list(page.aliases),
            "categories": list(page.categories),
            "internal_links": list(page.internal_links)[:50],  # cap for transport
            "metadata": dict(page.metadata),
        },
        score=None,
        source_kind=SourceKind.WIKI,
    )


# ----- The adapter ------------------------------------------------------


class WikiCrawlerAdapter:
    """Multiplexes Lexicanum/TVTropes/Fandom under one search adapter.

    Each ``search()`` call spins up a fresh crawler scoped to one source,
    runs a BFS capped at ``limit`` pages, and returns the visited pages
    as a single SearchPage. Crawlers are not cached across calls because
    each call may target a different source and may hold different seed
    data on the queue — sharing state would tangle them.
    """

    name = "wiki_crawler"
    source_kind = SourceKind.WIKI

    def __init__(
        self,
        *,
        user_agent: str | None = None,
        max_pages_per_query: int = 25,
    ) -> None:
        self._user_agent = user_agent
        self._max_pages_per_query = int(max_pages_per_query)

    async def aclose(self) -> None:
        return None

    def _make_crawler(self, source: str):
        if source == "lexicanum":
            return LexicanumCrawler(user_agent=self._user_agent)
        if source == "tvtropes":
            return TVTropesCrawler(user_agent=self._user_agent)
        if source == "fandom":
            return FandomCrawler(user_agent=self._user_agent)
        raise ValueError(f"unknown wiki source: {source!r}")

    async def _crawl(
        self, source: str, slug: str, *, max_pages: int
    ) -> list[WikiPage]:
        crawler = self._make_crawler(source)
        pages: list[WikiPage] = []
        try:
            if source == "tvtropes":
                # TVTropes seeds are full paths, not slugs.
                seed = slug if slug.startswith("/") else f"/pmwiki/pmwiki.php/Main/{slug}"
                async for p in crawler.crawl(seed_path=seed, max_pages=max_pages):
                    pages.append(p)
                    if len(pages) >= max_pages:
                        break
            else:
                seeds: tuple[str, ...] = (slug,) if slug else ()
                # Fallback: when no slug is supplied we use the crawler's
                # built-in seed set (curated W40k / WHFB list).
                kwargs: dict[str, Any] = {"max_pages": max_pages}
                if seeds:
                    kwargs["seeds"] = seeds
                async for p in crawler.crawl(**kwargs):
                    pages.append(p)
                    if len(pages) >= max_pages:
                        break
        finally:
            await crawler.aclose()
        return pages

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        # Cursor: integer offset into the prior crawl. We always re-run
        # the BFS up to (offset + limit + 1) so we can tell whether more
        # pages exist beyond the slice we're returning. This matches the
        # arxiv adapter's offset-style cursor + has-more detection.
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        want = max(int(limit), 1)
        cap = min(offset + want + 1, self._max_pages_per_query)
        source, slug = _parse_query(query)

        pages = await self._crawl(source, slug, max_pages=cap)
        sliced = pages[offset : offset + want]
        results = [_wikipage_to_result(p, self.name) for p in sliced]
        next_cursor = (
            str(offset + len(sliced)) if len(pages) > offset + want else None
        )
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        """Re-fetch the page at ``result.url`` and return its markdown body.

        We can't recover the source from URL alone in every case
        (TVTropes has fixed namespaces, Fandom URLs vary by wiki), so we
        prefer the ``raw.source`` hint deposited by ``_wikipage_to_result``
        and fall back to URL-based detection.
        """
        source = (result.raw or {}).get("source") or _detect_source_from_url(result.url)
        if source is None:
            raise ValueError(f"cannot determine wiki source for {result.url!r}")

        crawler = self._make_crawler("fandom" if source == "warhammerfantasy_fandom" else source)
        try:
            page: WikiPage | None = None
            if isinstance(crawler, LexicanumCrawler):
                slug = result.external_id
                html = await crawler._fetch(f"/wiki/{slug}")
                if html is not None:
                    page = crawler._parse(html, title=slug, url=result.url)
            elif isinstance(crawler, TVTropesCrawler):
                path = (result.raw or {}).get("metadata", {}).get("path") or _path_from_url(result.url)
                html = await crawler._fetch(path)
                if html is not None:
                    page = crawler._parse(html, path=path)
            elif isinstance(crawler, FandomCrawler):
                slug = result.external_id
                page = await crawler.fetch_page(slug)
        finally:
            await crawler.aclose()

        if page is None:
            raise RuntimeError(f"failed to re-fetch {result.url!r}")

        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=result.url,
            title=page.title,
            content_type="text/markdown",
            body=page.body_markdown,
            source_tier="wiki",
            metadata={
                "source": page.source,
                "categories": page.categories,
                "aliases": page.aliases,
            },
        )


def _detect_source_from_url(url: str) -> str | None:
    if "lexicanum.com" in url:
        return "lexicanum"
    if "tvtropes.org" in url:
        return "tvtropes"
    if "fandom.com" in url:
        return "fandom"
    return None


def _path_from_url(url: str) -> str:
    from urllib.parse import urlparse

    return urlparse(url).path or "/"
