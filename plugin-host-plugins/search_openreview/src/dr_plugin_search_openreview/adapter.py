"""OpenReview adapter (legacy logic, lifted from app/adapters/openreview.py).

Self-contained: SearchResult/SearchPage/FetchedDoc + BaseHttpAdapter
retry helper inlined.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar

import httpx
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)


# ----- shared types ---------------------------------------------------


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


# ----- OpenReview adapter --------------------------------------------

_BASE_URL = "https://api.openreview.net"
_PAPER_LIMIT_MAX = 100  # API hard limit per page

_KNOWN_VENUES: dict[str, str] = {
    "NeurIPS": "NeurIPS",
    "ICLR": "ICLR",
    "ICML": "ICML",
    "COLM": "COLM",
    "TMLR": "TMLR",
    "AISTATS": "AISTATS",
    "UAI": "UAI",
}


def _venue_from_invitation(invitation: str | None) -> str | None:
    """Pluck a venue name out of an OpenReview invitation string."""
    if not invitation:
        return None
    head = invitation.split("/", 1)[0]
    head = head.removesuffix(".cc")
    return _KNOWN_VENUES.get(head, head or None)


def _stringify(value: Any) -> str | None:
    """Coerce a v1 content-field value into a plain string.

    v1 stores values inline (content.title = "Foo") but newer venues nest them
    in a {"value": ...} envelope. Handle both so a future schema change
    doesn't silently zero out our titles.
    """
    if value is None:
        return None
    if isinstance(value, str):
        return _first(value)
    if isinstance(value, dict):
        inner = value.get("value")
        if isinstance(inner, str):
            return _first(inner)
    return None


class OpenReviewAdapter(_BaseHttpAdapter):
    name = "openreview"
    source_kind = SourceKind.JOURNAL

    def __init__(self) -> None:
        username = os.environ.get("OPENREVIEW_USERNAME")
        password = os.environ.get("OPENREVIEW_PASSWORD")
        super().__init__(base_url=_BASE_URL)
        self._auth = (username, password) if username and password else None
        self._token: str | None = None

    @staticmethod
    def _parse_cdate(cdate: Any) -> datetime | None:
        """OpenReview cdate is epoch ms (int)."""
        if cdate is None:
            return None
        try:
            millis = int(cdate)
        except (TypeError, ValueError):
            return None
        if millis <= 0:
            return None
        return datetime.fromtimestamp(millis / 1000, tz=timezone.utc)

    def _to_result(self, note: dict[str, Any]) -> SearchResult | None:
        if not isinstance(note, dict):
            return None
        content = note.get("content") or {}

        title = _stringify(content.get("title"))
        if not title:
            return None

        abstract = _stringify(content.get("abstract"))
        tldr = _stringify(content.get("TL;DR"))
        if tldr and abstract:
            abstract = f"{tldr}\n\n{abstract}"
        elif tldr:
            abstract = tldr

        authors_raw = content.get("authors")
        authors: list[str] = []
        if isinstance(authors_raw, list):
            for a in authors_raw:
                name = _first(a) if isinstance(a, str) else _stringify(a)
                if name:
                    authors.append(name)
        elif isinstance(authors_raw, dict):
            inner = authors_raw.get("value")
            if isinstance(inner, list):
                authors.extend(_first(x) for x in inner if isinstance(x, str))
                authors = [a for a in authors if a]

        invitation = note.get("invitation") if isinstance(note.get("invitation"), str) else None
        venue = _venue_from_invitation(invitation)

        forum_id = note.get("forum") or note.get("id")
        external_id = str(forum_id) if forum_id else str(note.get("id") or "")
        url = f"https://openreview.net/forum?id={external_id}" if external_id else ""

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract=abstract,
            authors=authors,
            published_at=self._parse_cdate(note.get("cdate")),
            venue=venue,
            citation_count=None,
            raw=note,
            score=None,
            source_kind=SourceKind.JOURNAL,
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
            "term": query,
            "limit": max(min(int(limit), _PAPER_LIMIT_MAX), 1),
            "offset": offset,
        }

        data = await self.get_json("/notes/search", params=params)
        notes = data.get("notes") or []
        results: list[SearchResult] = []
        for note in notes:
            row = self._to_result(note) if isinstance(note, dict) else None
            if row is not None:
                results.append(row)

        next_cursor: str | None = None
        if results:
            next_cursor = str(offset + len(notes))

        total = data.get("count")
        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=int(total) if isinstance(total, int) else None,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        external_id = result.external_id
        pdf_url = f"{_BASE_URL}/pdf?id={external_id}"
        try:
            pdf_bytes = await self.get_bytes(pdf_url)
        except httpx.HTTPError:
            pdf_bytes = b""

        title = result.title
        abstract = _first(result.abstract) or ""
        reviews_md = await self._fetch_reviews_md(external_id)

        if pdf_bytes:
            return FetchedDoc(
                adapter=self.name,
                external_id=external_id,
                url=result.url,
                title=title,
                content_type="application/pdf",
                body=pdf_bytes,
                source_tier="pdf",
                metadata={
                    "pdf_url": pdf_url,
                    "reviews_markdown": reviews_md,
                    "venue": result.venue,
                },
            )

        body_parts = [f"# {title}\n"]
        if abstract:
            body_parts.append(abstract.rstrip() + "\n")
        if reviews_md:
            body_parts.append("\n## Peer reviews\n")
            body_parts.append(reviews_md.rstrip() + "\n")
        body = "\n".join(body_parts)
        return FetchedDoc(
            adapter=self.name,
            external_id=external_id,
            url=result.url,
            title=title,
            content_type="text/markdown",
            body=body,
            source_tier="json",
            metadata={"venue": result.venue},
        )

    async def _fetch_reviews_md(self, forum_id: str) -> str:
        """Fetch + format the review thread for forum_id as markdown.

        Empty string when there are no public reviews (rejected papers,
        in-review submissions).
        """
        if not forum_id:
            return ""
        try:
            data = await self.get_json(
                "/notes",
                params={"forum": forum_id, "limit": 100},
            )
        except httpx.HTTPError:
            return ""

        notes = data.get("notes") or []
        out: list[str] = []
        for note in notes:
            if not isinstance(note, dict):
                continue
            invitation = str(note.get("invitation") or "")
            # Only surface /Official_Review or /Review notes; comments + meta-reviews
            # bloat the digest.
            if "Review" not in invitation:
                continue
            content = note.get("content") or {}
            heading = _stringify(content.get("title")) or invitation.split("/")[-1]
            rating = _stringify(content.get("rating"))
            confidence = _stringify(content.get("confidence"))
            summary = _stringify(content.get("summary")) or _stringify(content.get("review"))
            weaknesses = _stringify(content.get("weaknesses"))
            strengths = _stringify(content.get("strengths"))

            block = [f"### {heading}"]
            if rating:
                block.append(f"- **Rating:** {rating}")
            if confidence:
                block.append(f"- **Confidence:** {confidence}")
            if summary:
                block.append("\n" + summary.strip())
            if strengths:
                block.append("\n**Strengths.** " + strengths.strip())
            if weaknesses:
                block.append("\n**Weaknesses.** " + weaknesses.strip())
            out.append("\n".join(block))
        return "\n\n".join(out)
