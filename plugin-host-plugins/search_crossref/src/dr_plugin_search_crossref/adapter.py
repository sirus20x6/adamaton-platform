"""Crossref REST API adapter.

Hits the public Crossref API (https://api.crossref.org/works) with a polite
User-Agent that includes a contact email. Pagination uses Crossref's cursor
mechanism: pass ``cursor=*`` on the first call, and Crossref returns a
``next-cursor`` that we echo back on the next page.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


CROSSREF_WORKS_URL = "https://api.crossref.org/works"
USER_AGENT = (
    "adamaton-deepresearch/search_crossref "
    "(https://github.com/sirus20x6; mailto:sirus20x6@gmail.com)"
)


class CrossrefAdapter:
    name = "crossref"
    source_kind = SourceKind.JOURNAL

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _first(seq: Any) -> str:
        if isinstance(seq, list) and seq:
            v = seq[0]
            return v if isinstance(v, str) else str(v)
        return ""

    @staticmethod
    def _parse_published(item: dict[str, Any]) -> datetime | None:
        # Crossref puts the date in `published.date-parts` (preferred) but
        # older payloads use `published-print` / `published-online` / `issued`.
        for key in ("published", "published-print", "published-online", "issued"):
            block = item.get(key)
            if not isinstance(block, dict):
                continue
            parts = block.get("date-parts")
            if not (isinstance(parts, list) and parts and isinstance(parts[0], list)):
                continue
            ymd = parts[0]
            try:
                year = int(ymd[0])
                month = int(ymd[1]) if len(ymd) > 1 and ymd[1] is not None else 1
                day = int(ymd[2]) if len(ymd) > 2 and ymd[2] is not None else 1
                return datetime(year, month, day, tzinfo=timezone.utc)
            except (TypeError, ValueError):
                continue
        return None

    @staticmethod
    def _authors(item: dict[str, Any]) -> list[str]:
        out: list[str] = []
        for a in item.get("author") or []:
            if not isinstance(a, dict):
                continue
            given = (a.get("given") or "").strip()
            family = (a.get("family") or "").strip()
            name = " ".join(p for p in (given, family) if p)
            if not name:
                name = (a.get("name") or "").strip()
            if name:
                out.append(name)
        return out

    def _to_result(self, item: dict[str, Any]) -> SearchResult:
        doi = (item.get("DOI") or "").strip()
        title = self._first(item.get("title"))
        venue = self._first(item.get("container-title"))
        abstract = (item.get("abstract") or "").strip()
        url = f"https://doi.org/{doi}" if doi else (item.get("URL") or "")
        external_id = doi or url
        cited_by = item.get("is-referenced-by-count")
        try:
            citation_count = int(cited_by) if cited_by is not None else 0
        except (TypeError, ValueError):
            citation_count = 0

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title.strip(),
            url=url,
            abstract=abstract,
            authors=self._authors(item),
            published_at=self._parse_published(item),
            venue=venue,
            citation_count=citation_count,
            raw=item,
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
        params: dict[str, Any] = {
            "query": query,
            "rows": rows,
            "cursor": cursor if cursor else "*",
        }
        if since is not None:
            params["filter"] = f"from-pub-date:{since.strftime('%Y-%m-%d')}"

        resp = await self._client.get(CROSSREF_WORKS_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()
        message = payload.get("message") or {}
        items = message.get("items") or []

        results = [self._to_result(item) for item in items if isinstance(item, dict)]

        next_cursor_raw = message.get("next-cursor") or ""
        # When the upstream has no more pages it either omits the cursor or
        # returns fewer rows than we asked for. Guard against echo-cursor
        # loops by clearing next_cursor on a short page.
        if not items or len(items) < rows:
            next_cursor = ""
        else:
            next_cursor = next_cursor_raw or ""

        total_estimated = 0
        total_raw = message.get("total-results")
        try:
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
