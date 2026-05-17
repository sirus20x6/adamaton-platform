"""Smoke test for the search_mastodon plugin.

Mastodon's /api/v2/search requires a bearer token, so this test skips
when ``MASTODON_TOKEN`` is missing (the host injects it at runtime).
Network flakes are also tolerated unless PLUGIN_LIVE_TEST is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_mastodon.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    if not os.getenv("MASTODON_TOKEN"):
        pytest.skip("MASTODON_TOKEN not set; mastodon /api/v2/search requires auth")

    plugin = Plugin()
    try:
        page = await plugin.query("open source", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"mastodon upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "mastodon"
        assert first.source_kind == SourceKind.FORUM
        assert first.title or first.external_id
