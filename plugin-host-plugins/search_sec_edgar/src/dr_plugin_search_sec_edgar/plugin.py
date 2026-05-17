"""SEC EDGAR search plugin."""

from __future__ import annotations

from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import SearchPage

from .adapter import SecEdgarAdapter


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        self._adapter = SecEdgarAdapter()

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        return await self._adapter.search(q, limit=limit, cursor=cursor, since=since)
