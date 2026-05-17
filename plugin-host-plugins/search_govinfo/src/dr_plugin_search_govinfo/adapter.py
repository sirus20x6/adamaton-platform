"""GovInfo (FDsys) adapter.

Uses the GovInfo Search API:
    POST https://api.govinfo.gov/search
with JSON body
    {
        "query": "<q>",
        "pageSize": N,
        "offsetMark": "<cursor or '*'>",
        "sorts": [{"field": "publishdate", "sortOrder": "DESC"}]
    }
and header
    X-Api-Key: <DATA_GOV_API_KEY>

The response contains `results[]`, `count` (total estimate), and
`nextOffsetMark` (an opaque pagination token; `*` for the first page).
"""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


GOVINFO_SEARCH_URL = "https://api.govinfo.gov/search"
USER_AGENT = (
    "adamaton-deepresearch/search_govinfo "
    "(+https://github.com/sirus20x6)"
)


def _parse_dt(value: Any) -> datetime | None:
    if not value or not isinstance(value, str):
        return None
    # GovInfo dateIssued values are typically YYYY-MM-DD; also tolerate
    # full ISO 8601 with a trailing Z.
    s = value.strip()
    if not s:
        return None
    try:
        if s.endswith("Z"):
            s = s[:-1] + "+00:00"
        return datetime.fromisoformat(s)
    except ValueError:
        # Fall back to date-only.
        try:
            return datetime.strptime(value[:10], "%Y-%m-%d")
        except (ValueError, TypeError):
            return None


class GovInfoAdapter:
    name = "govinfo"
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        self._client: httpx.AsyncClient | None = None

    def _api_key(self) -> str:
        key = os.environ.get("DATA_GOV_API_KEY", "").strip()
        if not key:
            raise RuntimeError(
                "DATA_GOV_API_KEY env var is required for search_govinfo"
            )
        return key

    async def _get_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(
                timeout=30.0,
                headers={"User-Agent": USER_AGENT},
            )
        return self._client

    def _build_query(self, q: str, since: datetime | None) -> str:
        query = q.strip()
        if since is not None:
            day = since.strftime("%Y-%m-%d")
            clause = f"dateIssued:range({day},*)"
            query = f"{query} {clause}".strip() if query else clause
        return query

    def _to_result(self, hit: dict[str, Any]) -> SearchResult:
        external_id = (
            hit.get("packageId")
            or hit.get("granuleId")
            or hit.get("resultLink")
            or hit.get("title")
            or ""
        )
        download = hit.get("download") or {}
        if isinstance(download, dict):
            url = (
                download.get("pdfLink")
                or download.get("txtLink")
                or download.get("xmlLink")
                or download.get("modsLink")
                or hit.get("resultLink")
                or ""
            )
        else:
            url = hit.get("resultLink") or ""

        title = (hit.get("title") or "").strip()
        abstract_raw = hit.get("summary")
        abstract = abstract_raw.strip() if isinstance(abstract_raw, str) else ""

        venue_raw = hit.get("collectionName") or hit.get("collectionCode") or ""
        venue = venue_raw if isinstance(venue_raw, str) else ""

        published = _parse_dt(hit.get("dateIssued"))

        return SearchResult(
            adapter=self.name,
            external_id=str(external_id),
            title=title,
            url=url or "",
            abstract=abstract,
            authors=[],
            published_at=published,
            venue=venue,
            citation_count=0,
            raw=hit,
            score=0.0,
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
        page_size = max(int(limit), 1)
        offset_mark = cursor if cursor else "*"
        q = self._build_query(query, since)

        body = {
            "query": q,
            "pageSize": page_size,
            "offsetMark": offset_mark,
            "sorts": [{"field": "publishdate", "sortOrder": "DESC"}],
        }

        client = await self._get_client()
        resp = await client.post(
            GOVINFO_SEARCH_URL,
            json=body,
            headers={
                "X-Api-Key": self._api_key(),
                "Accept": "application/json",
            },
        )
        resp.raise_for_status()
        payload = resp.json() or {}

        raw_results = payload.get("results") or []
        results: list[SearchResult] = []
        if isinstance(raw_results, list):
            for hit in raw_results:
                if isinstance(hit, dict):
                    results.append(self._to_result(hit))

        next_mark_raw = payload.get("nextOffsetMark")
        next_cursor = ""
        if isinstance(next_mark_raw, str) and next_mark_raw and next_mark_raw != offset_mark:
            if results:
                next_cursor = next_mark_raw

        total = payload.get("count")
        try:
            total_estimated = int(total) if total is not None else 0
        except (TypeError, ValueError):
            total_estimated = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None
