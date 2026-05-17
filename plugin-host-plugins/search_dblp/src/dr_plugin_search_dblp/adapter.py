"""DBLP search API adapter.

Hits the public DBLP search endpoint
(https://dblp.org/search/publ/api) which returns JSON when ``format=json``.
The response shape is ``result.hits.hit[]`` where each hit has an ``info``
block containing ``title``, ``authors.author[]``, ``venue``, ``year``,
``doi`` (optional), ``url``, and ``key``.

DBLP does not support a native date filter, so we pass ``since`` through
and apply it client-side against ``info.year``. Pagination uses ``h``
(hit count) and ``f`` (offset); the cursor we round-trip is the next
offset as a decimal string.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


DBLP_SEARCH_URL = "https://dblp.org/search/publ/api"
USER_AGENT = (
    "adamaton-deepresearch/search_dblp "
    "(+https://github.com/sirus20x6)"
)


class DblpAdapter:
    name = "dblp"
    source_kind = SourceKind.JOURNAL

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _parse_offset(cursor: str | None) -> int:
        if not cursor:
            return 0
        try:
            v = int(cursor)
            return v if v >= 0 else 0
        except (TypeError, ValueError):
            return 0

    @staticmethod
    def _authors(info: dict[str, Any]) -> list[str]:
        block = info.get("authors")
        if not isinstance(block, dict):
            return []
        raw = block.get("author")
        if raw is None:
            return []
        # DBLP returns a single dict when there's exactly one author and a
        # list of dicts otherwise. Normalize both shapes (and tolerate the
        # occasional bare string) into a flat list of names.
        if isinstance(raw, dict):
            raw = [raw]
        elif not isinstance(raw, list):
            return []
        out: list[str] = []
        for a in raw:
            if isinstance(a, dict):
                text = a.get("text")
                if isinstance(text, str) and text.strip():
                    out.append(text.strip())
            elif isinstance(a, str):
                t = a.strip()
                if t:
                    out.append(t)
        return out

    @staticmethod
    def _parse_year(info: dict[str, Any]) -> tuple[int | None, datetime | None]:
        raw = info.get("year")
        if raw is None:
            return None, None
        try:
            year = int(raw)
        except (TypeError, ValueError):
            return None, None
        try:
            return year, datetime(year, 1, 1, tzinfo=timezone.utc)
        except ValueError:
            return year, None

    def _to_result(self, hit: dict[str, Any]) -> SearchResult | None:
        info = hit.get("info")
        if not isinstance(info, dict):
            return None
        key = (info.get("key") or hit.get("@id") or "").strip()
        url = (info.get("url") or "").strip()
        external_id = key or url
        if not external_id:
            return None

        title = (info.get("title") or "").strip()
        venue_raw = info.get("venue")
        if isinstance(venue_raw, list):
            venue = ", ".join(str(v) for v in venue_raw if v)
        else:
            venue = (venue_raw or "").strip() if isinstance(venue_raw, str) else ""

        _year, published_at = self._parse_year(info)

        try:
            score = float(hit.get("@score")) if hit.get("@score") is not None else 0.0
        except (TypeError, ValueError):
            score = 0.0

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            authors=self._authors(info),
            published_at=published_at,
            venue=venue,
            raw=hit,
            score=score,
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
        rows = max(int(limit), 1)
        offset = self._parse_offset(cursor)

        params: dict[str, Any] = {
            "q": query,
            "format": "json",
            "h": rows,
            "f": offset,
        }

        resp = await self._client.get(DBLP_SEARCH_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        result_block = payload.get("result") or {}
        hits_block = result_block.get("hits") or {}
        raw_hits = hits_block.get("hit") or []
        if isinstance(raw_hits, dict):
            raw_hits = [raw_hits]
        elif not isinstance(raw_hits, list):
            raw_hits = []

        results: list[SearchResult] = []
        since_year = since.year if since is not None else None
        for hit in raw_hits:
            if not isinstance(hit, dict):
                continue
            r = self._to_result(hit)
            if r is None:
                continue
            if since_year is not None:
                # Client-side year filter: drop anything strictly older.
                pub = r.published_at
                if pub is None or pub.year < since_year:
                    continue
            results.append(r)

        # Pagination: DBLP returns @total, @sent, @first as strings.
        total_estimated = 0
        sent = len(raw_hits)
        first_idx = offset
        try:
            total_raw = hits_block.get("@total")
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0
        try:
            sent_raw = hits_block.get("@sent")
            if sent_raw is not None:
                sent = int(sent_raw)
        except (TypeError, ValueError):
            pass
        try:
            first_raw = hits_block.get("@first")
            if first_raw is not None:
                first_idx = int(first_raw)
        except (TypeError, ValueError):
            pass

        next_offset = first_idx + sent
        if sent <= 0 or (total_estimated and next_offset >= total_estimated):
            next_cursor = ""
        else:
            next_cursor = str(next_offset)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
