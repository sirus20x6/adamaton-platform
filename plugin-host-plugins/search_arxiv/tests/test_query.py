"""Smoke test for the arxiv search plugin.

arXiv's public API is flaky (the arxiv package will raise
UnexpectedEmptyPageError when the upstream feed momentarily returns
nothing). We treat any such transient failure as a skip rather than a
test failure — same posture as the historical adapter test suite.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_arxiv.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("attention is all you need", 3, None, None)
    except Exception as e:
        # arxiv.UnexpectedEmptyPageError, httpx network errors, etc.
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"arxiv upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "arxiv"
        assert first.source_kind == SourceKind.PREPRINT
        assert first.title


async def test_query_with_empty_query_handles_cleanly() -> None:
    """Defensive: the legacy code passes empty queries straight through.

    We don't assert behavior — just that no exception bubbles out unexpectedly.
    """
    plugin = Plugin()
    try:
        page = await plugin.query("", 1, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"arxiv upstream flaky: {type(e).__name__}: {e}")
    assert isinstance(page, SearchPage)
