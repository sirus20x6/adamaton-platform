"""Async adapter for Mastodon's v2 status search API.

Endpoint:
    GET https://{instance}/api/v2/search
        ?q={q}&type=statuses&limit={N}&max_id={id}
    Authorization: Bearer {access_token}

Response shape:
    {
        "accounts": [],
        "statuses": [
            {
                "id": "111...",
                "url": "https://mastodon.social/@user/111...",
                "created_at": "2025-01-01T12:34:56.000Z",
                "content": "<p>HTML body...</p>",
                "account": {
                    "username": "user",
                    "acct": "user@instance",
                    "display_name": "..."
                },
                "favourites_count": 12,
                "reblogs_count": 3,
                "replies_count": 1,
                ...
            },
            ...
        ],
        "hashtags": []
    }

Auth is REQUIRED — Mastodon's /api/v2/search rejects unauthenticated
clients. We read ``MASTODON_TOKEN`` and ``MASTODON_INSTANCE`` from the
process environment (the host injects ``config_schema`` values via env).

Pagination uses ``max_id`` (older direction). We encode the cursor as
the last status id seen on the previous page.

Date filter: Mastodon's search API doesn't accept a date predicate, so
``since`` is applied client-side against ``created_at``.
"""

from __future__ import annotations

import os
from datetime import datetime, timezone
from typing import Any

import httpx
from selectolax.parser import HTMLParser

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_ADAPTER_NAME = "mastodon"
_USER_AGENT = (
    "adamaton-deepresearch/search_mastodon (+https://github.com/sirus20x6)"
)
_DEFAULT_INSTANCE = "mastodon.social"
_TITLE_LIMIT = 80


def _strip_html(html: str) -> str:
    """Return the visible text of a Mastodon content blob.

    Mastodon emits ``<p>``/``<br>``-wrapped HTML; we only need a plain
    string for the title. selectolax is fast and tolerant of malformed
    input (Mastodon clients sometimes ship loose markup).
    """
    if not html:
        return ""
    try:
        text = HTMLParser(html).text(separator=" ", strip=True) or ""
    except Exception:
        # Fall back to a raw return rather than blowing up on weird input;
        # the abstract field still carries the original HTML for callers.
        return html
    return " ".join(text.split())


def _parse_created_at(value: Any) -> datetime | None:
    if not value:
        return None
    try:
        # Mastodon serializes as RFC3339 with a trailing ``Z``.
        return datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None


class MastodonConfigError(RuntimeError):
    """Raised when ``MASTODON_TOKEN`` isn't configured."""


class MastodonAdapter:
    def __init__(
        self,
        *,
        instance: str | None = None,
        access_token: str | None = None,
    ) -> None:
        host = (
            instance
            or os.environ.get("MASTODON_INSTANCE")
            or _DEFAULT_INSTANCE
        ).strip()
        # Accept either ``mastodon.social`` or ``https://mastodon.social``.
        if "://" in host:
            host = host.split("://", 1)[1]
        host = host.rstrip("/")
        self._instance = host
        self._base_url = f"https://{host}"
        self._token = access_token or os.environ.get("MASTODON_TOKEN")

    def _require_token(self) -> str:
        if not self._token:
            raise MastodonConfigError(
                "MASTODON_TOKEN is not set; configure access_token for "
                "the search_mastodon plugin"
            )
        return self._token

    def _headers(self) -> dict[str, str]:
        return {
            "User-Agent": _USER_AGENT,
            "Accept": "application/json",
            "Authorization": f"Bearer {self._require_token()}",
        }

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        # Mastodon caps ``limit`` at 40 for /api/v2/search.
        api_limit = max(1, min(int(limit) if limit else 10, 40))
        params: dict[str, Any] = {
            "q": q,
            "type": "statuses",
            "limit": api_limit,
        }
        if cursor:
            params["max_id"] = cursor

        url = f"{self._base_url}/api/v2/search"
        async with httpx.AsyncClient(timeout=30.0) as client:
            response = await client.get(
                url, params=params, headers=self._headers()
            )
            response.raise_for_status()
            payload = response.json()

        statuses = payload.get("statuses") or []

        # Normalize ``since`` to an aware datetime for the client-side filter.
        since_aware: datetime | None = None
        if since is not None:
            since_aware = (
                since if since.tzinfo else since.replace(tzinfo=timezone.utc)
            )

        results: list[SearchResult] = []
        last_id: str | None = None
        for status in statuses:
            if not isinstance(status, dict):
                continue
            status_id = status.get("id")
            if isinstance(status_id, (str, int)) and str(status_id):
                last_id = str(status_id)
            created = _parse_created_at(status.get("created_at"))
            if since_aware is not None:
                if created is None or created < since_aware:
                    continue
            results.append(self._to_result(status))

        # ``max_id`` paginates older — if we got a full page, advance to the
        # oldest id we saw; otherwise the feed is exhausted.
        next_cursor = ""
        if last_id and len(statuses) >= api_limit:
            next_cursor = last_id

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=0,
        )

    def _to_result(self, status: dict[str, Any]) -> SearchResult:
        status_id = str(status.get("id") or "")
        content_html = status.get("content") or ""
        stripped = _strip_html(content_html)
        title = stripped[:_TITLE_LIMIT]

        account = status.get("account") or {}
        acct = account.get("acct") if isinstance(account, dict) else None
        authors = [acct] if isinstance(acct, str) and acct else []

        try:
            score = float(status.get("favourites_count") or 0)
        except (TypeError, ValueError):
            score = 0.0

        try:
            replies = int(status.get("replies_count") or 0)
        except (TypeError, ValueError):
            replies = 0

        return SearchResult(
            adapter=_ADAPTER_NAME,
            external_id=status_id,
            title=title,
            url=status.get("url") or "",
            # The brief calls out keeping the full content HTML on abstract.
            abstract=content_html,
            authors=authors,
            published_at=_parse_created_at(status.get("created_at")),
            venue=self._instance,
            citation_count=replies,
            raw=dict(status),
            score=score,
            source_kind=SourceKind.FORUM,
        )
