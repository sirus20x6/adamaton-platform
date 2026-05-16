"""Sanity test for the Wikipedia search plugin."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_wikipedia.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


def test_plugin_instantiates() -> None:
    p = Plugin()
    assert p is not None
    reg = getattr(Plugin, "_dr_rpc_registry", {})
    assert "SearchQuery" in reg
    assert "SearchFetch" in reg


@pytest.mark.asyncio
@pytest.mark.skipif(
    not os.getenv("PLUGIN_LIVE_TEST"),
    reason="set PLUGIN_LIVE_TEST=1 to hit en.wikipedia.org",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query("Alan Turing", 5, None, None)
    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
