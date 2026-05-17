"""Smoke test for the Bluesky search plugin.

The public AT Protocol AppView is generally healthy, but we follow the
same tolerance posture as the other network-backed plugins: any upstream
failure is treated as a skip unless PLUGIN_LIVE_TEST is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_bluesky.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("machine learning", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"bluesky upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "bluesky"
        assert first.source_kind == SourceKind.FORUM
        assert first.title or first.external_id
