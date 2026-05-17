"""Common Crawl URL-pattern search adapter.

Common Crawl exposes per-crawl CDX indexes. The flow is:

1. ``GET https://index.commoncrawl.org/collinfo.json`` returns the list of
   available indexes, ordered newest first. We cache this list per process.
2. ``GET https://index.commoncrawl.org/{index_id}-index?url={pattern}&output=json``
   returns newline-delimited JSON; each line has ``url``, ``timestamp``,
   ``mime``, ``status``, ``length``, ``digest``.

The CDX endpoint pages via ``page`` + ``pageSize`` parameters; a special
``&showNumPages=true`` first request returns the total page count as a
single JSON object. We encode the next page number into ``cursor``.

The ``q`` argument is a URL pattern (e.g. ``*.example.com/*``), not a
keyword query. Non-URL-shaped queries return an empty SearchPage rather
than raising — Common Crawl has no full-text search.

Date filtering is done client-side against the parsed CDX ``timestamp``
(YYYYMMDDHHMMSS).
"""

from __future__ import annotations

import asyncio
import json
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


CC_COLLINFO_URL = "https://index.commoncrawl.org/collinfo.json"
CC_INDEX_BASE = "https://index.commoncrawl.org"
USER_AGENT = (
    "adamaton-deepresearch/search_commoncrawl "
    "(+https://github.com/sirus20x6)"
)


def _looks_like_url_pattern(q: str) -> bool:
    """Best-effort URL-pattern sniff.

    Common Crawl accepts a URL pattern like ``*.example.com/*`` or a bare
    host like ``example.com``. We accept anything that contains a dot or a
    wildcard and is otherwise non-empty. Whitespace-only or obvious
    natural-language queries are rejected so we can return an empty page.
    """
    if not q:
        return False
    s = q.strip()
    if not s:
        return False
    # Reject anything that contains whitespace — URL patterns don't.
    if any(c.isspace() for c in s):
        return False
    # Must contain a dot (hostname) or a wildcard.
    return "." in s or "*" in s


def _parse_cc_timestamp(ts: str) -> datetime | None:
    """Parse a CDX timestamp like ``20240115123045`` into a datetime."""
    if not ts or not ts.isdigit() or len(ts) < 8:
        return None
    # Pad shorter timestamps (rare but defensive).
    ts = (ts + "00000000000000")[:14]
    try:
        return datetime.strptime(ts, "%Y%m%d%H%M%S")
    except ValueError:
        return None


class CommonCrawlAdapter:
    name = "commoncrawl"
    source_kind = SourceKind.WEB
    page_size = 50

    def __init__(
        self,
        *,
        index_id: str = "latest",
        crawl: str | None = None,
    ) -> None:
        # ``crawl`` is a more explicit alias for ``index_id``; if both are set,
        # the explicit crawl wins.
        self._configured_index = (crawl or index_id or "latest").strip() or "latest"
        self._resolved_index: str | None = None
        self._collinfo_lock = asyncio.Lock()
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    async def _resolve_index(self) -> str:
        """Resolve ``latest`` to a concrete index id; otherwise pass through."""
        if self._configured_index != "latest":
            return self._configured_index
        if self._resolved_index is not None:
            return self._resolved_index
        async with self._collinfo_lock:
            if self._resolved_index is not None:
                return self._resolved_index
            resp = await self._client.get(CC_COLLINFO_URL)
            resp.raise_for_status()
            collinfo = resp.json()
            if not isinstance(collinfo, list) or not collinfo:
                raise RuntimeError("Common Crawl collinfo.json returned no indexes")
            # collinfo is ordered newest-first; entries have ``id`` keys.
            first = collinfo[0]
            if not isinstance(first, dict) or "id" not in first:
                raise RuntimeError("Common Crawl collinfo.json entry missing 'id'")
            self._resolved_index = str(first["id"])
            return self._resolved_index

    def _index_url(self, index_id: str) -> str:
        # Some entries are already suffixed with ``-index``; tolerate both.
        if index_id.endswith("-index"):
            return f"{CC_INDEX_BASE}/{index_id}"
        return f"{CC_INDEX_BASE}/{index_id}-index"

    async def _fetch_page(
        self,
        index_id: str,
        url_pattern: str,
        page: int,
    ) -> tuple[list[dict[str, Any]], int | None]:
        """Return (records, total_pages) for one CDX page.

        ``total_pages`` is only populated on the first call (when we ask
        ``showNumPages=true``).
        """
        idx_url = self._index_url(index_id)

        # First call: discover total pages so we know when to stop.
        total_pages: int | None = None
        if page == 0:
            meta_resp = await self._client.get(
                idx_url,
                params={
                    "url": url_pattern,
                    "output": "json",
                    "showNumPages": "true",
                    "pageSize": str(self.page_size),
                },
            )
            if meta_resp.status_code == 404:
                # Common Crawl returns 404 when no records match.
                return [], 0
            meta_resp.raise_for_status()
            try:
                meta = meta_resp.json()
                if isinstance(meta, dict):
                    total_pages = int(meta.get("pages", 0))
            except (json.JSONDecodeError, ValueError, TypeError):
                total_pages = None

        data_resp = await self._client.get(
            idx_url,
            params={
                "url": url_pattern,
                "output": "json",
                "page": str(page),
                "pageSize": str(self.page_size),
            },
        )
        if data_resp.status_code == 404:
            return [], total_pages if total_pages is not None else 0
        data_resp.raise_for_status()

        records: list[dict[str, Any]] = []
        for line in data_resp.text.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(rec, dict):
                records.append(rec)
        return records, total_pages

    def _to_result(self, rec: dict[str, Any]) -> SearchResult:
        url = str(rec.get("url") or "")
        timestamp = str(rec.get("timestamp") or "")
        digest = str(rec.get("digest") or "")
        if timestamp and digest:
            external_id = f"{timestamp}-{digest}"
        else:
            external_id = url or f"cc:{timestamp or digest or 'unknown'}"
        published = _parse_cc_timestamp(timestamp)
        raw: dict[str, Any] = {
            "timestamp": timestamp,
            "mime": rec.get("mime"),
            "status": rec.get("status"),
            "length": rec.get("length"),
            "digest": digest,
            "offset": rec.get("offset"),
            "filename": rec.get("filename"),
            "languages": rec.get("languages"),
            "encoding": rec.get("encoding"),
        }
        # Strip None values to keep ``raw`` tidy.
        raw = {k: v for k, v in raw.items() if v is not None}
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=url,  # Common Crawl returns no title at the index level.
            url=url,
            published_at=published,
            venue="Common Crawl",
            raw=raw,
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
        # Non-URL-pattern queries return an empty page (per the brief).
        if not _looks_like_url_pattern(query):
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        limit = max(int(limit), 1)
        start_page = int(cursor) if cursor and cursor.isdigit() else 0

        index_id = await self._resolve_index()

        collected: list[SearchResult] = []
        page = start_page
        total_pages: int | None = None
        last_page_seen = page

        while len(collected) < limit:
            records, discovered_pages = await self._fetch_page(
                index_id, query, page
            )
            if page == start_page and discovered_pages is not None:
                total_pages = discovered_pages

            for rec in records:
                result = self._to_result(rec)
                if since is not None:
                    if result.published_at is None or result.published_at < since:
                        continue
                collected.append(result)
                if len(collected) >= limit:
                    break

            last_page_seen = page
            page += 1

            # Stop conditions:
            # - We exceeded the known total page count.
            # - The CDX endpoint returned an empty page and we don't know
            #   the total page count (defensive — avoids infinite loops).
            if total_pages is not None and page >= total_pages:
                break
            if not records and total_pages is None:
                break

        # Encode next cursor.
        if len(collected) >= limit and (
            total_pages is None or last_page_seen + 1 < total_pages
        ):
            next_cursor = str(last_page_seen + 1)
        else:
            next_cursor = ""

        return SearchPage(
            results=collected,
            next_cursor=next_cursor,
            total_estimated=(total_pages or 0) * self.page_size if total_pages else 0,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
