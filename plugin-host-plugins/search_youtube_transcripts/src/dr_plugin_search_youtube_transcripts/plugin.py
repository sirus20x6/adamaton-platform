"""YouTube (transcripts) search plugin."""

from __future__ import annotations

from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import FetchedDoc, SearchPage, SearchResult, SourceKind

from .adapter import YouTubeTranscriptsAdapter


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        self._adapter = YouTubeTranscriptsAdapter()

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        return await self._adapter.search(q, limit=limit, cursor=cursor, since=since)

    @search.fetch
    async def fetch(self, result: SearchResult) -> FetchedDoc:
        return await self._adapter.fetch(result)
