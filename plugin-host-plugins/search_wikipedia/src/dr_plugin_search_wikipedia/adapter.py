"""Wikipedia adapter (self-contained copy from legacy
app/adapters/wikipedia.py).

``search()`` hits the legacy ``opensearch`` endpoint (4-tuple of
``[query, [titles], [descriptions], [urls]]``). ``fetch()`` retrieves
parsed HTML through ``/api/rest_v1/page/html/{title}`` and markdownifies
it after stripping reference/infobox cruft.

Config via env: ``WIKIPEDIA_LANGUAGE`` (subdomain, default ``en``).
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any, Awaitable, Callable, TypeVar
from urllib.parse import quote

import httpx
from bs4 import BeautifulSoup
from markdownify import markdownify as _markdownify
from tenacity import (
    AsyncRetrying,
    retry_if_exception,
    stop_after_attempt,
    wait_exponential,
)


# ---- Local protocol mirrors --------------------------------------------


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


# ---- HTTP base ----------------------------------------------------------

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

    async def get_text(
        self,
        url: str,
        *,
        params: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> str:
        async def _do() -> str:
            r = await self._client.get(url, params=params, headers=headers)
            r.raise_for_status()
            return r.text

        return await self._retry(_do)


# ---- Wikipedia adapter --------------------------------------------------

_API_BASE = "https://en.wikipedia.org"
_OPENSEARCH = "/w/api.php"
_PAGE_HTML = "/api/rest_v1/page/html/{title}"

_STRIP_SELECTORS = (
    "table.infobox",
    "table.navbox",
    "table.metadata",
    "div.hatnote",
    "div.thumb",
    "div.reflist",
    "ol.references",
    "sup.reference",
    "span.mw-editsection",
    "div.navbox",
    "div.printfooter",
    "div#toc",
)


def _wiki_html_to_markdown(html: str) -> str:
    soup = BeautifulSoup(html, "lxml")
    for selector in _STRIP_SELECTORS:
        for node in soup.select(selector):
            node.decompose()
    body = soup.find("section") or soup.body or soup
    rendered = _markdownify(str(body), heading_style="ATX")
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


class WikipediaAdapter(_BaseHttpAdapter):
    name = "wikipedia"
    source_kind = SourceKind.WIKI

    def __init__(self, *, language: str | None = None) -> None:
        # WIKIPEDIA_LANGUAGE env knob is new; legacy hardcoded the kwarg.
        lang = language or os.environ.get("WIKIPEDIA_LANGUAGE") or "en"
        base = _API_BASE if lang == "en" else f"https://{lang}.wikipedia.org"
        super().__init__(base_url=base)
        self._language = lang
        self._site_root = base.rstrip("/")

    def _to_result(
        self, title: str, desc: str | None, url: str | None
    ) -> SearchResult:
        clean_title = title or ""
        link = url or f"{self._site_root}/wiki/{quote(clean_title.replace(' ', '_'))}"
        return SearchResult(
            adapter=self.name,
            external_id=clean_title,
            title=clean_title,
            url=str(link),
            abstract=_first(desc),
            authors=[],
            published_at=None,
            venue="Wikipedia",
            citation_count=None,
            raw={"title": clean_title, "description": desc, "url": url},
            score=None,
            source_kind=SourceKind.WIKI,
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
            "action": "opensearch",
            "search": query,
            "limit": max(min(int(limit), 50), 1),
            "format": "json",
        }
        # opensearch returns a bare JSON array; use raw text to avoid the
        # {"data": ...} wrapping the base class applies to non-dict bodies.
        text = await self.get_text(_OPENSEARCH, params=params)
        try:
            import json as _json

            payload = _json.loads(text)
        except ValueError:
            payload = []

        titles: list[str] = []
        descs: list[str] = []
        urls: list[str] = []
        if isinstance(payload, list) and len(payload) >= 4:
            titles = [str(t) for t in (payload[1] or [])]
            descs = [str(d) for d in (payload[2] or [])]
            urls = [str(u) for u in (payload[3] or [])]

        results: list[SearchResult] = []
        for idx, title in enumerate(titles):
            desc = descs[idx] if idx < len(descs) else None
            url = urls[idx] if idx < len(urls) else None
            results.append(self._to_result(title, desc, url))
        return SearchPage(results=results, next_cursor=None, total_estimated=None)

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        title = result.external_id or result.title
        path = _PAGE_HTML.format(title=quote(title.replace(" ", "_"), safe=""))
        html = await self.get_text(path)
        markdown = _wiki_html_to_markdown(html)
        return FetchedDoc(
            adapter=self.name,
            external_id=title,
            url=result.url,
            title=title,
            content_type="text/markdown",
            body=markdown,
            source_tier="html",
            metadata={"language": self._language},
        )
