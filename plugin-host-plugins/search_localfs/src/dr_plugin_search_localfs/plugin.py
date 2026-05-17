"""Local filesystem search plugin — gRPC façade over LocalFSAdapter."""

from __future__ import annotations

import os
from datetime import datetime

from dr_plugin_sdk import plugin, search
from dr_plugin_sdk.types import SearchPage

from .adapter import LocalFSAdapter


@plugin(manifest="../../plugin.json")
class Plugin:
    """Plugin entrypoint.

    ``root_path`` is sourced from the ``LOCALFS_ROOT`` environment variable
    (the plugin-host materializes per-plugin config into the child env).
    Tests bypass the env entirely by constructing :class:`LocalFSAdapter`
    directly with ``root_path=`` — see ``tests/test_query.py``.
    """

    def __init__(self) -> None:
        root = os.environ.get("LOCALFS_ROOT", "")
        self._adapter = LocalFSAdapter(root_path=root) if root else None

    @search.query
    async def query(
        self,
        q: str,
        limit: int,
        cursor: str | None,
        since: datetime | None,
    ) -> SearchPage:
        if self._adapter is None:
            # No root configured -> nothing to search; return empty page rather
            # than raise, so the host can surface a clean "not configured" UX.
            return SearchPage(results=[], next_cursor="", total_estimated=0)
        return await self._adapter.search(q, limit=limit, cursor=cursor, since=since)
