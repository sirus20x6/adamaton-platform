"""Smoke test for the pkg.go.dev search plugin.

pkg.go.dev has no JSON API; the adapter scrapes HTML, so it is sensitive
to both network flakiness and layout changes. Treat any upstream error
as a skip unless PLUGIN_LIVE_TEST is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_pkggo.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("kubernetes client", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"pkg.go.dev upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
