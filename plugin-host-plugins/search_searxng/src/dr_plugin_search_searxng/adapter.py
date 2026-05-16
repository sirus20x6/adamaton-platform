"""SearXNG adapter (self-contained for the plugin substrate).

Copied from ``app/adapters/searxng.py`` with the legacy base HTTP /
protocol scaffolding inlined.

Talks to a SearXNG instance via its JSON output endpoint. Each fetched
URL is run through BeautifulSoup (chrome stripped) then markdownify so
the body the host gets is clean enough for LLM consumption.

Env: prefers ``SEARXNG_BASE_URL`` (new), falls back to ``SEARXNG_URL``
(legacy), then ``http://searxng:8080`` (default for docker compose).
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)

USER_AGENT = (
    "deepresearch-plugin-searxng/0.1.0 (+https://github.com/sirus20x6/deepresearch)"
)

_DEFAULT_URL = "http://searxng:8080"
_STRIP_TAGS = ("script", "style", "noscript", "nav", "header", "footer", "aside", "form")

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


def _stable_id(url: str) -> str:
    return hashlib.sha256(url.encode("utf-8", errors="replace")).hexdigest()[:16]


def _html_to_markdown(html: str) -> str:
    soup = BeautifulSoup(html, "lxml")
    for tag in soup(list(_STRIP_TAGS)):
        tag.decompose()
    main = soup.find("main") or soup.find("article") or soup.body or soup
    rendered = _markdownify(str(main), heading_style="ATX")
    # Collapse runs of blank lines so the markdown body stays compact.
    lines = [line.rstrip() for line in rendered.splitlines()]
    cleaned: list[str] = []
    blank = 0
    for line in lines:
        if line.strip():
            cleaned.append(line)
            blank = 0
        else:
            blank += 1
            if blank <= 1:
                cleaned.append("")
    return "\n".join(cleaned).strip() + "\n"


class SearxngConfigError(RuntimeError):
    """Raised when no SearXNG endpoint is configured."""


class SearxngAdapter:
    """Adapter over a SearXNG JSON endpoint."""

    name = "searxng"
    source_kind = SourceKind.WEB

    def __init__(self, *, base_url: str | None = None) -> None:
        env_url = (
            os.environ.get("SEARXNG_BASE_URL")
            or os.environ.get("SEARXNG_URL")
            or _DEFAULT_URL
        )
        url = (base_url or env_url).rstrip("/")
        headers = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        }
        try:
            self._client = httpx.AsyncClient(
                base_url=url,
                headers=headers,
                timeout=httpx.Timeout(15.0, connect=5.0),
                http2=True,
                follow_redirects=True,
            )
        except ImportError:
            self._client = httpx.AsyncClient(
                base_url=url,
                headers=headers,
                timeout=httpx.Timeout(15.0, connect=5.0),
                http2=False,
                follow_redirects=True,
            )
        self._instance_url = url

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

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()

    async def _get_json(
        self, url: str, *, params: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        async def _do() -> dict[str, Any]:
            response = await self._client.get(url, params=params)
            response.raise_for_status()
            data = response.json()
            return data if isinstance(data, dict) else {"data": data}

        return await self._retry(_do)

    async def _get_text(self, url: str) -> str:
        async def _do() -> str:
            response = await self._client.get(url)
            response.raise_for_status()
            return response.text

        return await self._retry(_do)

    @staticmethod
    def _parse_date(value: Any) -> datetime | None:
        if not value:
            return None
        try:
            return datetime.fromisoformat(str(value))
        except ValueError:
            return None

    def _to_result(self, item: dict[str, Any]) -> SearchResult | None:
        url = _first(item.get("url"))
        if not url:
            return None
        title = _first(item.get("title")) or url
        snippet = _first(item.get("content"))
        engines = item.get("engines") or item.get("engine") or []
        if isinstance(engines, str):
            engines = [engines]
        published = self._parse_date(item.get("publishedDate"))
        return SearchResult(
            adapter=self.name,
            external_id=_stable_id(url),
            title=title,
            url=url,
            abstract=snippet,
            authors=[],
            published_at=published,
            venue=None,
            citation_count=None,
            raw={**item, "engines": engines},
            score=item.get("score"),
            source_kind=SourceKind.WEB,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        # SearXNG paginates via ``pageno`` (1-indexed); we use cursor as that.
        page_no = int(cursor) if cursor and cursor.isdigit() else 1
        params: dict[str, Any] = {
            "q": query,
            "format": "json",
            "pageno": page_no,
        }
        data = await self._get_json("/search", params=params)
        items = data.get("results") or []
        out: list[SearchResult] = []
        for item in items:
            if not isinstance(item, dict):
                continue
            mapped = self._to_result(item)
            if mapped is not None:
                out.append(mapped)
            if len(out) >= int(limit):
                break
        next_cursor = str(page_no + 1) if out else None
        return SearchPage(
            results=out,
            next_cursor=next_cursor,
            total_estimated=data.get("number_of_results"),
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        # SearXNG only returns search snippets; the real body lives at the
        # original URL. Fetch + clean to markdown.
        html = await self._get_text(result.url)
        markdown = _html_to_markdown(html)
        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/markdown",
            body=markdown,
            source_tier="html",
            metadata={"engines": result.raw.get("engines", [])},
        )


__all__ = [
    "FetchedDoc",
    "SearchPage",
    "SearchResult",
    "SearxngAdapter",
    "SearxngConfigError",
    "SourceKind",
]
