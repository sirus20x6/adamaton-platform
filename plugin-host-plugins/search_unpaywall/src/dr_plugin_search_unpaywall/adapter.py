"""Unpaywall REST API adapter.

Hits the public Unpaywall API (https://api.unpaywall.org/v2) which requires
a contact email on every request. Two endpoints are used:

- ``/v2/search?query=...&email=...&page=N`` for keyword search
- ``/v2/{doi}?email=...``                    for per-DOI lookup (used by fetch)

Pagination is 1-indexed via the ``page`` parameter. ``cursor`` is encoded as
the next page number; an empty cursor means page 1.

Date filtering is not natively supported; ``since`` is applied client-side
against each result's ``published_date``.
"""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import FetchedDoc, SearchPage, SearchResult, SourceKind


UNPAYWALL_BASE_URL = "https://api.unpaywall.org/v2"
USER_AGENT = (
    "adamaton-deepresearch/search_unpaywall "
    "(+https://github.com/sirus20x6)"
)


class UnpaywallAdapter:
    name = "unpaywall"
    source_kind = SourceKind.JOURNAL

    def __init__(self, *, email: str | None = None, timeout: float = 30.0) -> None:
        self._email = email or os.environ.get("UNPAYWALL_EMAIL") or ""
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _parse_date(value: Any) -> datetime | None:
        if not value:
            return None
        if isinstance(value, datetime):
            return value
        text = str(value)
        # Unpaywall publishes ISO-8601-ish dates like "2018-09-04" or full
        # timestamps; fromisoformat handles both on 3.11+.
        try:
            return datetime.fromisoformat(text.rstrip("Z"))
        except ValueError:
            pass
        # Fall back to year-only.
        try:
            return datetime(int(text[:4]), 1, 1)
        except (TypeError, ValueError):
            return None

    @staticmethod
    def _authors(item: dict[str, Any]) -> list[str]:
        out: list[str] = []
        for a in item.get("z_authors") or []:
            if not isinstance(a, dict):
                continue
            family = (a.get("family") or "").strip()
            given = (a.get("given") or "").strip()
            name = " ".join(p for p in (given, family) if p) or family or given
            if not name:
                name = (a.get("name") or "").strip()
            if name:
                out.append(name)
        return out

    def _to_result(self, item: dict[str, Any]) -> SearchResult:
        # The /v2/search endpoint wraps each hit in {"response": {...}, "score": ..., "snippet": ...}.
        # Accept both wrapped and bare shapes.
        score: float = 0.0
        if "response" in item and isinstance(item["response"], dict):
            raw_score = item.get("score")
            try:
                score = float(raw_score) if raw_score is not None else 0.0
            except (TypeError, ValueError):
                score = 0.0
            work = item["response"]
        else:
            work = item

        doi = (work.get("doi") or "").strip()
        title = (work.get("title") or "").strip()
        venue = (work.get("journal_name") or "").strip()
        url = f"https://doi.org/{doi}" if doi else (work.get("doi_url") or "")
        external_id = doi or url
        published_at = self._parse_date(work.get("published_date"))

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract="",
            authors=self._authors(work),
            published_at=published_at,
            venue=venue,
            citation_count=0,
            raw=work,
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
        if not self._email:
            raise RuntimeError(
                "Unpaywall requires a contact email. Set UNPAYWALL_EMAIL or pass "
                "`email` in the plugin config."
            )

        try:
            page_num = int(cursor) if cursor else 1
        except ValueError:
            page_num = 1
        if page_num < 1:
            page_num = 1

        params: dict[str, Any] = {
            "query": query,
            "email": self._email,
            "page": page_num,
        }

        resp = await self._client.get(f"{UNPAYWALL_BASE_URL}/search", params=params)
        resp.raise_for_status()
        payload = resp.json()
        items = payload.get("results") or []

        results: list[SearchResult] = []
        for item in items:
            if not isinstance(item, dict):
                continue
            r = self._to_result(item)
            if since is not None and r.published_at is not None and r.published_at < since:
                continue
            results.append(r)

        # Trim to caller's limit; if the upstream gave us a full page we
        # assume more pages exist.
        clipped = results[: max(int(limit), 1)]
        had_full_page = len(items) >= max(int(limit), 1)
        next_cursor = str(page_num + 1) if had_full_page else ""

        return SearchPage(
            results=clipped,
            next_cursor=next_cursor,
            total_estimated=0,
        )

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        if not self._email:
            raise RuntimeError(
                "Unpaywall requires a contact email. Set UNPAYWALL_EMAIL or pass "
                "`email` in the plugin config."
            )

        doi = (result.external_id or "").strip()
        if doi.startswith("http"):
            # external_id fell back to a URL; try to recover the DOI tail.
            tail = doi.split("doi.org/", 1)[-1]
            doi = tail or doi

        if not doi or doi.startswith("http"):
            raise RuntimeError(
                f"Cannot fetch from Unpaywall without a DOI (got {result.external_id!r})."
            )

        resp = await self._client.get(
            f"{UNPAYWALL_BASE_URL}/{doi}", params={"email": self._email}
        )
        resp.raise_for_status()
        work = resp.json() or {}

        best = work.get("best_oa_location") or {}
        if not isinstance(best, dict):
            best = {}
        oa_url = (best.get("url_for_pdf") or best.get("url") or "").strip()

        return FetchedDoc(
            adapter=self.name,
            external_id=doi,
            url=oa_url or (result.url or f"https://doi.org/{doi}"),
            title=result.title or (work.get("title") or "").strip(),
            content_type="application/pdf" if best.get("url_for_pdf") else "text/html",
            body=b"",
            source_tier="oa_link",
            metadata={
                "oa_status": work.get("oa_status") or "",
                "license": best.get("license") or "",
            },
        )

    async def aclose(self) -> None:
        await self._client.aclose()
