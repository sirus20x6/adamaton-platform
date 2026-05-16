"""Sanity test: Plugin() loads, query() returns a SearchPage.

The live network call is gated on PLUGIN_LIVE_TEST so CI runs offline.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_hf_papers.plugin import Plugin
from dr_plugin_sdk.types import SearchPage


def test_plugin_instantiates() -> None:
    # Smoke: class decoration + manifest load + lazy adapter init.
    p = Plugin()
    assert p is not None
    # Registry populated from the @search.query / @search.fetch tags.
    reg = getattr(Plugin, "_dr_rpc_registry", {})
    assert "SearchQuery" in reg
    assert "SearchFetch" in reg


@pytest.mark.asyncio
@pytest.mark.skipif(
    not os.getenv("PLUGIN_LIVE_TEST"),
    reason="set PLUGIN_LIVE_TEST=1 to hit huggingface.co",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query("transformer", 5, None, None)
    assert isinstance(page, SearchPage)
    # HF papers may return zero hits for an obscure query, so just assert shape.
    assert isinstance(page.results, list)
