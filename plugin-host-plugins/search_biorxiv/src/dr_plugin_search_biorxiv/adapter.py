"""bioRxiv / medRxiv / chemRxiv adapter.

This plugin proxies through Europe PMC because the bioRxiv native API
(`api.biorxiv.org`) only supports date-range listing, **not** keyword
search. Europe PMC indexes all three preprint servers (bioRxiv, medRxiv,
chemRxiv) and exposes a single keyword/cursorMark search endpoint.

Endpoint:
    GET https://www.ebi.ac.uk/europepmc/webservices/rest/search
    query: <q> AND (SRC:PPR) AND (PUBLISHER:"bioRxiv" OR PUBLISHER:"medRxiv" OR PUBLISHER:"chemRxiv")
    format=json
    resultType=core
    pageSize=<N>
    cursorMark=<* or token>

Pagination: cursorMark. Empty/None -> "*" (first page). The response's
`nextCursorMark` is returned verbatim; when it matches the cursor we
just sent, we've reached the end.

Date filter: appended to the query as `FIRST_PDATE:[YYYY-MM-DD TO *]`.

Returns SDK types directly (no legacy translation layer).
"""

from __future__ import annotations

from datetime import datetime
from typing import Any, Iterable

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_BASE_URL = "https://www.ebi.ac.uk/europepmc/webservices/rest/search"
_USER_AGENT = (
    "adamaton-deepresearch/search_biorxiv "
    "(+https://github.com/sirus20x6)"
)

_ALL_SERVERS: tuple[str, ...] = ("bioRxiv", "medRxiv", "chemRxiv")


def _first(text: str | None) -> str:
    if text is None:
        return ""
    return text.strip()


def _parse_date(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    raw = value.strip()
    # Europe PMC commonly returns YYYY-MM-DD; sometimes YYYY-MM or YYYY.
    for fmt in ("%Y-%m-%d", "%Y-%m", "%Y"):
        try:
            return datetime.strptime(raw, fmt)
        except ValueError:
            continue
    # Last-ditch: isoformat-ish.
    try:
        return datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return None


def _authors_from_list(author_list: Any) -> list[str]:
    """Europe PMC `authorList.author` is a list of {fullName, ...} dicts."""
    if not isinstance(author_list, dict):
        return []
    authors = author_list.get("author")
    if not isinstance(authors, list):
        return []
    out: list[str] = []
    for a in authors:
        if isinstance(a, dict):
            name = _first(a.get("fullName")) or _first(a.get("collectiveName"))
            if name:
                out.append(name)
    return out


def _build_publisher_clause(servers: Iterable[str]) -> str:
    quoted = [f'PUBLISHER:"{s}"' for s in servers]
    if len(quoted) == 1:
        return quoted[0]
    return "(" + " OR ".join(quoted) + ")"


class BiorxivAdapter:
    """Adapter for the bioRxiv/medRxiv/chemRxiv preprints via Europe PMC."""

    name = "biorxiv"
    source_kind = SourceKind.PREPRINT

    def __init__(self, *, servers: Iterable[str] | None = None) -> None:
        chosen: tuple[str, ...]
        if servers is None:
            chosen = _ALL_SERVERS
        else:
            seen: list[str] = []
            for s in servers:
                if isinstance(s, str) and s in _ALL_SERVERS and s not in seen:
                    seen.append(s)
            chosen = tuple(seen) if seen else _ALL_SERVERS
        self._servers = chosen
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={
                "User-Agent": _USER_AGENT,
                "Accept": "application/json",
            },
            follow_redirects=True,
        )

    async def aclose(self) -> None:
        if not self._client.is_closed:
            await self._client.aclose()

    def _build_query(self, q: str, since: datetime | None) -> str:
        publisher_clause = _build_publisher_clause(self._servers)
        parts = [q.strip(), "(SRC:PPR)", publisher_clause]
        # Drop empty user query gracefully — the rest is still a valid filter.
        parts = [p for p in parts if p]
        if since is not None:
            parts.append(f"FIRST_PDATE:[{since.date().isoformat()} TO *]")
        return " AND ".join(parts)

    @staticmethod
    def _venue_from_publisher(record: dict[str, Any]) -> str:
        # Europe PMC exposes the originating server as `publisher` for
        # preprints. Fall back to `bookOrReportDetails.publisher` or
        # `journalTitle` if missing.
        publisher = _first(record.get("publisher"))
        if publisher:
            return publisher
        bord = record.get("bookOrReportDetails")
        if isinstance(bord, dict):
            return _first(bord.get("publisher"))
        return _first(record.get("journalTitle"))

    @staticmethod
    def _build_external_id(record: dict[str, Any]) -> str:
        # Prefer DOI; fall back to <source>:<id> (typically PPR:PPRxxxxx).
        doi = _first(record.get("doi"))
        if doi:
            return doi
        source = _first(record.get("source"))
        rec_id = _first(record.get("id"))
        if source and rec_id:
            return f"{source}:{rec_id}"
        return rec_id or doi or ""

    @staticmethod
    def _build_url(record: dict[str, Any]) -> str:
        doi = _first(record.get("doi"))
        if doi:
            return f"https://doi.org/{doi}"
        full_text_list = record.get("fullTextUrlList")
        if isinstance(full_text_list, dict):
            urls = full_text_list.get("fullTextUrl")
            if isinstance(urls, list):
                for u in urls:
                    if isinstance(u, dict):
                        link = _first(u.get("url"))
                        if link:
                            return link
        source = _first(record.get("source"))
        rec_id = _first(record.get("id"))
        if source and rec_id:
            return f"https://europepmc.org/article/{source}/{rec_id}"
        return ""

    def _to_result(self, record: dict[str, Any]) -> SearchResult:
        external_id = self._build_external_id(record)
        title = _first(record.get("title"))
        abstract = _first(record.get("abstractText"))
        authors = _authors_from_list(record.get("authorList"))
        published_at = _parse_date(
            record.get("firstPublicationDate")
            or record.get("electronicPublicationDate")
            or record.get("pubYear")
        )
        venue = self._venue_from_publisher(record)
        citation_count_raw = record.get("citedByCount")
        try:
            citation_count = int(citation_count_raw) if citation_count_raw is not None else 0
        except (TypeError, ValueError):
            citation_count = 0
        return SearchResult(
            external_id=external_id,
            adapter=self.name,
            title=title,
            url=self._build_url(record),
            abstract=abstract,
            authors=authors,
            published_at=published_at,
            venue=venue,
            citation_count=citation_count,
            raw=record,
            score=0.0,
            source_kind=SourceKind.PREPRINT,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        page_size = max(min(int(limit), 100), 1)
        cursor_mark = cursor if cursor else "*"
        params = {
            "query": self._build_query(query, since),
            "format": "json",
            "resultType": "core",
            "pageSize": page_size,
            "cursorMark": cursor_mark,
        }
        response = await self._client.get(_BASE_URL, params=params)
        response.raise_for_status()
        data = response.json()

        result_list = data.get("resultList") if isinstance(data, dict) else None
        records: list[dict[str, Any]] = []
        if isinstance(result_list, dict):
            maybe = result_list.get("result")
            if isinstance(maybe, list):
                records = [r for r in maybe if isinstance(r, dict)]

        results = [self._to_result(r) for r in records]

        next_cursor_mark = ""
        if isinstance(data, dict):
            next_cursor_mark = _first(data.get("nextCursorMark"))

        # Europe PMC echoes the same cursorMark when there are no further
        # pages. Treat that as "end".
        next_cursor = next_cursor_mark if next_cursor_mark and next_cursor_mark != cursor_mark else ""

        try:
            total = int(data.get("hitCount", 0)) if isinstance(data, dict) else 0
        except (TypeError, ValueError):
            total = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )
