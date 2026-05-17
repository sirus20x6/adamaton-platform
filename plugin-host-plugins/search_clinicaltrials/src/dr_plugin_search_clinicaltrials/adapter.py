"""ClinicalTrials.gov v2 REST API adapter.

Hits the public ClinicalTrials.gov API v2 (https://clinicaltrials.gov/api/v2/studies)
and returns SDK ``SearchPage`` / ``SearchResult`` values directly. Pagination uses
the registry's opaque ``pageToken`` cursor: the cursor passed in is forwarded as
``pageToken`` on the next request, and the response's ``nextPageToken`` is echoed
back to the host as ``next_cursor``.

The ``since`` filter is folded into ``query.term`` using the registry's
``AREA[StartDate]RANGE[YYYY-MM-DD, MAX]`` syntax, since the v2 API has no
dedicated start-date parameter.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


CLINICALTRIALS_API_URL = "https://clinicaltrials.gov/api/v2/studies"
USER_AGENT = (
    "adamaton-deepresearch/search_clinicaltrials "
    "(+https://github.com/sirus20x6)"
)


class ClinicalTrialsAdapter:
    name = "clinicaltrials"
    source_kind = SourceKind.WEB

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _parse_date(value: Any) -> datetime | None:
        """Parse a ClinicalTrials.gov date string (YYYY-MM-DD or YYYY-MM)."""
        if not isinstance(value, str) or not value:
            return None
        for fmt in ("%Y-%m-%d", "%Y-%m", "%Y"):
            try:
                return datetime.strptime(value, fmt)
            except ValueError:
                continue
        return None

    def _to_result(self, study: dict[str, Any]) -> SearchResult:
        protocol = study.get("protocolSection") or {}
        ident = protocol.get("identificationModule") or {}
        status = protocol.get("statusModule") or {}
        desc = protocol.get("descriptionModule") or {}
        sponsors = protocol.get("sponsorCollaboratorsModule") or {}

        nct_id = (ident.get("nctId") or "").strip()
        title = (ident.get("briefTitle") or ident.get("officialTitle") or "").strip()
        abstract = (desc.get("briefSummary") or "").strip()
        lead = sponsors.get("leadSponsor") or {}
        venue = (lead.get("name") or "").strip() if isinstance(lead, dict) else ""

        start_struct = status.get("startDateStruct") or {}
        start_date_raw = (
            start_struct.get("date") if isinstance(start_struct, dict) else None
        )
        published_at = self._parse_date(start_date_raw)

        url = f"https://clinicaltrials.gov/study/{nct_id}" if nct_id else ""
        external_id = nct_id or url

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract=abstract,
            authors=[],
            published_at=published_at,
            venue=venue,
            raw=study,
            source_kind=SourceKind.WEB,
        )

    @staticmethod
    def _compose_query_term(query: str, since: datetime | None) -> str:
        term = (query or "").strip()
        if since is not None:
            date_filter = (
                f"AREA[StartDate]RANGE[{since.strftime('%Y-%m-%d')}, MAX]"
            )
            term = f"{term} AND {date_filter}" if term else date_filter
        return term

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        page_size = max(int(limit), 1)
        params: dict[str, Any] = {
            "query.term": self._compose_query_term(query, since),
            "pageSize": page_size,
            "format": "json",
        }
        if cursor:
            params["pageToken"] = cursor

        resp = await self._client.get(CLINICALTRIALS_API_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        studies = payload.get("studies") or []
        results = [
            self._to_result(s) for s in studies if isinstance(s, dict)
        ]

        next_cursor = (payload.get("nextPageToken") or "").strip()

        total_estimated = 0
        total_raw = payload.get("totalCount")
        try:
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
