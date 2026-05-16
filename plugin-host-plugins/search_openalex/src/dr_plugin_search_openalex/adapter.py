"""OpenAlex adapter (legacy logic, lifted from app/adapters/openalex.py).

Self-contained: SearchResult/SearchPage/FetchedDoc + BaseHttpAdapter
retry helper inlined.
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


# ----- _base helpers --------------------------------------------------

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
        raise RuntimeError("unreachable")

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


# ----- OpenAlex adapter ----------------------------------------------

_BASE_URL = "https://api.openalex.org"
_DEFAULT_MAILTO = "sirus20x6@gmail.com"


def _reconstruct_abstract(inverted: dict[str, list[int]] | None) -> str | None:
    """Convert OpenAlex's word -> [positions] map back into prose."""
    if not inverted:
        return None
    position_word: dict[int, str] = {}
    for word, positions in inverted.items():
        if not isinstance(positions, list):
            continue
        for pos in positions:
            if isinstance(pos, int):
                position_word[pos] = word
    if not position_word:
        return None
    return " ".join(position_word[i] for i in sorted(position_word))


class OpenAlexAdapter(_BaseHttpAdapter):
    name = "openalex"
    source_kind = SourceKind.JOURNAL

    def __init__(self, *, mailto: str | None = None) -> None:
        super().__init__(base_url=_BASE_URL)
        self._mailto = mailto or os.environ.get("OPENALEX_MAILTO") or _DEFAULT_MAILTO

    @staticmethod
    def _parse_date(value: Any) -> datetime | None:
        if not value:
            return None
        if isinstance(value, datetime):
            return value
        text = str(value)
        try:
            return datetime.fromisoformat(text)
        except ValueError:
            return None

    @staticmethod
    def _classify(work: dict[str, Any]) -> SourceKind:
        primary = work.get("primary_location") or {}
        source = primary.get("source") if isinstance(primary, dict) else None
        if isinstance(source, dict):
            host = source.get("host_organization") or source.get("host_organization_name")
            if host:
                return SourceKind.JOURNAL
            stype = (source.get("type") or "").lower()
            if stype in {"journal", "book series", "conference", "ebook platform"}:
                return SourceKind.JOURNAL
        return SourceKind.PREPRINT

    def _to_result(self, work: dict[str, Any]) -> SearchResult:
        external_id = str(work.get("id") or "")
        title = _first(work.get("title")) or _first(work.get("display_name")) or ""
        doi = _first(work.get("doi"))
        url = doi if doi and doi.startswith("http") else (
            f"https://doi.org/{doi}" if doi else external_id
        )
        authors: list[str] = []
        for authorship in work.get("authorships") or []:
            if not isinstance(authorship, dict):
                continue
            author = authorship.get("author") or {}
            name = _first(author.get("display_name")) if isinstance(author, dict) else None
            if name:
                authors.append(name)
        primary = work.get("primary_location") or {}
        venue = None
        if isinstance(primary, dict):
            source = primary.get("source") or {}
            if isinstance(source, dict):
                venue = _first(source.get("display_name"))
        published = self._parse_date(work.get("publication_date"))
        if not published and work.get("publication_year"):
            try:
                published = datetime(int(work["publication_year"]), 1, 1)
            except (TypeError, ValueError):
                published = None
        abstract = _reconstruct_abstract(work.get("abstract_inverted_index"))
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url or external_id,
            abstract=abstract,
            authors=authors,
            published_at=published,
            venue=venue,
            citation_count=work.get("cited_by_count"),
            raw=work,
            score=work.get("relevance_score"),
            source_kind=self._classify(work),
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        params: dict[str, Any] = {
            "search": query,
            "per-page": max(min(int(limit), 200), 1),
            "mailto": self._mailto,
        }
        if cursor:
            params["cursor"] = cursor
        else:
            params["cursor"] = "*"
        if since is not None:
            params["filter"] = f"from_publication_date:{since.date().isoformat()}"

        data = await self.get_json("/works", params=params)
        works = data.get("results") or []
        results = [self._to_result(w) for w in works if isinstance(w, dict)]
        meta = data.get("meta") or {}
        next_cursor = _first(meta.get("next_cursor")) if isinstance(meta, dict) else None
        total = meta.get("count") if isinstance(meta, dict) else None
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=int(total) if isinstance(total, int) else None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        raw = result.raw or {}
        primary = raw.get("primary_location") or {}
        pdf_url = None
        if isinstance(primary, dict):
            pdf_url = _first(primary.get("pdf_url"))
        if not pdf_url:
            best = raw.get("best_oa_location") or {}
            if isinstance(best, dict):
                pdf_url = _first(best.get("pdf_url"))

        if pdf_url:
            try:
                pdf_bytes = await self.get_bytes(pdf_url)
                return FetchedDoc(
                    adapter=self.name,
                    external_id=result.external_id,
                    url=result.url,
                    title=result.title,
                    content_type="application/pdf",
                    body=pdf_bytes,
                    source_tier="pdf",
                    metadata={"pdf_url": pdf_url},
                )
            except httpx.HTTPError:
                pass

        abstract = result.abstract or _reconstruct_abstract(
            raw.get("abstract_inverted_index")
        ) or ""
        body = f"# {result.title}\n\n{abstract}\n" if abstract else f"# {result.title}\n"
        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/markdown",
            body=body,
            source_tier="json",
            metadata={"work_id": result.external_id},
        )
