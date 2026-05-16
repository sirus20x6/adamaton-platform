"""Smoke test for the semantic_scholar search plugin.

Network-required test gated on PLUGIN_LIVE_TEST=1 to keep CI hermetic.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_semantic_scholar.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


@pytest.mark.skipif(
    not os.getenv("PLUGIN_LIVE_TEST"),
    reason="semantic_scholar requires the live API; set PLUGIN_LIVE_TEST=1",
)
async def test_query_live() -> None:
    plugin = Plugin()
    page = await plugin.query("attention is all you need", 3, None, None)
    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "semantic_scholar"
        assert first.title


async def test_plugin_instantiates() -> None:
    # Always-run smoke: confirm wiring works without making any HTTP calls.
    plugin = Plugin()
    assert plugin._adapter.name == "semantic_scholar"
    await plugin._adapter.aclose()
