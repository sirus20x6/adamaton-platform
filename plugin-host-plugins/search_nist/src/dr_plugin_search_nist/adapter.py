"""NIST NVD (CVE) adapter — async httpx client against the NVD 2.0 REST API.

Endpoint:
    https://services.nvd.nist.gov/rest/json/cves/2.0

Pagination is offset-based via ``startIndex`` + ``resultsPerPage``; the
``cursor`` field is the next ``startIndex`` encoded as a string.

A date filter requires BOTH ``pubStartDate`` and ``pubEndDate``. When the
caller supplies ``since`` we set the end bound to "now" (UTC).

An optional ``NVD_API_KEY`` env var raises the unauthenticated rate limit
from 5/30s to 50/30s.
"""

from __future__ import annotations

import os
from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


NVD_API_URL = "https://services.nvd.nist.gov/rest/json/cves/2.0"
USER_AGENT = (
    "adamaton-deepresearch/search_nist (+https://github.com/sirus20x6)"
)
# NVD's documented hard cap is 2000 per page; we cap defensively.
MAX_RESULTS_PER_PAGE = 2000


def _parse_dt(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        # NVD returns ISO 8601, sometimes with trailing 'Z'.
        if value.endswith("Z"):
            value = value[:-1] + "+00:00"
        return datetime.fromisoformat(value)
    except (TypeError, ValueError):
        return None


def _english_description(descriptions: list[dict[str, Any]] | None) -> str:
    if not descriptions:
        return ""
    for d in descriptions:
        if d.get("lang") == "en" and d.get("value"):
            return str(d["value"])
    # Fallback to the first description if no English is provided.
    first = descriptions[0]
    return str(first.get("value") or "")


def _base_score(metrics: dict[str, Any] | None) -> float | None:
    if not metrics:
        return None
    # Prefer CVSS v3.1, fall back to v3.0, then v2.0.
    for key in ("cvssMetricV31", "cvssMetricV30", "cvssMetricV2"):
        entries = metrics.get(key) or []
        for entry in entries:
            cvss = entry.get("cvssData") or {}
            score = cvss.get("baseScore")
            if score is not None:
                try:
                    return float(score)
                except (TypeError, ValueError):
                    continue
    return None


def _references(refs: list[dict[str, Any]] | None) -> list[str]:
    if not refs:
        return []
    out: list[str] = []
    for ref in refs:
        url = ref.get("url")
        if url:
            out.append(str(url))
    return out


class NistNvdAdapter:
    name = "nist_nvd"
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        self._api_key = os.environ.get("NVD_API_KEY") or None
        headers = {"User-Agent": USER_AGENT, "Accept": "application/json"}
        if self._api_key:
            headers["apiKey"] = self._api_key
        self._client = httpx.AsyncClient(timeout=30.0, headers=headers)

    def _to_result(self, vuln: dict[str, Any]) -> SearchResult:
        cve = vuln.get("cve") or {}
        cve_id = str(cve.get("id") or "").strip()
        abstract = _english_description(cve.get("descriptions"))
        title_body = abstract.strip().replace("\n", " ")
        title = f"{cve_id}: {title_body[:80]}" if cve_id else title_body[:80]
        published = _parse_dt(cve.get("published"))
        score = _base_score(cve.get("metrics"))
        url = (
            f"https://nvd.nist.gov/vuln/detail/{cve_id}"
            if cve_id
            else "https://nvd.nist.gov/vuln"
        )
        raw: dict[str, Any] = {
            "id": cve_id,
            "published": cve.get("published"),
            "lastModified": cve.get("lastModified"),
            "vulnStatus": cve.get("vulnStatus"),
            "references": _references(cve.get("references")),
            "metrics": cve.get("metrics"),
            "weaknesses": cve.get("weaknesses"),
        }
        return SearchResult(
            adapter=self.name,
            external_id=cve_id,
            title=title,
            url=url,
            abstract=abstract,
            authors=[],
            published_at=published,
            venue="NIST NVD",
            citation_count=0,
            raw=raw,
            score=score if score is not None else 0.0,
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
        start_index = int(cursor) if cursor and cursor.isdigit() else 0
        results_per_page = max(1, min(int(limit), MAX_RESULTS_PER_PAGE))

        params: dict[str, Any] = {
            "keywordSearch": query,
            "resultsPerPage": results_per_page,
            "startIndex": start_index,
        }

        if since is not None:
            # NVD requires BOTH bounds when filtering on publish dates.
            if since.tzinfo is None:
                start = since.replace(tzinfo=timezone.utc)
            else:
                start = since.astimezone(timezone.utc)
            now = datetime.now(timezone.utc)
            # NVD expects ISO 8601 with milliseconds; trailing offset is OK.
            params["pubStartDate"] = start.strftime("%Y-%m-%dT%H:%M:%S.000")
            params["pubEndDate"] = now.strftime("%Y-%m-%dT%H:%M:%S.000")

        resp = await self._client.get(NVD_API_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        vulnerabilities = payload.get("vulnerabilities") or []
        total = int(payload.get("totalResults") or 0)
        returned = int(payload.get("resultsPerPage") or len(vulnerabilities))
        page_start = int(payload.get("startIndex") or start_index)

        results = [self._to_result(v) for v in vulnerabilities]

        next_start = page_start + returned
        next_cursor = str(next_start) if next_start < total else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
