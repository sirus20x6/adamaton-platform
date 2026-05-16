"""Smoke test for the openalex search plugin."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_openalex.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


pytestmark = pytest.mark.asyncio


@pytest.mark.skipif(
    not os.getenv("PLUGIN_LIVE_TEST"),
    reason="openalex requires the live API; set PLUGIN_LIVE_TEST=1",
)
async def test_query_live() -> None:
    plugin = Plugin()
    page = await plugin.query("graph neural network", 3, None, None)
    assert isinstance(page, SearchPage)
    if page.results:
        first = page.results[0]
        assert first.adapter == "openalex"
        assert first.title


async def test_plugin_instantiates() -> None:
    plugin = Plugin()
    assert plugin._adapter.name == "openalex"
    await plugin._adapter.aclose()
