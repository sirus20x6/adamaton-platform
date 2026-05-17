"""Smoke test for the Common Crawl search plugin.

The Common Crawl index is sometimes flaky / temporarily returns 503 when
the AWS-hosted CDX node is under load. We follow the same posture as the
arxiv plugin: tolerate transient upstream failures unless
``PLUGIN_LIVE_TEST`` is set.

Note: ``q`` is a URL pattern, not a keyword. We use ``*.example.com/*``
as the smoke query.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_commoncrawl.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("*.example.com/*", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"commoncrawl upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "commoncrawl"
        assert first.source_kind == SourceKind.WEB
        # Common Crawl doesn't expose a title; we set title=url, and url
        # itself should always be present.
        assert first.url
        assert first.external_id


async def test_non_url_pattern_returns_empty_page() -> None:
    """Common Crawl is URL-pattern only — keyword queries must return an
    empty page rather than erroring."""
    plugin = Plugin()
    page = await plugin.query("attention is all you need", 5, None, None)
    assert isinstance(page, SearchPage)
    assert page.results == []
    assert page.next_cursor == ""
