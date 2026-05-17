"""IETF Datatracker adapter.

Calls the public Datatracker REST API at
https://datatracker.ietf.org/api/v1/doc/document/ with type=rfc and an
icontains filter on the title. Results are SDK SearchResult/SearchPage.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_API_URL = "https://datatracker.ietf.org/api/v1/doc/document/"
_USER_AGENT = (
    "adamaton-deepresearch/search_rfc (+https://github.com/sirus20x6)"
)


def _parse_dt(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    # Datatracker emits ISO-8601 timestamps like "2021-05-27T14:00:00".
    # Some legacy rows include a trailing "Z". Accept both via fromisoformat
    # after stripping the trailing Z (Python 3.11+ would parse it natively).
    candidate = value.rstrip("Z")
    try:
        return datetime.fromisoformat(candidate)
    except ValueError:
        return None


class RfcAdapter:
    name = "rfc"
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={"User-Agent": _USER_AGENT, "Accept": "application/json"},
        )

    def _to_result(self, doc: dict[str, Any]) -> SearchResult:
        name = (doc.get("name") or "").strip()
        title = (doc.get("title") or "").strip()
        url = f"https://datatracker.ietf.org/doc/{name}/" if name else ""

        # Published date: prefer rfc.published (actual RFC publication), fall
        # back to the document's `time` (last revision timestamp).
        rfc_block = doc.get("rfc") if isinstance(doc.get("rfc"), dict) else None
        published_at: datetime | None = None
        if rfc_block:
            published_at = _parse_dt(rfc_block.get("published"))
        if published_at is None:
            published_at = _parse_dt(doc.get("time"))

        # authors: list of API resource URIs in the Datatracker response.
        # Resolving them requires extra HTTP calls; v0.1.0 leaves authors
        # empty per spec.
        return SearchResult(
            adapter=self.name,
            external_id=name,
            title=title,
            url=url,
            abstract="",
            authors=[],
            published_at=published_at,
            venue="IETF RFC",
            raw=doc,
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
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        page_limit = max(int(limit), 1)

        params: dict[str, Any] = {
            "type": "rfc",
            "format": "json",
            "limit": page_limit,
            "offset": offset,
            "title__icontains": query,
        }
        if since is not None:
            params["time__gte"] = since.strftime("%Y-%m-%dT00:00:00")

        resp = await self._client.get(_API_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        objects = payload.get("objects") or []
        meta = payload.get("meta") or {}
        total = int(meta.get("total_count") or 0)

        results = [self._to_result(d) for d in objects if isinstance(d, dict)]

        # Pagination: meta.next is a relative URL when more pages exist.
        next_cursor = ""
        if meta.get("next"):
            next_cursor = str(offset + len(results))

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
