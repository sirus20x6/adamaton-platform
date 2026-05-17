"""Logseq graph search plugin."""

from __future__ import annotations

import os
from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import SearchPage

from .adapter import LogseqAdapter


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(self) -> None:
        # Config is supplied via env (the host injects config_schema values).
        graph_path = os.environ.get("LOGSEQ_GRAPH_PATH") or os.environ.get("GRAPH_PATH", "")
        include_journals_env = os.environ.get("LOGSEQ_INCLUDE_JOURNALS", "true")
        include_journals = include_journals_env.strip().lower() not in {"0", "false", "no"}
        self._adapter = LogseqAdapter(
            graph_path=graph_path,
            include_journals=include_journals,
        )

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        return await self._adapter.search(q, limit=limit, cursor=cursor, since=since)
