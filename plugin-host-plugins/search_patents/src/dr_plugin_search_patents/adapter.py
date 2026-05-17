"""USPTO PatentsView API adapter.

Hits the public PatentsView endpoint (https://api.patentsview.org/patents/query)
with a POST body containing a query (`q`), a list of requested fields (`f`),
and pagination options (`o`). Pagination is encoded as a 1-indexed page number
in ``cursor``; we return the next page number when the current page is full,
and an empty cursor otherwise.

No auth is required. Date filtering uses an ``_and`` block combining the
text-any query with a ``_gte`` on ``patent_date``.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


PATENTSVIEW_URL = "https://api.patentsview.org/patents/query"
USER_AGENT = (
    "adamaton-deepresearch/search_patents "
    "(+https://github.com/sirus20x6)"
)

REQUESTED_FIELDS = [
    "patent_number",
    "patent_title",
    "patent_abstract",
    "patent_date",
    "inventors",
    "assignees",
]


class PatentsViewAdapter:
    name = "patents"
    source_kind = SourceKind.WEB

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "application/json",
                "Content-Type": "application/json",
            },
        )

    @staticmethod
    def _parse_page(cursor: str | None) -> int:
        if not cursor:
            return 1
        try:
            n = int(cursor)
        except (TypeError, ValueError):
            return 1
        return max(n, 1)

    @staticmethod
    def _parse_published(value: Any) -> datetime | None:
        if not isinstance(value, str) or not value:
            return None
        # PatentsView returns dates as "YYYY-MM-DD".
        try:
            return datetime.strptime(value, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        except ValueError:
            return None

    @staticmethod
    def _authors(item: dict[str, Any]) -> list[str]:
        out: list[str] = []
        for inv in item.get("inventors") or []:
            if not isinstance(inv, dict):
                continue
            first = (inv.get("inventor_first_name") or "").strip()
            last = (inv.get("inventor_last_name") or "").strip()
            name = " ".join(p for p in (first, last) if p)
            if name:
                out.append(name)
        return out

    @staticmethod
    def _venue(item: dict[str, Any]) -> str:
        for ass in item.get("assignees") or []:
            if not isinstance(ass, dict):
                continue
            org = (ass.get("assignee_organization") or "").strip()
            if org:
                return org
        return ""

    def _to_result(self, item: dict[str, Any]) -> SearchResult:
        patent_number = (item.get("patent_number") or "").strip()
        title = (item.get("patent_title") or "").strip()
        abstract = (item.get("patent_abstract") or "").strip()
        url = (
            f"https://patents.google.com/patent/US{patent_number}"
            if patent_number
            else ""
        )

        return SearchResult(
            adapter=self.name,
            external_id=patent_number,
            title=title,
            url=url,
            abstract=abstract,
            authors=self._authors(item),
            published_at=self._parse_published(item.get("patent_date")),
            venue=self._venue(item),
            raw=item,
            source_kind=SourceKind.WEB,
        )

    @staticmethod
    def _build_query(query: str, since: datetime | None) -> dict[str, Any]:
        text_any = {"_text_any": {"patent_title": query}}
        if since is None:
            return text_any
        return {
            "_and": [
                text_any,
                {"_gte": {"patent_date": since.strftime("%Y-%m-%d")}},
            ]
        }

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        per_page = max(int(limit), 1)
        page = self._parse_page(cursor)

        body: dict[str, Any] = {
            "q": self._build_query(query, since),
            "f": REQUESTED_FIELDS,
            "o": {"per_page": per_page, "page": page},
        }

        resp = await self._client.post(PATENTSVIEW_URL, json=body)
        resp.raise_for_status()
        payload = resp.json()

        patents = payload.get("patents") or []
        results = [
            self._to_result(item) for item in patents if isinstance(item, dict)
        ]

        total_estimated = 0
        total_raw = payload.get("total_patent_count")
        try:
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0

        # If we got a full page and the next page would still be within range,
        # advance the cursor; otherwise return empty.
        if patents and len(patents) >= per_page and (
            total_estimated == 0 or page * per_page < total_estimated
        ):
            next_cursor = str(page + 1)
        else:
            next_cursor = ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
