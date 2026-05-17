"""NCBI PubMed adapter — esearch + esummary over httpx."""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


ESEARCH_URL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esearch.fcgi"
ESUMMARY_URL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/esummary.fcgi"

ADAPTER_NAME = "pubmed"
USER_AGENT = (
    "adamaton-deepresearch/search_pubmed (+https://github.com/sirus20x6)"
)


def _parse_pubdate(raw: str) -> datetime | None:
    """Parse an NCBI pubdate string.

    PubMed pubdate is notoriously irregular:
      "2024 Mar 15", "2024 Mar", "2024", "2024 Spring", "2024 Mar-Apr".
    Best-effort: try a few common shapes; fall back to year-only; else None.
    """
    if not raw:
        return None
    s = raw.strip()
    # Strip a trailing season/range qualifier ("2024 Mar-Apr" -> "2024 Mar").
    s_norm = s.split("-")[0].strip()
    for fmt in ("%Y %b %d", "%Y %b", "%Y/%m/%d", "%Y/%m", "%Y"):
        try:
            return datetime.strptime(s_norm, fmt)
        except ValueError:
            continue
    # Last-ditch: pull the leading 4-digit year if present.
    head = s_norm.split(" ", 1)[0]
    if len(head) == 4 and head.isdigit():
        try:
            return datetime(int(head), 1, 1)
        except ValueError:
            return None
    return None


def _authors_from_record(record: dict[str, Any]) -> list[str]:
    out: list[str] = []
    for a in record.get("authors") or []:
        if isinstance(a, dict):
            name = a.get("name")
            if name:
                out.append(str(name))
    return out


class PubMedAdapter:
    def __init__(self) -> None:
        self._api_key = os.environ.get("NCBI_API_KEY") or None

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        if limit <= 0:
            return SearchPage(results=[], next_cursor="", total_estimated=0)

        retstart = 0
        if cursor:
            try:
                retstart = max(0, int(cursor))
            except ValueError:
                retstart = 0

        esearch_params: dict[str, Any] = {
            "db": "pubmed",
            "term": q,
            "retmode": "json",
            "retstart": retstart,
            "retmax": limit,
        }
        if since is not None:
            esearch_params["mindate"] = since.strftime("%Y/%m/%d")
            esearch_params["datetype"] = "pdat"
        if self._api_key:
            esearch_params["api_key"] = self._api_key

        headers = {"User-Agent": USER_AGENT}

        async with httpx.AsyncClient(timeout=30.0, headers=headers) as client:
            r = await client.get(ESEARCH_URL, params=esearch_params)
            r.raise_for_status()
            esearch_data = r.json()

            esr = esearch_data.get("esearchresult", {}) or {}
            id_list: list[str] = list(esr.get("idlist") or [])
            try:
                total = int(esr.get("count") or 0)
            except (TypeError, ValueError):
                total = 0

            if not id_list:
                return SearchPage(results=[], next_cursor="", total_estimated=total)

            esummary_params: dict[str, Any] = {
                "db": "pubmed",
                "id": ",".join(id_list),
                "retmode": "json",
            }
            if self._api_key:
                esummary_params["api_key"] = self._api_key

            r2 = await client.get(ESUMMARY_URL, params=esummary_params)
            r2.raise_for_status()
            esummary_data = r2.json()

        result_block = esummary_data.get("result", {}) or {}
        uids: list[str] = list(result_block.get("uids") or id_list)

        results: list[SearchResult] = []
        for pmid in uids:
            record = result_block.get(pmid)
            if not isinstance(record, dict):
                continue
            pubdate_raw = record.get("pubdate") or record.get("epubdate") or ""
            venue = (
                record.get("fulljournalname")
                or record.get("source")
                or ""
            )
            results.append(
                SearchResult(
                    adapter=ADAPTER_NAME,
                    external_id=str(pmid),
                    title=str(record.get("title") or "").strip(),
                    url=f"https://pubmed.ncbi.nlm.nih.gov/{pmid}/",
                    authors=_authors_from_record(record),
                    published_at=_parse_pubdate(str(pubdate_raw)),
                    venue=str(venue),
                    raw=record,
                    source_kind=SourceKind.JOURNAL,
                )
            )

        next_start = retstart + len(id_list)
        next_cursor = str(next_start) if next_start < total else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )
