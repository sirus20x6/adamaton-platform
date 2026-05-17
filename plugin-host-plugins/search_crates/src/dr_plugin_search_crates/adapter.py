"""crates.io adapter.

Hits the public ``https://crates.io/api/v1/crates`` search endpoint.
crates.io requires a descriptive User-Agent header — anonymous or
generic UAs may be aggressively rate-limited.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind

_BASE_URL = "https://crates.io/api/v1"
_USER_AGENT = (
    "adamaton-deepresearch/search_crates "
    "(https://github.com/sirus20x6; mailto:sirus20x6@gmail.com)"
)


def _parse_iso8601(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        # crates.io emits timestamps like 2014-11-20T20:25:24.488131+00:00
        # or with a trailing Z. ``fromisoformat`` handles both on 3.11+.
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


class CratesAdapter:
    name = "crates"
    source_kind = SourceKind.REPO

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

    def _to_result(self, crate: dict[str, Any]) -> SearchResult:
        name = (crate.get("name") or crate.get("id") or "").strip()
        max_version = (crate.get("max_version") or "").strip()
        external_id = f"{name}@{max_version}" if max_version else name
        url = f"https://crates.io/crates/{name}" if name else ""
        description = (crate.get("description") or "").strip()
        downloads = crate.get("downloads")
        try:
            score = float(downloads) if downloads is not None else 0.0
        except (TypeError, ValueError):
            score = 0.0
        published_at = _parse_iso8601(crate.get("created_at"))
        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=name,
            url=url,
            abstract=description,
            authors=[],
            published_at=published_at,
            venue="crates.io",
            citation_count=0,
            raw=dict(crate),
            score=score,
            source_kind=SourceKind.REPO,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        per_page = max(min(int(limit), 100), 1)
        page = int(cursor) if cursor and cursor.isdigit() and int(cursor) > 0 else 1
        params: dict[str, Any] = {
            "q": query,
            "per_page": per_page,
            "page": page,
        }

        r = await self._client.get("/crates", params=params)
        r.raise_for_status()
        data = r.json()
        if not isinstance(data, dict):
            data = {}

        crates = data.get("crates") or []
        meta = data.get("meta") or {}

        results: list[SearchResult] = []
        for crate in crates:
            if not isinstance(crate, dict):
                continue
            if since is not None:
                updated = _parse_iso8601(crate.get("updated_at"))
                if updated is None:
                    continue
                # Ensure both sides are timezone-aware for comparison.
                cmp_since = since
                if cmp_since.tzinfo is None:
                    cmp_since = cmp_since.replace(tzinfo=timezone.utc)
                if updated.tzinfo is None:
                    updated = updated.replace(tzinfo=timezone.utc)
                if updated < cmp_since:
                    continue
            results.append(self._to_result(crate))

        # crates.io meta has a ``next_page`` query-string-like value when
        # there are more pages. Fall back to incrementing ``page`` when a
        # full page was returned.
        next_cursor = ""
        next_page_raw = meta.get("next_page")
        if isinstance(next_page_raw, str) and next_page_raw:
            # next_page comes back as "?page=2&per_page=10&q=tokio"
            for part in next_page_raw.lstrip("?").split("&"):
                if part.startswith("page="):
                    candidate = part.split("=", 1)[1]
                    if candidate.isdigit():
                        next_cursor = candidate
                        break
        if not next_cursor and len(crates) >= per_page:
            next_cursor = str(page + 1)

        total_raw = meta.get("total")
        try:
            total_estimated = int(total_raw) if total_raw is not None else 0
        except (TypeError, ValueError):
            total_estimated = 0

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )
