"""Bluesky adapter — wraps app.bsky.feed.searchPosts on the public AppView.

The endpoint is unauthenticated and returns posts in a flat list along with
an opaque `cursor` string for pagination. Each post carries enough metadata
(handle, post record text, createdAt, like/repost/reply counts) to map
directly into the SDK `SearchResult`. Bluesky posts have no titles; we
synthesize one from the first 80 characters of the post body.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

import httpx

from dr_plugin_sdk.types import SearchPage, SearchResult, SourceKind


_API_BASE = "https://public.api.bsky.app/xrpc"
_SEARCH_PATH = "/app.bsky.feed.searchPosts"


def _parse_iso8601(value: str | None) -> datetime | None:
    if not value:
        return None
    # AT Protocol timestamps are RFC 3339 with a trailing 'Z'. Normalize.
    raw = value.replace("Z", "+00:00")
    try:
        dt = datetime.fromisoformat(raw)
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt


def _rkey_from_uri(uri: str) -> str:
    # AT URI form: at://did:plc:xxxxx/app.bsky.feed.post/<rkey>
    if not uri:
        return ""
    return uri.rstrip("/").split("/")[-1]


class BlueskyAdapter:
    name = "bluesky"
    source_kind = SourceKind.FORUM

    def __init__(self) -> None:
        self._client = httpx.AsyncClient(
            timeout=30.0,
            headers={
                "User-Agent": (
                    "adamaton-deepresearch/search_bluesky "
                    "(+https://github.com/sirus20x6)"
                ),
                "Accept": "application/json",
            },
        )

    def _to_result(self, post: dict[str, Any]) -> SearchResult:
        author = post.get("author") or {}
        record = post.get("record") or {}
        handle = author.get("handle") or ""
        uri = post.get("uri") or ""
        rkey = _rkey_from_uri(uri)
        text = (record.get("text") or "").strip()
        title = text[:80]
        url = f"https://bsky.app/profile/{handle}/post/{rkey}" if handle and rkey else ""
        published_at = _parse_iso8601(record.get("createdAt"))
        like_count = int(post.get("likeCount") or 0)

        raw: dict[str, Any] = {
            "uri": uri,
            "cid": post.get("cid") or "",
            "author": {
                "did": author.get("did") or "",
                "handle": handle,
                "displayName": author.get("displayName") or "",
            },
            "indexedAt": post.get("indexedAt") or "",
            "likeCount": like_count,
            "repostCount": int(post.get("repostCount") or 0),
            "replyCount": int(post.get("replyCount") or 0),
        }

        return SearchResult(
            adapter=self.name,
            external_id=uri,
            title=title,
            url=url,
            abstract=text,
            authors=[handle] if handle else [],
            published_at=published_at,
            venue="Bluesky",
            citation_count=0,
            raw=raw,
            score=float(like_count),
            source_kind=SourceKind.FORUM,
        )

    async def search(
        self,
        query: str,
        *,
        limit: int = 10,
        cursor: str | None = None,
        since: datetime | None = None,
    ) -> SearchPage:
        capped_limit = max(1, min(int(limit), 100))

        q = query or ""
        if since is not None:
            # Bluesky's search query language supports `since:YYYY-MM-DD`.
            since_str = since.astimezone(timezone.utc).strftime("%Y-%m-%d")
            if "since:" not in q:
                q = f"{q} since:{since_str}".strip()

        params: dict[str, str] = {"q": q, "limit": str(capped_limit)}
        if cursor:
            params["cursor"] = cursor

        resp = await self._client.get(_API_BASE + _SEARCH_PATH, params=params)
        resp.raise_for_status()
        payload = resp.json()

        posts = payload.get("posts") or []
        next_cursor = payload.get("cursor") or ""

        results = [self._to_result(p) for p in posts]

        return SearchPage(
            results=results,
            next_cursor=next_cursor,
            total_estimated=0,
        )

    async def aclose(self) -> None:
        await self._client.aclose()
