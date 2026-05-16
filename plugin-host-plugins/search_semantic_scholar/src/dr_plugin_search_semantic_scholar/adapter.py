"""Semantic Scholar adapter (legacy logic, lifted from app/adapters/semantic_scholar.py).

Self-contained — carries the BaseHttpAdapter retry helper and the
SearchResult/SearchPage/FetchedDoc dataclasses inline so the plugin has
no app.* imports.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar

import httpx
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)


# ----- shared types (copied from app.adapters.protocol) ---------------


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


# ----- _base helpers (copied minimally) -------------------------------

USER_AGENT = (
    "deepresearch-platform/0.1.0 (+https://github.com/sirus20x6/deepresearch)"
)

T = TypeVar("T")


def _first(text: str | None) -> str | None:
    if text is None:
        return None
    stripped = text.strip()
    return stripped or None


def _is_transient(exc: BaseException) -> bool:
    if isinstance(exc, httpx.HTTPStatusError):
        return exc.response.status_code in (429, 500, 502, 503, 504)
    return isinstance(
        exc,
        (
            httpx.ConnectError,
            httpx.ReadError,
            httpx.WriteError,
            httpx.RemoteProtocolError,
            httpx.ReadTimeout,
            httpx.ConnectTimeout,
            httpx.PoolTimeout,
        ),
    )


class _BaseHttpAdapter:
    def __init__(
        self,
        *,
        base_url: str | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | httpx.Timeout | None = None,
        http2: bool = True,
    ) -> None:
        merged_headers: dict[str, str] = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        }
        if headers:
            merged_headers.update(headers)
        try:
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged_headers,
                timeout=timeout if timeout is not None else httpx.Timeout(15.0, connect=5.0),
                http2=http2,
                follow_redirects=True,
            )
        except ImportError:
            # h2 missing — fall back to HTTP/1.1 transparently.
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged_headers,
                timeout=timeout if timeout is not None else httpx.Timeout(15.0, connect=5.0),
                http2=False,
                follow_redirects=True,
            )

    @property
    def client(self) -> httpx.AsyncClient:
        return self._client

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()

    async def _retry(self, op: Callable[[], Awaitable[T]]) -> T:
        async for attempt in AsyncRetrying(
            stop=stop_after_attempt(3),
            wait=wait_exponential(multiplier=0.5, min=0.5, max=8.0),
            retry=retry_if_exception(_is_transient),
            reraise=True,
        ):
            with attempt:
                return await op()
        raise RuntimeError("unreachable: AsyncRetrying exited without result")

    async def get_json(
        self,
        url: str,
        *,
        params: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        async def _do() -> dict[str, Any]:
            response = await self._client.get(url, params=params, headers=headers)
            response.raise_for_status()
            data = response.json()
            if not isinstance(data, dict):
                return {"data": data}
            return data

        return await self._retry(_do)

    async def get_bytes(
        self,
        url: str,
        *,
        params: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> bytes:
        async def _do() -> bytes:
            response = await self._client.get(url, params=params, headers=headers)
            response.raise_for_status()
            return response.content

        return await self._retry(_do)


# ----- Semantic Scholar adapter --------------------------------------

_BASE_URL = "https://api.semanticscholar.org/graph/v1"

_SEARCH_FIELDS = (
    "title,abstract,authors,year,externalIds,openAccessPdf,citationCount,venue,"
    "publicationDate,url"
)
_DETAIL_FIELDS = (
    "title,abstract,authors,year,externalIds,openAccessPdf,citationCount,venue,"
    "publicationDate,url,publicationVenue,fieldsOfStudy"
)


class SemanticScholarAdapter(_BaseHttpAdapter):
    name = "semantic_scholar"
    source_kind = SourceKind.JOURNAL

    def __init__(self, *, api_key: str | None = None) -> None:
        api_key = api_key if api_key is not None else os.environ.get(
            "SEMANTIC_SCHOLAR_API_KEY"
        )
        headers: dict[str, str] = {}
        if api_key:
            headers["x-api-key"] = api_key
        super().__init__(base_url=_BASE_URL, headers=headers)
        self._api_key = api_key

    @staticmethod
    def _parse_date(value: str | None, year: int | None) -> datetime | None:
        if value:
            try:
                return datetime.fromisoformat(value)
            except ValueError:
                pass
        if year:
            try:
                return datetime(int(year), 1, 1)
            except (TypeError, ValueError):
                return None
        return None

    @staticmethod
    def _build_url(paper: dict[str, Any]) -> str:
        url = _first(paper.get("url"))
        if url:
            return url
        external = paper.get("externalIds") or {}
        doi = external.get("DOI") if isinstance(external, dict) else None
        if doi:
            return f"https://doi.org/{doi}"
        paper_id = paper.get("paperId") or ""
        return f"https://www.semanticscholar.org/paper/{paper_id}"

    def _to_result(self, paper: dict[str, Any]) -> SearchResult:
        venue = _first(paper.get("venue"))
        kind = SourceKind.JOURNAL if venue else SourceKind.PREPRINT
        authors = []
        for author in paper.get("authors") or []:
            if not isinstance(author, dict):
                continue
            name = _first(author.get("name"))
            if name:
                authors.append(name)
        published = self._parse_date(paper.get("publicationDate"), paper.get("year"))
        return SearchResult(
            adapter=self.name,
            external_id=str(paper.get("paperId") or ""),
            title=_first(paper.get("title")) or "",
            url=self._build_url(paper),
            abstract=_first(paper.get("abstract")),
            authors=authors,
            published_at=published,
            venue=venue,
            citation_count=paper.get("citationCount"),
            raw=paper,
            score=None,
            source_kind=kind,
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
        params: dict[str, Any] = {
            "query": query,
            "limit": max(min(int(limit), 100), 1),
            "offset": offset,
            "fields": _SEARCH_FIELDS,
        }
        if since is not None:
            params["publicationDateOrYear"] = f"{since.date().isoformat()}:"

        data = await self.get_json("/paper/search", params=params)
        items = data.get("data") or []
        results = [self._to_result(p) for p in items if isinstance(p, dict)]
        next_token = data.get("next")
        next_cursor = str(next_token) if next_token is not None else None
        total = data.get("total")
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=int(total) if isinstance(total, int) else None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        params = {"fields": _DETAIL_FIELDS}
        paper = await self.get_json(f"/paper/{result.external_id}", params=params)

        oa = paper.get("openAccessPdf") if isinstance(paper, dict) else None
        pdf_url = _first(oa.get("url")) if isinstance(oa, dict) else None
        title = _first(paper.get("title")) or result.title
        url = self._build_url(paper) if isinstance(paper, dict) else result.url

        if pdf_url:
            try:
                pdf_bytes = await self.get_bytes(pdf_url)
                return FetchedDoc(
                    adapter=self.name,
                    external_id=result.external_id,
                    url=url,
                    title=title,
                    content_type="application/pdf",
                    body=pdf_bytes,
                    source_tier="pdf",
                    metadata={"pdf_url": pdf_url, "paper": paper},
                )
            except httpx.HTTPError:
                # Fall through to abstract markdown if the PDF download fails.
                pass

        abstract = _first(paper.get("abstract")) or _first(result.abstract) or ""
        body = f"# {title}\n\n{abstract}\n" if abstract else f"# {title}\n"
        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=url,
            title=title,
            content_type="text/markdown",
            body=body,
            source_tier="json",
            metadata={"paper": paper},
        )
