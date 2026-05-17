"""Smoke test for search_biorxiv.

Europe PMC's public API can be intermittently slow or rate-limit; any
upstream failure outside of PLUGIN_LIVE_TEST mode is treated as a skip,
matching the arxiv plugin posture.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_sdk.types import SearchPage
from dr_plugin_search_biorxiv.plugin import Plugin

pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("single-cell RNA-seq", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(
            f"Europe PMC upstream flaky: {type(e).__name__}: {e}"
        )

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.title or first.external_id
