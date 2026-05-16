"""LinkUp web-search adapter (self-contained for the plugin substrate).

Copied from ``app/adapters/linkup.py`` with the shared HTTP scaffolding
and legacy protocol dataclasses inlined so this package doesn't import
from ``app.*``.

Endpoint: ``POST https://api.linkup.so/v1/search`` with JSON body
``{q, depth, outputType, includeImages, fromDate, toDate}``. ``depth``
is ``standard`` by default; the legacy fallback wrapper bumped it to
``deep`` when ``QueryHints.depth == 'comprehensive'``.
"""

from __future__ import annotations

import hashlib
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

USER_AGENT = (
    "deepresearch-plugin-linkup/0.1.0 (+https://github.com/sirus20x6/deepresearch)"
)

_BASE_URL = "https://api.linkup.so"

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


class LinkUpConfigError(RuntimeError):
    """Raised when ``LINKUP_API_KEY`` isn't configured."""


class LinkUpAdapter:
    """LinkUp search API adapter."""

    name = "linkup"
    source_kind = SourceKind.WEB

    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        depth: str | None = None,
    ) -> None:
        url = (base_url or os.environ.get("LINKUP_BASE_URL") or _BASE_URL).rstrip("/")
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
        self._api_key = api_key or os.environ.get("LINKUP_API_KEY")
        env_depth = os.environ.get("LINKUP_DEPTH")
        # set_depth() is the runtime knob; env_depth seeds it, ctor arg wins.
        chosen = depth or env_depth or "standard"
        self._depth = chosen if chosen in ("standard", "deep") else "standard"

    def _require_key(self) -> str:
        if not self._api_key:
            raise LinkUpConfigError(
                "LINKUP_API_KEY is not set; configure it or fall back to another provider"
            )
        return self._api_key

    def set_depth(self, depth: str) -> None:
        if depth in ("standard", "deep"):
            self._depth = depth

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

    def _to_result(self, item: dict[str, Any]) -> SearchResult | None:
        url = _first(item.get("url"))
        if not url:
            return None
        # LinkUp puts the title under ``name`` (not ``title``).
        title = _first(item.get("name")) or _first(item.get("title")) or url
        content = _first(item.get("content"))
        return SearchResult(
            adapter=self.name,
            external_id=_stable_id(url),
            title=title,
            url=url,
            abstract=content,
            authors=[],
            published_at=None,
            venue=None,
            citation_count=None,
            raw=item,
            score=None,
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
        api_key = self._require_key()
        body: dict[str, Any] = {
            "q": query,
            "depth": self._depth,
            "outputType": "searchResults",
            "includeImages": False,
        }
        if since is not None:
            body["fromDate"] = since.date().isoformat()

        async def _do() -> dict[str, Any]:
            response = await self._client.post(
                "/v1/search",
                json=body,
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "Content-Type": "application/json",
                },
            )
            response.raise_for_status()
            data = response.json()
            return data if isinstance(data, dict) else {"results": data}

        data = await self._retry(_do)
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
        return SearchPage(
            results=out,
            next_cursor=None,
            total_estimated=len(out),
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        body = result.abstract or _first(result.raw.get("content")) or ""
        return FetchedDoc(
            adapter=self.name,
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/plain",
            body=body,
            source_tier="api",
            metadata={"linkup_type": result.raw.get("type")},
        )


__all__ = [
    "FetchedDoc",
    "LinkUpAdapter",
    "LinkUpConfigError",
    "SearchPage",
    "SearchResult",
    "SourceKind",
]
