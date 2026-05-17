"""Internet Archive Scholar adapter.

Calls scholar.archive.org's public JSON search endpoint:

    https://scholar.archive.org/search?q={q}&offset=M&limit=N&format=json

The endpoint returns `results[]` records with `title`, `abstracts[]`,
`contribs[].raw_name`, `release_date`, `release_year`, `publisher`, `ident`,
`doi`, `fulltext.access_url`, etc., plus a `hits` count we surface as
`total_estimated`.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


USER_AGENT = (
    "adamaton-deepresearch/search_wayback (+https://github.com/sirus20x6)"
)
SEARCH_URL = "https://scholar.archive.org/search"


def _parse_release_date(record: dict[str, Any]) -> datetime | None:
    raw = record.get("release_date")
    if isinstance(raw, str) and raw:
        # release_date is typically YYYY-MM-DD; tolerate prefix.
        for fmt in ("%Y-%m-%d", "%Y-%m", "%Y"):
            try:
                return datetime.strptime(raw[: len(fmt.replace("%Y", "0000"))], fmt)
            except ValueError:
                continue
    year = record.get("release_year")
    if isinstance(year, int) and year > 0:
        try:
            return datetime(year, 1, 1)
        except ValueError:
            return None
    return None


def _result_url(record: dict[str, Any], ident: str) -> str:
    fulltext = record.get("fulltext") or {}
    if isinstance(fulltext, dict):
        access = fulltext.get("access_url")
        if isinstance(access, str) and access:
            return access
    if ident:
        return f"https://scholar.archive.org/work/{ident}"
    return ""


def _external_id(record: dict[str, Any]) -> str:
    ident = record.get("ident")
    if isinstance(ident, str) and ident:
        return ident
    doi = record.get("doi")
    if isinstance(doi, str) and doi:
        return f"doi:{doi}"
    # Final fallback: synthesize from title/year so the host always has something.
    title = (record.get("title") or "").strip().lower().replace(" ", "-")[:80]
    year = record.get("release_year") or ""
    return f"wayback:{year}:{title}" if title else "wayback:unknown"


def _authors(record: dict[str, Any]) -> list[str]:
    contribs = record.get("contribs") or []
    out: list[str] = []
    if isinstance(contribs, list):
        for c in contribs:
            if isinstance(c, dict):
                name = c.get("raw_name")
                if isinstance(name, str) and name.strip():
                    out.append(name.strip())
    return out


def _abstract(record: dict[str, Any]) -> str:
    abstracts = record.get("abstracts") or []
    if isinstance(abstracts, list) and abstracts:
        first = abstracts[0]
        if isinstance(first, dict):
            body = first.get("body")
            if isinstance(body, str) and body.strip():
                return body.strip()
        elif isinstance(first, str) and first.strip():
            return first.strip()
    return ""


class WaybackScholarAdapter:
    name = "search_wayback"
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={
                "User-Agent": USER_AGENT,
                "Accept": "application/json",
            },
        )

    def _to_result(self, record: dict[str, Any]) -> SearchResult:
        ident = record.get("ident") if isinstance(record.get("ident"), str) else ""
        external_id = _external_id(record)
        title = (record.get("title") or "").strip()
        url = _result_url(record, ident or "")
        published_at = _parse_release_date(record)
        publisher = record.get("publisher")
        venue = publisher.strip() if isinstance(publisher, str) else ""

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract=_abstract(record),
            authors=_authors(record),
            published_at=published_at,
            venue=venue,
            citation_count=0,
            raw=dict(record),
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
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        limit = max(int(limit), 1)

        q = query
        if since is not None:
            q = f"{q} release_year:[{since.year} TO *]".strip()

        params = {
            "q": q,
            "offset": str(offset),
            "limit": str(limit),
            "format": "json",
        }
        resp = await self._client.get(SEARCH_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        raw_results = payload.get("results") or []
        results = [
            self._to_result(r) for r in raw_results if isinstance(r, dict)
        ]

        hits = payload.get("hits")
        total_estimated = int(hits) if isinstance(hits, int) else 0

        next_offset = offset + len(results)
        has_more = (
            len(results) >= limit
            and (total_estimated == 0 or next_offset < total_estimated)
        )
        next_cursor = str(next_offset) if has_more else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
