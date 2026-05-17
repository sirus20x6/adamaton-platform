"""Obsidian vault search plugin.

Thin gRPC façade over :class:`ObsidianAdapter`. The adapter is constructed
lazily from plugin config (``vault_path`` is required); the host injects
config via constructor kwargs.
"""

from __future__ import annotations

import os
from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import SearchPage

from .adapter import ObsidianAdapter


@plugin(manifest="../../plugin.json")
class Plugin:
    def __init__(
        self,
        vault_path: str | None = None,
        exclude_folders: list[str] | None = None,
    ) -> None:
        # Allow the host to pass config either via kwargs (preferred) or
        # via the OBSIDIAN_VAULT_PATH env var as a deployment escape hatch.
        path = vault_path or os.environ.get("OBSIDIAN_VAULT_PATH") or ""
        excludes = (
            exclude_folders
            if exclude_folders is not None
            else [".obsidian", ".trash"]
        )
        self._adapter = ObsidianAdapter(
            vault_path=path,
            exclude_folders=excludes,
        )

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
