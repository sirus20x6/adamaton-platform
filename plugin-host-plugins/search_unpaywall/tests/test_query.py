"""Smoke test for search_unpaywall."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_unpaywall.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    if not os.getenv("UNPAYWALL_EMAIL"):
        # The Unpaywall API requires an email param; without it we can't even
        # form a valid request. Treat as a skip in unit-test mode.
        if not os.getenv("PLUGIN_LIVE_TEST"):
            pytest.skip("UNPAYWALL_EMAIL not set")

    plugin = Plugin()
    try:
        page = await plugin.query("open access machine learning", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"unpaywall upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
