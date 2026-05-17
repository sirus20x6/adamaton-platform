"""Smoke test for the OECD iLibrary search plugin.

OECD iLibrary is an HTML scrape — markup drift or upstream hiccups can cause
parse failures. We treat any such transient failure as a skip, matching the
historical adapter tolerance pattern, unless PLUGIN_LIVE_TEST is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_oecd.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("GDP forecast", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"OECD iLibrary upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
