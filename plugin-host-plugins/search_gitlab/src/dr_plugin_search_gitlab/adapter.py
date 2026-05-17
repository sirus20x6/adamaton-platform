"""GitLab REST API adapter.

Hits ``GET {base_url}/api/v4/search?scope=projects&search=<q>`` against the
configured GitLab instance (defaults to https://gitlab.com). The endpoint
returns a JSON array of project objects, with pagination via the ``page``
and ``per_page`` query parameters and an ``X-Total-Pages`` response header.

Auth is optional: setting ``GITLAB_TOKEN`` in the environment adds a Bearer
header which enables private-project visibility and lifts the anonymous
rate cap. ``base_url`` can be overridden via env var for self-hosted
GitLab instances.

The GitLab project search has no native ``created_after`` / ``last_activity_after``
parameter under ``scope=projects``, so a ``since`` filter is applied
client-side against each project's ``last_activity_at``.
"""

from __future__ import annotations

import os
from datetime import datetime
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


USER_AGENT = "adamaton-deepresearch/search_gitlab (+https://github.com/sirus20x6)"
DEFAULT_BASE_URL = "https://gitlab.com"


def _parse_iso8601(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


class GitlabAdapter:
    name = "gitlab"
    source_kind = SourceKind.REPO

    def __init__(
        self,
        *,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = 30.0,
    ) -> None:
        resolved_base = (
            base_url
            or os.environ.get("GITLAB_BASE_URL")
            or DEFAULT_BASE_URL
        ).rstrip("/")
        resolved_token = token if token is not None else os.environ.get("GITLAB_TOKEN")

        headers: dict[str, str] = {
            "User-Agent": USER_AGENT,
            "Accept": "application/json",
        }
        if resolved_token:
            headers["Authorization"] = f"Bearer {resolved_token}"

        self._base_url = resolved_base
        self._client = httpx.AsyncClient(
            base_url=resolved_base,
            timeout=timeout,
            headers=headers,
        )

    def _to_result(self, item: dict[str, Any]) -> SearchResult:
        project_id = item.get("id")
        external_id = str(project_id) if project_id is not None else (
            item.get("path_with_namespace") or item.get("web_url") or ""
        )
        title = (item.get("name_with_namespace") or item.get("name") or "").strip()
        url = (item.get("web_url") or "").strip()
        abstract = (item.get("description") or "").strip()

        stars_raw = item.get("star_count")
        try:
            stars = int(stars_raw) if stars_raw is not None else 0
        except (TypeError, ValueError):
            stars = 0

        return SearchResult(
            adapter=self.name,
            external_id=external_id,
            title=title,
            url=url,
            abstract=abstract,
            authors=[],
            published_at=_parse_iso8601(item.get("created_at")),
            venue="GitLab",
            citation_count=stars,
            raw=item,
            score=float(stars),
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
        try:
            page = int(cursor) if cursor else 1
        except ValueError:
            page = 1
        if page < 1:
            page = 1

        params: dict[str, Any] = {
            "scope": "projects",
            "search": query,
            "per_page": per_page,
            "page": page,
        }

        resp = await self._client.get("/api/v4/search", params=params)
        resp.raise_for_status()
        payload = resp.json()
        items = payload if isinstance(payload, list) else []

        results: list[SearchResult] = []
        for item in items:
            if not isinstance(item, dict):
                continue
            if since is not None:
                last_activity = _parse_iso8601(item.get("last_activity_at"))
                if last_activity is None or last_activity < since:
                    continue
            results.append(self._to_result(item))

        total_pages_header = resp.headers.get("X-Total-Pages")
        try:
            total_pages = int(total_pages_header) if total_pages_header else 0
        except ValueError:
            total_pages = 0

        total_raw = resp.headers.get("X-Total")
        try:
            total_estimated = int(total_raw) if total_raw else 0
        except ValueError:
            total_estimated = 0

        if total_pages:
            next_cursor = str(page + 1) if page < total_pages else ""
        else:
            next_cursor = str(page + 1) if len(items) >= per_page else ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
