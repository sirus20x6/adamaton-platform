"""CORE adapter — CORE v3 /search/works over httpx.

CORE is an aggregator of open-access research papers (200M+ records).
API docs: https://api.core.ac.uk/docs/v3
"""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


SEARCH_URL = "https://api.core.ac.uk/v3/search/works"

ADAPTER_NAME = "core"
USER_AGENT = (
    "adamaton-deepresearch/search_core (+https://github.com/sirus20x6)"
)


def _first(text: str | None) -> str | None:
    if text is None:
        return None
    stripped = str(text).strip()
    return stripped or None


def _parse_published(value: Any) -> datetime | None:
    if not value:
        return None
    if isinstance(value, datetime):
        return value
    text = str(value).strip()
    if not text:
        return None
    # CORE typically emits ISO-8601 with optional trailing Z.
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        pass
    for fmt in ("%Y-%m-%d", "%Y-%m", "%Y"):
        try:
            return datetime.strptime(text, fmt)
        except ValueError:
            continue
    head = text[:4]
    if head.isdigit():
        try:
            return datetime(int(head), 1, 1)
        except ValueError:
            return None
    return None


def _authors_from_work(work: dict[str, Any]) -> list[str]:
    out: list[str] = []
    for a in work.get("authors") or []:
        if isinstance(a, dict):
            name = _first(a.get("name"))
            if name:
                out.append(name)
        elif isinstance(a, str):
            name = _first(a)
            if name:
                out.append(name)
    return out


def _url_from_work(work: dict[str, Any]) -> str:
    download = _first(work.get("downloadUrl"))
    if download:
        return download
    for link in work.get("links") or []:
        if not isinstance(link, dict):
            continue
        if (link.get("type") or "").lower() == "display":
            url = _first(link.get("url"))
            if url:
                return url
    # Last resort: any link url, then DOI.
    for link in work.get("links") or []:
        if isinstance(link, dict):
            url = _first(link.get("url"))
            if url:
                return url
    doi = _first(work.get("doi"))
    if doi:
        if doi.startswith("http"):
            return doi
        return f"https://doi.org/{doi}"
    return ""


class CoreAdapter:
    def __init__(self) -> None:
        api_key = os.environ.get("CORE_API_KEY")
        if not api_key:
            raise RuntimeError(
                "CORE_API_KEY environment variable is required for the CORE "
                "search plugin (search_core)."
            )
        self._api_key = api_key

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        if limit <= 0:
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        offset = 0
        if cursor:
            try:
                offset = max(0, int(cursor))
            except ValueError:
                offset = 0

        query = q or ""
        if since is not None:
            date_filter = f"publishedDate>={since.date().isoformat()}"
            query = f"({query}) AND {date_filter}" if query.strip() else date_filter

        params: dict[str, Any] = {
            "q": query,
            "limit": int(limit),
            "offset": offset,
        }
        headers = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
            "Authorization": f"Bearer {self._api_key}",
        }

        async with httpx.AsyncClient(timeout=30.0, headers=headers) as client:
            response = await client.get(SEARCH_URL, params=params)
            response.raise_for_status()
            data = response.json()

        if not isinstance(data, dict):
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        works = data.get("results") or []
        try:
            total = int(data.get("totalHits") or 0)
        except (TypeError, ValueError):
            total = 0

        results: list[SearchResult] = []
        for work in works:
            if not isinstance(work, dict):
                continue
            external_id = str(work.get("id") or "").strip()
            if not external_id:
                # Fall back to DOI to keep an identity around.
                external_id = _first(work.get("doi")) or ""
                if not external_id:
                    continue
            title = _first(work.get("title")) or ""
            abstract = _first(work.get("abstract")) or ""
            venue = _first(work.get("publisher")) or ""
            published = _parse_published(work.get("publishedDate"))
            results.append(
                SearchResult(
                    adapter=ADAPTER_NAME,
                    external_id=external_id,
                    title=title,
                    url=_url_from_work(work),
                    abstract=abstract,
                    authors=_authors_from_work(work),
                    published_at=published,
                    venue=venue,
                    raw=work,
                    source_kind=SourceKind.JOURNAL,
                )
            )

        next_offset = offset + len(works)
        next_cursor = str(next_offset) if next_offset < total and works else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )
