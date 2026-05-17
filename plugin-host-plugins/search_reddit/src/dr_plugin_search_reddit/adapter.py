"""Async adapter for the unauthenticated Reddit JSON search endpoint.

Endpoint:
    GET https://www.reddit.com/search.json
        ?q={q}&limit={N}&after={fullname}&t={time}

The response is a "listing" JSON shape:

    {
        "kind": "Listing",
        "data": {
            "after": "t3_xyz",  # cursor for the next page
            "before": null,
            "dist": 25,
            "children": [
                {"kind": "t3", "data": {"id": "...", "name": "t3_...",
                                         "title": "...", "selftext": "...",
                                         "url": "...", "permalink": "...",
                                         "subreddit": "...", "author": "...",
                                         "created_utc": 1234567890.0,
                                         "score": 42, "num_comments": 7}},
                ...
            ]
        }
    }

A clear, non-generic User-Agent is REQUIRED — Reddit returns HTTP 429
otherwise. We don't have OAuth so this is the only practical signal.

Date filtering: Reddit only supports a coarse ``t=hour|day|week|month|
year|all`` knob. We pick the smallest window that covers ``since`` and
then filter client-side on ``created_utc``.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_ADAPTER_NAME = "reddit"
_USER_AGENT = "adamaton-deepresearch/search_reddit (by /u/sirus20x6)"
_BASE_URL = "https://www.reddit.com"

# Ordered smallest -> largest. Each entry is (slug, max-age-seconds).
# ``all`` is unbounded so we use it as the fallback when ``since`` is None or
# older than a year.
_TIME_BUCKETS: list[tuple[str, float]] = [
    ("hour", 60 * 60),
    ("day", 60 * 60 * 24),
    ("week", 60 * 60 * 24 * 7),
    ("month", 60 * 60 * 24 * 31),
    ("year", 60 * 60 * 24 * 366),
]


def _pick_time_bucket(since: datetime | None) -> str:
    """Pick the smallest Reddit ``t`` bucket that fully covers ``since``."""
    if since is None:
        return "all"
    since_aware = since if since.tzinfo else since.replace(tzinfo=timezone.utc)
    age = (datetime.now(timezone.utc) - since_aware).total_seconds()
    if age <= 0:
        return "hour"
    for slug, span in _TIME_BUCKETS:
        if age <= span:
            return slug
    return "all"


def _parse_created(value: Any) -> datetime | None:
    if value is None:
        return None
    try:
        return datetime.fromtimestamp(float(value), tz=timezone.utc)
    except (TypeError, ValueError, OSError):
        return None


class RedditAdapter:
    def __init__(self, base_url: str | None = None) -> None:
        self._base_url = (base_url or _BASE_URL).rstrip("/")

    def _headers(self) -> dict[str, str]:
        return {
            "User-Agent": _USER_AGENT,
            "Accept": "application/json",
        }

    async def search(
        self,
        q: str,
        *,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        # Reddit caps at 100 per page; default to the requested limit but
        # clamp to a reasonable range.
        api_limit = max(1, min(int(limit) if limit else 10, 100))

        params: dict[str, Any] = {
            "q": q,
            "limit": api_limit,
            "t": _pick_time_bucket(since),
        }
        if cursor:
            params["after"] = cursor

        url = f"{self._base_url}/search.json"
        async with httpx.AsyncClient(timeout=30.0) as client:
            response = await client.get(
                url, params=params, headers=self._headers()
            )
            response.raise_for_status()
            payload = response.json()

        data = payload.get("data") or {}
        children = data.get("children") or []

        # Normalize ``since`` for the client-side filter.
        since_aware: datetime | None = None
        if since is not None:
            since_aware = (
                since if since.tzinfo else since.replace(tzinfo=timezone.utc)
            )

        results: list[SearchResult] = []
        last_name: str | None = None
        for child in children:
            if not isinstance(child, dict):
                continue
            item = child.get("data") or {}
            if not isinstance(item, dict):
                continue
            created = _parse_created(item.get("created_utc"))
            if since_aware is not None:
                if created is None or created < since_aware:
                    # Still remember the fullname so the next page after
                    # this one can pick up correctly.
                    name = item.get("name")
                    if isinstance(name, str) and name:
                        last_name = name
                    continue
            results.append(_post_to_result(item))
            name = item.get("name")
            if isinstance(name, str) and name:
                last_name = name

        # Reddit's listing exposes its own ``after`` token; prefer that, fall
        # back to the last seen fullname so we still advance even if every
        # row on this page was filtered out client-side.
        next_cursor = data.get("after") or last_name or ""
        if not isinstance(next_cursor, str):
            next_cursor = ""
        # If the page was short and Reddit gave us no ``after``, we're done.
        if not data.get("after") and len(children) < api_limit:
            next_cursor = ""

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=0,
        )


def _post_to_result(post: dict[str, Any]) -> SearchResult:
    fullname = post.get("name") or post.get("id") or ""
    permalink = post.get("permalink") or ""
    if permalink and not permalink.startswith("http"):
        post_url = f"https://reddit.com{permalink}"
    else:
        post_url = permalink or (post.get("url") or "")

    subreddit = post.get("subreddit") or ""
    venue = f"r/{subreddit}" if subreddit else ""

    author = post.get("author")
    authors = [author] if isinstance(author, str) and author else []

    try:
        score = float(post.get("score") or 0)
    except (TypeError, ValueError):
        score = 0.0

    return SearchResult(
        adapter=_ADAPTER_NAME,
        external_id=str(fullname),
        title=post.get("title") or "",
        url=post_url,
        abstract=post.get("selftext") or "",
        authors=authors,
        published_at=_parse_created(post.get("created_utc")),
        venue=venue,
        citation_count=int(post.get("num_comments") or 0),
        raw=dict(post),
        score=score,
        source_kind=SourceKind.FORUM,
    )
