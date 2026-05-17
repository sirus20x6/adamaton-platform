"""Smoke test for the pubmed search plugin."""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_pubmed.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("CRISPR review", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"pubmed upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "pubmed"
        assert first.source_kind == SourceKind.JOURNAL
        assert first.title or first.external_id
        assert first.url.startswith("https://pubmed.ncbi.nlm.nih.gov/")
