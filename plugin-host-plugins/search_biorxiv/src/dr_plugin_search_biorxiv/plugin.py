"""bioRxiv / medRxiv / chemRxiv search plugin.

Note: the bioRxiv native JSON API (`api.biorxiv.org`) only supports
date-range listing, so this plugin proxies through Europe PMC, which
indexes all three preprint servers and supports keyword + cursorMark.
"""

from __future__ import annotations

import os
from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import SearchPage

from .adapter import BiorxivAdapter


def _servers_from_env() -> list[str] | None:
    raw = os.environ.get("BIORXIV_SERVERS")
    if not raw:
        return None
    parts = [p.strip() for p in raw.split(",") if p.strip()]
    return parts or None


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        self._adapter = BiorxivAdapter(servers=_servers_from_env())

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        return await self._adapter.search(
            q, limit=limit, cursor=cursor, since=since
        )
