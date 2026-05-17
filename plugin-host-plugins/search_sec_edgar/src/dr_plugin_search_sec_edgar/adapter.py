"""SEC EDGAR full-text search adapter.

Hits the EDGAR full-text search backend at
``https://efts.sec.gov/LATEST/search-index`` which returns an
Elasticsearch-style ``hits.hits[]`` payload. EDGAR requires a
User-Agent header that identifies the requester and includes an email
address; requests without it are throttled or rejected.

Pagination uses EDGAR's ``from`` + ``size`` window. We encode the next
offset (``from + size``) as the cursor string. Empty cursor means first
page.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


EDGAR_SEARCH_URL = "https://efts.sec.gov/LATEST/search-index"
# EDGAR's fair-access policy: include a contact email so they can
# reach out before throttling. See https://www.sec.gov/os/accessing-edgar-data.
USER_AGENT = "adamaton-deepresearch sirus20x6@gmail.com"


def _accession_no_dashes(accession: str) -> str:
    """Convert 0001234567-89-012345 -> 000123456789012345."""
    return accession.replace("-", "")


def _build_archive_url(cik: str | int, accession: str) -> str:
    """Build the canonical EDGAR filing index URL."""
    try:
        cik_int = int(str(cik).lstrip("0") or "0")
    except (TypeError, ValueError):
        cik_int = 0
    return (
        f"https://www.sec.gov/Archives/edgar/data/{cik_int}/"
        f"{_accession_no_dashes(accession)}/{accession}-index.htm"
    )


def _parse_file_date(file_date: Any) -> datetime | None:
    if not isinstance(file_date, str) or not file_date:
        return None
    # EDGAR file_date is "YYYY-MM-DD".
    try:
        return datetime.strptime(file_date[:10], "%Y-%m-%d")
    except ValueError:
        return None


class SecEdgarAdapter:
    name = "sec_edgar"
    source_kind = SourceKind.WEB

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "application/json",
            },
        )

    def _to_result(self, hit: dict[str, Any]) -> SearchResult:
        source: dict[str, Any] = hit.get("_source") or {}
        accession = hit.get("_id") or source.get("adsh") or ""
        display_names = source.get("display_names") or []
        first_display = (
            display_names[0]
            if isinstance(display_names, list) and display_names
            else ""
        )
        form = (source.get("form") or "").strip()
        title = (
            f"{first_display} {form}".strip()
            if first_display or form
            else accession
        )

        ciks = source.get("ciks") or []
        first_cik = ""
        if isinstance(ciks, list) and ciks:
            first_cik = str(ciks[0])

        url = ""
        if accession and first_cik:
            url = _build_archive_url(first_cik, accession)

        authors: list[str] = []
        if isinstance(display_names, list):
            authors = [str(n) for n in display_names if n]

        return SearchResult(
            adapter=self.name,
            external_id=str(accession),
            title=title,
            url=url,
            abstract="",
            authors=authors,
            published_at=_parse_file_date(source.get("file_date")),
            venue=form,
            citation_count=0,
            raw=dict(source),
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
        size = max(int(limit), 1)
        try:
            from_offset = int(cursor) if cursor else 0
        except ValueError:
            from_offset = 0
        if from_offset < 0:
            from_offset = 0

        params: dict[str, Any] = {
            "q": query,
            "from": from_offset,
            "size": size,
        }
        if since is not None:
            params["dateRange"] = "custom"
            params["startdt"] = since.strftime("%Y-%m-%d")

        resp = await self._client.get(EDGAR_SEARCH_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        hits_block = payload.get("hits") or {}
        hits = hits_block.get("hits") or []

        results: list[SearchResult] = []
        for hit in hits:
            if isinstance(hit, dict):
                results.append(self._to_result(hit))

        total_estimated = 0
        total_block = hits_block.get("total")
        if isinstance(total_block, dict):
            try:
                total_estimated = int(total_block.get("value") or 0)
            except (TypeError, ValueError):
                total_estimated = 0
        elif isinstance(total_block, int):
            total_estimated = total_block

        next_offset = from_offset + len(results)
        if not results or len(results) < size or next_offset >= total_estimated > 0:
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
