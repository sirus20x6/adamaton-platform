"""Smoke test for the CORE search plugin.

CORE requires CORE_API_KEY; without it the adapter raises at init, so
we skip the smoke test cleanly when the key isn't available.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_core.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    if not os.getenv("CORE_API_KEY"):
        pytest.skip("CORE_API_KEY not set; CORE adapter cannot initialize")

    try:
        plugin = Plugin()
        page = await plugin.query("graph neural networks", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"CORE upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
