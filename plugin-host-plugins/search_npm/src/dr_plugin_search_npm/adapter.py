"""npm registry search adapter.

Hits the public npm registry search endpoint
(``https://registry.npmjs.org/-/v1/search``) with ``text=<query>``,
``size=<limit>`` and ``from=<offset>``. The registry has no native
date filter, so ``since`` is applied client-side against each package's
``date`` field. Pagination cursor is encoded as the next ``from`` offset.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


NPM_SEARCH_URL = "https://registry.npmjs.org/-/v1/search"
USER_AGENT = (
    "adamaton-deepresearch/search_npm "
    "(+https://github.com/sirus20x6)"
)

# npm caps `size` at 250 per call; keep our own cap a bit lower to be polite.
_MAX_SIZE = 250


class NpmAdapter:
    name = "npm"
    source_kind = SourceKind.REPO

    def __init__(self, *, timeout: float = 30.0) -> None:
        self._client = httpx.AsyncClient(
            timeout=timeout,
            headers={"User-Agent": USER_AGENT, "Accept": "application/json"},
        )

    @staticmethod
    def _parse_date(value: Any) -> datetime | None:
        if not isinstance(value, str) or not value:
            return None
        # npm dates look like "2024-03-12T14:22:01.123Z" — datetime.fromisoformat
        # in 3.11+ handles trailing Z via replacement.
        try:
            iso = value.replace("Z", "+00:00")
            dt = datetime.fromisoformat(iso)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return dt
        except ValueError:
            return None

    @staticmethod
    def _maintainers(pkg: dict[str, Any]) -> list[str]:
        out: list[str] = []
        for m in pkg.get("maintainers") or []:
            if not isinstance(m, dict):
                continue
            uname = (m.get("username") or "").strip()
            if uname:
                out.append(uname)
        if out:
            return out
        # Fall back to publisher if maintainers is missing/empty.
        publisher = pkg.get("publisher")
        if isinstance(publisher, dict):
            uname = (publisher.get("username") or "").strip()
            if uname:
                return [uname]
        return []

    def _to_result(self, entry: dict[str, Any]) -> SearchResult:
        pkg = entry.get("package") or {}
        if not isinstance(pkg, dict):
            pkg = {}
        name = (pkg.get("name") or "").strip()
        version = (pkg.get("version") or "").strip()
        external_id = f"{name}@{version}" if name and version else (name or "")

        links = pkg.get("links") or {}
        if not isinstance(links, dict):
            links = {}
        url = (links.get("npm") or "").strip()
        if not url and name:
            url = f"https://www.npmjs.com/package/{name}"

        score_block = entry.get("score") or {}
        score_final = 0.0
        if isinstance(score_block, dict):
            raw_final = score_block.get("final")
            try:
                if raw_final is not None:
                    score_final = float(raw_final)
            except (TypeError, ValueError):
                score_final = 0.0

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=name,
            url=url,
            abstract=(pkg.get("description") or "").strip(),
            authors=self._maintainers(pkg),
            published_at=self._parse_date(pkg.get("date")),
            venue="npm",
            citation_count=0,
            raw=entry,
            score=score_final,
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
        size = max(min(int(limit), _MAX_SIZE), 1)
        offset = 0
        if cursor:
            try:
                offset = max(int(cursor), 0)
            except ValueError:
                offset = 0

        params: dict[str, Any] = {
            "text": query,
            "size": size,
            "from": offset,
        }

        resp = await self._client.get(NPM_SEARCH_URL, params=params)
        resp.raise_for_status()
        payload = resp.json()

        objects = payload.get("objects") or []
        if not isinstance(objects, list):
            objects = []

        results: list[SearchResult] = []
        for entry in objects:
            if not isinstance(entry, dict):
                continue
            result = self._to_result(entry)
            if since is not None and result.published_at is not None:
                if result.published_at < since:
                    continue
            results.append(result)

        total_estimated = 0
        total_raw = payload.get("total")
        try:
            if total_raw is not None:
                total_estimated = int(total_raw)
        except (TypeError, ValueError):
            total_estimated = 0

        # Compute next cursor based on the number of objects returned by the
        # upstream (not the client-side filtered count): if we got fewer than
        # size, or we've reached the reported total, there are no more pages.
        consumed = offset + len(objects)
        if not objects or len(objects) < size or consumed >= total_estimated > 0:
            next_cursor = ""
        else:
            next_cursor = str(consumed)

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
