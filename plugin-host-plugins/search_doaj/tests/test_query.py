"""Smoke test for search_doaj."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_doaj.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("climate change", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"doaj upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
