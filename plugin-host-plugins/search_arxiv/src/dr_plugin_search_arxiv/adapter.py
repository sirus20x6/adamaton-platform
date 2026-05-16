"""arXiv adapter (legacy logic, lifted out of app/adapters/arxiv.py).

The legacy code lived in app.adapters.arxiv and depended on
app.adapters.protocol. This file is self-contained: it carries its own
lightweight dataclasses so the plugin has no app.* imports.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any

import arxiv


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
    source_kind: SourceKind = SourceKind.WEB


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


class ArxivAdapter:
    name = "arxiv"
    source_kind = SourceKind.PREPRINT

    def __init__(self) -> None:
        self._client = arxiv.Client(page_size=50)

    @staticmethod
    def _canonical_id(entry_id: str) -> str:
        # entry_id looks like http://arxiv.org/abs/2401.01234v2
        tail = entry_id.rstrip("/").split("/")[-1]
        if "v" in tail and tail.rsplit("v", 1)[-1].isdigit():
            tail = tail.rsplit("v", 1)[0]
        return tail

    def _to_result(self, entry: arxiv.Result) -> SearchResult:
        external_id = self._canonical_id(entry.entry_id)
        published = entry.published if isinstance(entry.published, datetime) else None
        raw: dict[str, Any] = {
            "entry_id": entry.entry_id,
            "pdf_url": entry.pdf_url,
            "categories": list(entry.categories or []),
            "primary_category": entry.primary_category,
            "doi": entry.doi,
            "journal_ref": entry.journal_ref,
            "comment": entry.comment,
            "updated": entry.updated.isoformat() if entry.updated else None,
        }
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=(entry.title or "").strip(),
            url=entry.entry_id,
            abstract=(entry.summary or "").strip() or None,
            authors=[a.name for a in entry.authors],
            published_at=published,
            venue="arXiv",
            citation_count=None,
            raw=raw,
            score=None,
            source_kind=SourceKind.PREPRINT,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        max_results = max(int(limit), 1) + offset

        search = arxiv.Search(
            query=query,
            max_results=max_results,
            sort_by=arxiv.SortCriterion.Relevance,
            sort_order=arxiv.SortOrder.Descending,
        )

        loop = asyncio.get_running_loop()

        def _collect() -> list[arxiv.Result]:
            return list(self._client.results(search))

        entries = await loop.run_in_executor(None, _collect)

        if since is not None:
            entries = [e for e in entries if e.published and e.published >= since]

        sliced = entries[offset : offset + limit]
        results = [self._to_result(e) for e in sliced]
        next_cursor = str(offset + len(sliced)) if len(entries) > offset + limit else None

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        # The arxiv fetch cascade lives in r2g now; ingest-only, no return path.
        raise NotImplementedError(
            "ArxivAdapter.fetch is retired; use POST /platform/sources/ingest "
            f"with target=arxiv:{result.external_id} to ingest, then read "
            "the persisted document from platform.documents."
        )

    async def aclose(self) -> None:
        return None
