"""Readwise v2 highlights adapter.

Calls GET https://readwise.io/api/v2/highlights/ with the `search` query
parameter; the server returns a paginated list of highlight records. We
translate each highlight into an SDK ``SearchResult`` and pass the server's
``next`` URL cursor through to the next call.

Auth: requires a ``READWISE_TOKEN`` environment variable; sent as
``Authorization: Token <token>``.
"""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any
from urllib.parse import parse_qs, urlparse

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


READWISE_HIGHLIGHTS_URL = "https://readwise.io/api/v2/highlights/"
USER_AGENT = (
    "adamaton-deepresearch/search_readwise (+https://github.com/sirus20x6)"
)


def _parse_dt(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        # Readwise emits ISO-8601 timestamps, sometimes with a trailing Z.
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def _extract_cursor(next_url: str | None) -> str:
    if not next_url:
        return ""
    qs = parse_qs(urlparse(next_url).query)
    values = qs.get("page_cursor") or []
    return values[0] if values else ""


def _highlight_to_result(hit: dict[str, Any]) -> SearchResult:
    text = (hit.get("text") or "").strip()
    note = (hit.get("note") or "").strip()
    title = text[:80] if text else f"Readwise highlight {hit.get('id')}"

    abstract = text
    if note:
        abstract = f"{text}\n\nNote: {note}" if text else f"Note: {note}"

    book_id = hit.get("book_id")
    source_url = hit.get("url")
    if source_url:
        url = source_url
    elif book_id is not None:
        url = f"https://readwise.io/library/items/{book_id}"
    else:
        url = ""

    return SearchResult(
        adapter="readwise",
        external_id=str(hit.get("id")),
        title=title,
        url=url,
        abstract=abstract,
        published_at=_parse_dt(hit.get("highlighted_at")) or _parse_dt(hit.get("created_at")),
        venue="Readwise",
        raw=hit,
        source_kind=SourceKind.WIKI,
    )


class ReadwiseAdapter:
    name = "readwise"

    def __init__(self, token: str | None = None) -> None:
        self._token = token or os.environ.get("READWISE_TOKEN", "")
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={"User-Agent": USER_AGENT},
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        if not self._token:
            raise RuntimeError(
                "READWISE_TOKEN env var is required for the Readwise plugin."
            )

        params: dict[str, Any] = {
            "search": query,
            "page_size": max(1, min(int(limit), 1000)),
        }
        if cursor:
            params["page_cursor"] = cursor
        if since is not None:
            params["highlighted_at__gt"] = since.isoformat()

        headers = {"Authorization": f"Token {self._token}"}
        resp = await self._client.get(
            READWISE_HIGHLIGHTS_URL, params=params, headers=headers
        )
        resp.raise_for_status()
        payload = resp.json()

        results = [_highlight_to_result(h) for h in payload.get("results", []) or []]
        next_cursor = _extract_cursor(payload.get("next"))
        total = int(payload.get("count") or 0)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
