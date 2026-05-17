"""Smoke test for the YouTube (transcripts) search plugin.

YouTube's HTML scraping path (via yt-dlp) is intermittently flaky — rate
limits, signature churn, etc. Treat any upstream failure as a skip unless
``PLUGIN_LIVE_TEST`` is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_youtube_transcripts.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("rust ownership explained", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"youtube upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "youtube_transcripts"
        assert first.source_kind == SourceKind.WEB
        assert first.title or first.external_id
