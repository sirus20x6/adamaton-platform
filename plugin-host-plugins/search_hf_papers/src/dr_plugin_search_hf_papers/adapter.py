"""Hugging Face Papers adapter (self-contained copy from legacy app/adapters).

HF Papers is a community-curated daily index over arxiv. The unique
signal it carries is engagement (upvotes + discussion + AI summaries)
plus a curated GitHub-repo link when present. Papers themselves live
on arxiv; fetch() routes straight at arxiv.org/pdf so the trust pipeline
sees source_tier=pdf rather than an HTML scrape.

API: GET https://huggingface.co/api/papers/search?q=... — keyless,
returns up to 120 hits per call.
"""

from __future__ import annotations

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

# ---- Local dataclasses mirroring the legacy app.adapters.protocol --------
# Plugin.py converts these into the SDK's dr_plugin_sdk.types equivalents
# on the wire boundary.


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


# ---- Minimal HTTP base (copy of relevant slice of legacy _base.py) -------

USER_AGENT = "deepresearch-platform/0.1.0 (+https://github.com/sirus20x6/deepresearch)"

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
        merged: dict[str, str] = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        }
        if headers:
            merged.update(headers)
        # http2=True wants the optional `h2` package; fall back gracefully.
        try:
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged,
                timeout=timeout if timeout is not None else httpx.Timeout(15.0, connect=5.0),
                http2=http2,
                follow_redirects=True,
            )
        except ImportError:
            self._client = httpx.AsyncClient(
                base_url=base_url or "",
                headers=merged,
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
            r = await self._client.get(url, params=params, headers=headers)
            r.raise_for_status()
            data = r.json()
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
            r = await self._client.get(url, params=params, headers=headers)
            r.raise_for_status()
            return r.content

        return await self._retry(_do)


# ---- HF Papers adapter (verbatim behavior from legacy) -------------------

_BASE_URL = "https://huggingface.co"


def _parse_iso(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    raw = value.replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(raw)
    except ValueError:
        return None


class HFPapersAdapter(_BaseHttpAdapter):
    name = "hf_papers"
    source_kind = SourceKind.PREPRINT

    def __init__(self) -> None:
        super().__init__(base_url=_BASE_URL)

    def _to_result(self, row: dict[str, Any]) -> SearchResult | None:
        paper = row.get("paper") if isinstance(row, dict) else None
        if not isinstance(paper, dict):
            return None

        arxiv_id = _first(paper.get("id"))
        if not arxiv_id:
            return None

        title = _first(paper.get("title")) or _first(row.get("title")) or ""
        if not title:
            return None
        title = " ".join(title.split())  # collapse hard-wrapped newlines

        summary = (
            _first(paper.get("ai_summary"))
            or _first(paper.get("summary"))
            or _first(row.get("summary"))
        )

        authors_raw = paper.get("authors") or []
        authors: list[str] = []
        if isinstance(authors_raw, list):
            for a in authors_raw:
                if isinstance(a, dict):
                    name = _first(a.get("name"))
                else:
                    name = _first(a) if isinstance(a, str) else None
                if name:
                    authors.append(name)

        upvotes = paper.get("upvotes") if isinstance(paper.get("upvotes"), int) else None

        url = f"https://huggingface.co/papers/{arxiv_id}"
        return SearchResult(
            adapter=self.name,
            external_id=arxiv_id,
            title=title,
            url=url,
            abstract=summary,
            authors=authors,
            published_at=_parse_iso(paper.get("publishedAt") or row.get("publishedAt")),
            venue="Hugging Face Papers",
            citation_count=upvotes,
            raw=row,
            score=float(upvotes) if upvotes is not None else None,
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
        # No native cursor on /api/papers/search; we slice the array locally
        # when callers pass cursor=offset.
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        params: dict[str, Any] = {"q": query}

        data = await self.get_json("/api/papers/search", params=params)
        if isinstance(data, dict):
            rows = data.get("data") or []
        elif isinstance(data, list):  # type: ignore[unreachable]
            rows = data
        else:
            rows = []

        if not isinstance(rows, list):
            rows = []

        results: list[SearchResult] = []
        for row in rows[offset : offset + max(int(limit), 1)]:
            if not isinstance(row, dict):
                continue
            sr = self._to_result(row)
            if sr is not None:
                results.append(sr)

        next_cursor: str | None = None
        if len(rows) > offset + len(results):
            next_cursor = str(offset + len(results))

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=len(rows) if rows else None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        # HF doesn't host the bytes; arxiv.org/pdf is the canonical source.
        arxiv_id = result.external_id
        pdf_url = f"https://arxiv.org/pdf/{arxiv_id}"
        try:
            pdf_bytes = await self.get_bytes(pdf_url)
        except httpx.HTTPError:
            pdf_bytes = b""

        title = result.title
        if pdf_bytes:
            return FetchedDoc(
                adapter=self.name,
                external_id=arxiv_id,
                url=result.url,
                title=title,
                content_type="application/pdf",
                body=pdf_bytes,
                source_tier="pdf",
                metadata={
                    "pdf_url": pdf_url,
                    "arxiv_id": arxiv_id,
                    "raw": result.raw,
                },
            )

        abstract = _first(result.abstract) or ""
        body = f"# {title}\n\n{abstract}\n" if abstract else f"# {title}\n"
        return FetchedDoc(
            adapter=self.name,
            external_id=arxiv_id,
            url=result.url,
            title=title,
            content_type="text/markdown",
            body=body,
            source_tier="json",
            metadata={"arxiv_id": arxiv_id, "raw": result.raw},
        )
