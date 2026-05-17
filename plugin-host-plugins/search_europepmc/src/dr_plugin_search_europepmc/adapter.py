"""Europe PMC adapter.

Europe PMC's REST search API returns JSON with a `resultList.result[]`
array and a `nextCursorMark` pagination token. The first request uses
cursorMark=`*`; subsequent calls echo back the previous `nextCursorMark`.

API: GET https://www.ebi.ac.uk/europepmc/webservices/rest/search
  ?query=<q>&format=json&resultType=core&pageSize=N&cursorMark=*

No auth required.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_BASE_URL = "https://www.ebi.ac.uk/europepmc/webservices/rest"
_PLUGIN_ID = "search_europepmc"
_USER_AGENT = (
    f"adamaton-deepresearch/{_PLUGIN_ID} (+https://github.com/sirus20x6)"
)


def _first(text: Any) -> str:
    if not isinstance(text, str):
        return ""
    return text.strip()


def _parse_date(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    # Europe PMC emits dates like "2019-05-24"; tolerate ISO timestamps too.
    raw = value.replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(raw)
    except ValueError:
        # Sometimes only a year is supplied; fall back to Jan 1.
        try:
            return datetime(int(value[:4]), 1, 1)
        except (TypeError, ValueError):
            return None


def _authors(record: dict[str, Any]) -> list[str]:
    """Prefer the structured authorList; fall back to the flat authorString."""
    out: list[str] = []
    author_list = record.get("authorList")
    if isinstance(author_list, dict):
        authors_raw = author_list.get("author")
        if isinstance(authors_raw, list):
            for a in authors_raw:
                if not isinstance(a, dict):
                    continue
                name = _first(a.get("fullName")) or _first(a.get("lastName"))
                if name:
                    out.append(name)
    if out:
        return out
    flat = _first(record.get("authorString"))
    if flat:
        # authorString is "Smith J, Doe A, ..." — split and trim.
        out = [s.strip() for s in flat.split(",") if s.strip()]
    return out


def _external_id(record: dict[str, Any]) -> str:
    """Prefer DOI, fall back to the Europe PMC id (PMID-like)."""
    doi = _first(record.get("doi"))
    if doi:
        return doi
    eid = _first(record.get("id"))
    if eid:
        return eid
    # Last-ditch: pmid / pmcid.
    return _first(record.get("pmid")) or _first(record.get("pmcid")) or ""


def _url(record: dict[str, Any]) -> str:
    """Prefer the PMC reader URL when a pmcid is present; otherwise doi.org."""
    pmcid = _first(record.get("pmcid"))
    if pmcid:
        return f"https://europepmc.org/article/PMC/{pmcid}"
    doi = _first(record.get("doi"))
    if doi:
        return f"https://doi.org/{doi}"
    pmid = _first(record.get("pmid"))
    if pmid:
        return f"https://europepmc.org/article/MED/{pmid}"
    eid = _first(record.get("id"))
    src = _first(record.get("source"))
    if eid and src:
        return f"https://europepmc.org/article/{src}/{eid}"
    return ""


class EuropePMCAdapter:
    name = "europepmc"

    def __init__(self) -> None:
        self._client = httpx.AsyncClient(
            base_url=_BASE_URL,
            headers={
                "User-Agent": _USER_AGENT,
                "Accept": "application/json",
            },
            timeout=30.0,
            follow_redirects=True,
        )

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()

    def _to_result(self, record: dict[str, Any]) -> SearchResult:
        title = _first(record.get("title"))
        venue = _first(record.get("journalTitle"))
        return SearchResult(
            adapter=self.name,
            external_id=_external_id(record),
            title=title,
            url=_url(record),
            abstract=_first(record.get("abstractText")),
            authors=_authors(record),
            published_at=_parse_date(record.get("firstPublicationDate")),
            venue=venue,
            citation_count=int(record.get("citedByCount") or 0),
            raw=record,
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
        # Europe PMC accepts date filters as inline query terms.
        q = query
        if since is not None:
            q = f"{q} AND FIRST_PDATE:[{since.date().isoformat()} TO *]"

        params: dict[str, Any] = {
            "query": q,
            "format": "json",
            "resultType": "core",
            "pageSize": max(min(int(limit), 100), 1),
            "cursorMark": cursor if cursor else "*",
        }

        response = await self._client.get("/search", params=params)
        response.raise_for_status()
        data = response.json()
        if not isinstance(data, dict):
            data = {}

        result_list = data.get("resultList")
        records: list[Any] = []
        if isinstance(result_list, dict):
            records = result_list.get("result") or []
        if not isinstance(records, list):
            records = []

        results = [self._to_result(r) for r in records if isinstance(r, dict)]

        next_cursor_raw = data.get("nextCursorMark")
        next_cursor = _first(next_cursor_raw) if next_cursor_raw is not None else ""
        # Europe PMC echoes the same cursor when there are no more pages.
        if next_cursor and cursor and next_cursor == cursor:
            next_cursor = ""

        hit_count = data.get("hitCount")
        try:
            total = int(hit_count) if hit_count is not None else 0
        except (TypeError, ValueError):
            total = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )
