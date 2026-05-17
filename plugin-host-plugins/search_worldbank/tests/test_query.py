"""Smoke test for the World Bank WDS search plugin."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_worldbank.plugin import Plugin
from dr_plugin_sdk.types import SearchPage

pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("education investment", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"worldbank upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
