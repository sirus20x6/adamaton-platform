"""DOAJ adapter — async httpx client against the public DOAJ articles search API.

Endpoint: https://doaj.org/api/search/articles/{query}?page=N&pageSize=K

Response shape (relevant fields):
    {
      "total": int,
      "page": int,
      "pageSize": int,
      "results": [
        {
          "id": str,
          "bibjson": {
            "title": str,
            "abstract": str,
            "author": [{"name": str, ...}],
            "journal": {"title": str, ...},
            "year": "YYYY",
            "month": "MM",
            "identifier": [{"id": str, "type": "doi"|"issn"|...}],
            "link": [{"url": str, "type": "fulltext", ...}]
          }
        },
        ...
      ]
    }
"""

from __future__ import annotations

import os
import urllib.parse
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind

_BASE_URL = "https://doaj.org/api/search/articles"
_USER_AGENT = (
    "adamaton-deepresearch/search_doaj "
    "(+https://github.com/sirus20x6)"
)
_MONTH_NAMES = {
    "january": 1, "february": 2, "march": 3, "april": 4,
    "may": 5, "june": 6, "july": 7, "august": 8,
    "september": 9, "october": 10, "november": 11, "december": 12,
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7,
    "aug": 8, "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12,
}


def _parse_month(raw: Any) -> int | None:
    if raw is None:
        return None
    s = str(raw).strip()
    if not s:
        return None
    if s.isdigit():
        n = int(s)
        return n if 1 <= n <= 12 else None
    return _MONTH_NAMES.get(s.lower())


def _parse_published(bib: dict[str, Any]) -> datetime | None:
    year_raw = bib.get("year")
    if year_raw is None:
        return None
    try:
        year = int(str(year_raw).strip())
    except (TypeError, ValueError):
        return None
    month = _parse_month(bib.get("month")) or 1
    try:
        return datetime(year, month, 1)
    except ValueError:
        return None


def _pick_url(bib: dict[str, Any]) -> str:
    links = bib.get("link") or []
    if isinstance(links, list):
        for link in links:
            if isinstance(link, dict) and (link.get("type") or "").lower() == "fulltext":
                url = link.get("url")
                if url:
                    return str(url)
        for link in links:
            if isinstance(link, dict):
                url = link.get("url")
                if url:
                    return str(url)
    return ""


class DoajAdapter:
    name = "doaj"
    source_kind = SourceKind.JOURNAL

    def __init__(self) -> None:
        headers = {
            "User-Agent": _USER_AGENT,
            "Accept": "application/json",
        }
        self._api_key = os.environ.get("DOAJ_API_KEY") or None
        self._client = httpx.AsyncClient(timeout=30.0, headers=headers)

    def _build_query(self, q: str, since: datetime | None) -> str:
        query = q.strip() or "*"
        if since is not None:
            # DOAJ supports Lucene-style range filters on bibjson.year.
            query = f"({query}) AND bibjson.year:[{since.year} TO *]"
        return query

    def _to_result(self, hit: dict[str, Any]) -> SearchResult:
        external_id = str(hit.get("id") or "")
        bib = hit.get("bibjson") or {}
        title = (bib.get("title") or "").strip()
        abstract = (bib.get("abstract") or "").strip()
        authors_raw = bib.get("author") or []
        authors: list[str] = []
        if isinstance(authors_raw, list):
            for a in authors_raw:
                if isinstance(a, dict):
                    name = a.get("name")
                    if name:
                        authors.append(str(name))
        journal = bib.get("journal") or {}
        venue = ""
        if isinstance(journal, dict):
            venue = str(journal.get("title") or "").strip()
        url = _pick_url(bib)
        if not url and external_id:
            url = f"https://doaj.org/article/{external_id}"
        published_at = _parse_published(bib)
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract=abstract,
            authors=authors,
            published_at=published_at,
            venue=venue,
            raw={"bibjson": bib},
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
        page_size = max(int(limit), 1)
        if cursor and cursor.isdigit():
            page = max(int(cursor), 1)
        else:
            page = 1

        q = self._build_query(query, since)
        encoded_q = urllib.parse.quote(q, safe="")
        url = f"{_BASE_URL}/{encoded_q}"

        params: dict[str, Any] = {"page": page, "pageSize": page_size}
        if self._api_key:
            params["api_key"] = self._api_key

        resp = await self._client.get(url, params=params)
        resp.raise_for_status()
        payload = resp.json() if resp.content else {}

        raw_results = payload.get("results") or []
        results: list[SearchResult] = []
        if isinstance(raw_results, list):
            for hit in raw_results:
                if isinstance(hit, dict):
                    results.append(self._to_result(hit))

        total = payload.get("total")
        try:
            total_int = int(total) if total is not None else 0
        except (TypeError, ValueError):
            total_int = 0

        # Next cursor: advance the page if we filled the page and there's more.
        next_cursor = ""
        consumed = page * page_size
        if len(results) >= page_size and consumed < total_int:
            next_cursor = str(page + 1)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_int,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
