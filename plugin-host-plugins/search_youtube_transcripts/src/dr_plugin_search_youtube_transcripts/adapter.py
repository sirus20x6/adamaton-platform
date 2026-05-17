"""YouTube (transcripts) adapter.

Search uses ``yt-dlp``'s ``ytsearchN:q`` shortcut with ``extract_flat`` to
avoid hitting the YouTube Data API. Fetch retrieves the auto/uploaded
captions via ``youtube-transcript-api`` and returns them as plain text.

Both client libraries are synchronous, so we run them on the default
executor.
"""

from __future__ import annotations

import asyncio
from datetime import datetime
from typing import Any

from dr_plugin_sdk.types import FetchedDoc, SearchPage, SearchResult, SourceKind


_ADAPTER_NAME = "youtube_transcripts"


def _parse_upload_date(raw: Any) -> datetime | None:
    if not raw:
        return None
    s = str(raw)
    if len(s) != 8 or not s.isdigit():
        return None
    try:
        return datetime.strptime(s, "%Y%m%d")
    except ValueError:
        return None


class YouTubeTranscriptsAdapter:
    name = _ADAPTER_NAME
    source_kind = SourceKind.WEB

    def __init__(self) -> None:
        # yt-dlp options for fast metadata-only search.
        self._ydl_opts = {
            "quiet": True,
            "extract_flat": True,
            "default_search": "ytsearch",
            "skip_download": True,
            "no_warnings": True,
        }

    def _search_blocking(self, q: str, n: int) -> dict[str, Any]:
        # Imported lazily so module import doesn't fail when yt-dlp is missing
        # in unit-test/lint environments.
        from yt_dlp import YoutubeDL  # type: ignore

        with YoutubeDL(self._ydl_opts) as ydl:
            return ydl.extract_info(f"ytsearch{n}:{q}", download=False) or {}

    def _to_result(self, entry: dict[str, Any]) -> SearchResult:
        video_id = entry.get("id") or ""
        title = (entry.get("title") or "").strip()
        channel = entry.get("channel") or entry.get("uploader") or ""
        url = entry.get("url") or (
            f"https://www.youtube.com/watch?v={video_id}" if video_id else ""
        )
        # extract_flat sometimes returns a bare /watch?v=ID under "url"; rebuild
        # to a canonical https URL when that happens.
        if url and not url.startswith("http"):
            url = f"https://www.youtube.com/watch?v={video_id}"
        published_at = _parse_upload_date(entry.get("upload_date"))
        view_count = entry.get("view_count")
        score = float(view_count) if isinstance(view_count, (int, float)) else 0.0

        raw: dict[str, Any] = {
            "id": video_id,
            "channel": channel,
            "duration": entry.get("duration"),
            "view_count": view_count,
            "upload_date": entry.get("upload_date"),
            "channel_id": entry.get("channel_id"),
            "uploader": entry.get("uploader"),
            "live_status": entry.get("live_status"),
        }

        return SearchResult(
            adapter=_ADAPTER_NAME,
            external_id=video_id,
            title=title,
            url=url,
            abstract="",
            authors=[channel] if channel else [],
            published_at=published_at,
            venue="YouTube",
            citation_count=0,
            raw=raw,
            score=score,
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
        offset = int(cursor) if cursor and cursor.isdigit() else 0
        limit = max(int(limit), 1)
        # yt-dlp returns the first N hits — over-fetch to support the offset.
        n = offset + limit

        loop = asyncio.get_running_loop()
        info = await loop.run_in_executor(None, self._search_blocking, query, n)

        entries = list(info.get("entries") or [])

        # Map all then optionally filter by `since` — done client-side because
        # yt-dlp's flat search doesn't expose a server-side date filter.
        results_all = [self._to_result(e) for e in entries if e]
        if since is not None:
            results_all = [
                r for r in results_all if r.published_at and r.published_at >= since
            ]

        sliced = results_all[offset : offset + limit]
        next_cursor = (
            str(offset + len(sliced)) if len(results_all) > offset + limit else ""
        )

        return SearchPage(
            results=sliced,
            next_cursor=next_cursor,
            total_estimated=len(results_all),
        )

    def _fetch_transcript_blocking(self, video_id: str) -> str:
        from youtube_transcript_api import YouTubeTranscriptApi  # type: ignore

        transcript = YouTubeTranscriptApi.get_transcript(video_id)
        return "\n".join(segment.get("text", "") for segment in transcript)

    async def fetch(self, result: SearchResult) -> FetchedDoc:
        video_id = result.external_id
        if not video_id:
            raise ValueError("search_youtube_transcripts.fetch: missing external_id")

        loop = asyncio.get_running_loop()
        text = await loop.run_in_executor(
            None, self._fetch_transcript_blocking, video_id
        )

        url = result.url or f"https://www.youtube.com/watch?v={video_id}"
        return FetchedDoc(
            adapter=_ADAPTER_NAME,
            external_id=video_id,
            url=url,
            title=result.title or "",
            content_type="text/plain",
            body=text.encode("utf-8"),
            source_tier="transcript",
            metadata={
                "venue": "YouTube",
                "channel": result.authors[0] if result.authors else "",
            },
        )

    async def aclose(self) -> None:
        return None
