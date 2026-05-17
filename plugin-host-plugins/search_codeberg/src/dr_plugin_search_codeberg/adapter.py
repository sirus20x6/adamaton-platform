"""Async adapter for the Codeberg / Forgejo / Gitea repository search API.

Endpoint:
    GET {base_url}/api/v1/repos/search?q={q}&limit={N}&page={M}

Returns a JSON object with a ``data`` array of repo dicts. Repos carry
``id``, ``full_name``, ``description``, ``html_url``, ``created_at``,
``updated_at``, ``stars_count``, and ``forks_count`` (plus more we stash
into ``raw``).

The API has no native ``since`` filter for the repo search endpoint, so
we filter client-side on ``updated_at``.
"""

from __future__ import annotations

import os
from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_ADAPTER_NAME = "codeberg"
_USER_AGENT = (
    f"adamaton-deepresearch/{_ADAPTER_NAME} "
    "(+https://github.com/sirus20x6)"
)
_DEFAULT_BASE_URL = "https://codeberg.org"


def _parse_iso(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        # Codeberg returns RFC3339, e.g. "2023-08-21T12:34:56Z".
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None


class CodebergAdapter:
    def __init__(
        self,
        base_url: str | None = None,
        token: str | None = None,
    ) -> None:
        self._base_url = (
            base_url
            or os.environ.get("CODEBERG_BASE_URL")
            or _DEFAULT_BASE_URL
        ).rstrip("/")
        self._token = token or os.environ.get("CODEBERG_TOKEN")

    def _headers(self) -> dict[str, str]:
        headers = {
            "User-Agent": _USER_AGENT,
            "Accept": "application/json",
        }
        if self._token:
            headers["Authorization"] = f"token {self._token}"
        return headers

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        try:
            page_num = int(cursor) if cursor else 1
        except ValueError:
            page_num = 1
        if page_num < 1:
            page_num = 1

        # Codeberg/Forgejo caps limit at 50.
        api_limit = max(1, min(int(limit) if limit else 10, 50))

        url = f"{self._base_url}/api/v1/repos/search"
        params = {
            "q": q,
            "limit": api_limit,
            "page": page_num,
        }

        async with httpx.AsyncClient(timeout=30.0) as client:
            response = await client.get(
                url, params=params, headers=self._headers()
            )
            response.raise_for_status()
            payload = response.json()

        repos = payload.get("data") or []

        # Normalize ``since`` to a timezone-aware datetime for safe compare.
        since_aware: datetime | None = None
        if since is not None:
            since_aware = (
                since if since.tzinfo else since.replace(tzinfo=timezone.utc)
            )

        results: list[SearchResult] = []
        for repo in repos:
            updated_at = _parse_iso(repo.get("updated_at"))
            if since_aware is not None:
                if updated_at is None:
                    continue
                cmp_updated = (
                    updated_at
                    if updated_at.tzinfo
                    else updated_at.replace(tzinfo=timezone.utc)
                )
                if cmp_updated < since_aware:
                    continue
            results.append(_repo_to_result(repo))

        # If we got a full page from the API, assume there may be another.
        # When the client-side ``since`` filter rejects everything on this
        # page we still want the caller to be able to paginate further.
        next_cursor = ""
        if len(repos) >= api_limit:
            next_cursor = str(page_num + 1)

        total_estimated = 0
        ok = payload.get("ok")
        # Codeberg/Forgejo doesn't return a stable total count for repo
        # search; leave 0 unless we can read it from ``ok`` shape.
        if isinstance(ok, dict) and isinstance(ok.get("total"), int):
            total_estimated = ok["total"]

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=total_estimated,
        )


def _repo_to_result(repo: dict[str, Any]) -> SearchResult:
    repo_id = repo.get("id")
    external_id = (
        str(repo_id)
        if repo_id is not None
        else (repo.get("full_name") or repo.get("html_url") or "")
    )

    return SearchResult(
        adapter=_ADAPTER_NAME,
        external_id=external_id,
        title=repo.get("full_name") or "",
        url=repo.get("html_url") or "",
        abstract=repo.get("description") or "",
        authors=[],
        published_at=_parse_iso(repo.get("created_at")),
        venue="",
        citation_count=0,
        raw=dict(repo),
        score=float(repo.get("stars_count") or 0),
        source_kind=SourceKind.REPO,
    )
