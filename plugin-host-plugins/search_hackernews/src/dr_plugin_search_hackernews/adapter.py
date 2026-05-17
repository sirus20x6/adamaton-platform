"""Async adapter for the Algolia Hacker News Search API."""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind

_ADAPTER = "hackernews"
_VENUE = "Hacker News"
_ENDPOINT = "https://hn.algolia.com/api/v1/search"
_USER_AGENT = (
    "adamaton-deepresearch/search_hackernews (+https://github.com/sirus20x6)"
)


def _parse_published(value: Any) -> datetime | None:
    if not value:
        return None
    if isinstance(value, datetime):
        return value
    try:
        # Algolia returns ISO-8601 like "2024-03-15T12:34:56.000Z".
        s = str(value).replace("Z", "+00:00")
        return datetime.fromisoformat(s)
    except (TypeError, ValueError):
        return None


def _parse_page(cursor: str | None) -> int:
    if not cursor:
        return 0
    try:
        return max(0, int(cursor))
    except ValueError:
        return 0


def _hit_to_result(hit: dict[str, Any]) -> SearchResult:
    object_id = str(hit.get("objectID") or "")
    title = hit.get("title") or hit.get("story_title") or ""
    url = hit.get("url") or hit.get("story_url") or ""
    if not url and object_id:
        url = f"https://news.ycombinator.com/item?id={object_id}"
    author = hit.get("author")
    authors = [author] if author else []
    abstract = hit.get("story_text") or hit.get("comment_text") or ""
    points = hit.get("points")
    score = 0.0
    if isinstance(points, (int, float)):
        score = float(points)

    return SearchResult(
        adapter=_ADAPTER,
        external_id=object_id,
        title=title,
        url=url,
        abstract=abstract,
        authors=authors,
        published_at=_parse_published(hit.get("created_at")),
        venue=_VENUE,
        citation_count=0,
        raw=hit,
        score=score,
        source_kind=SourceKind.FORUM,
    )


class HackerNewsAdapter:
    """Thin async wrapper over the Algolia HN search endpoint."""

    def __init__(self, client: httpx.AsyncClient | None = None) -> None:
        self._client = client

    async def _get_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(
                timeout=30.0,
                headers={"User-Agent": _USER_AGENT},
            )
        return self._client

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        page = _parse_page(cursor)
        hits_per_page = max(1, min(int(limit or 10), 100))

        params: dict[str, Any] = {
            "query": q or "",
            "page": page,
            "hitsPerPage": hits_per_page,
        }
        if since is not None:
            ts = since
            if ts.tzinfo is None:
                ts = ts.replace(tzinfo=timezone.utc)
            unix_ts = int(ts.timestamp())
            params["numericFilters"] = f"created_at_i>{unix_ts}"

        client = await self._get_client()
        response = await client.get(_ENDPOINT, params=params)
        response.raise_for_status()
        payload = response.json()

        hits = payload.get("hits") or []
        results = [_hit_to_result(h) for h in hits if isinstance(h, dict)]

        nb_hits = payload.get("nbHits")
        total_estimated = int(nb_hits) if isinstance(nb_hits, (int, float)) else 0

        nb_pages = payload.get("nbPages")
        try:
            nb_pages_int = int(nb_pages) if nb_pages is not None else None
        except (TypeError, ValueError):
            nb_pages_int = None

        next_cursor = ""
        if hits and (nb_pages_int is None or page + 1 < nb_pages_int):
            next_cursor = str(page + 1)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None
