"""Smoke test for the NIST NVD (CVE) search plugin.

The NVD public API is occasionally rate-limited or transiently slow; we
follow the same posture as the other network-backed plugins and skip on
upstream errors unless ``PLUGIN_LIVE_TEST`` is set.
"""

from __future__ import annotations

import os

import pytest

from dr_plugin_search_nist.plugin import Plugin
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


async def test_query_returns_search_page() -> None:
    plugin = Plugin()
    try:
        page = await plugin.query("buffer overflow", 3, None, None)
    except Exception as e:
        if os.getenv("PLUGIN_LIVE_TEST"):
            raise
        pytest.skip(f"nvd upstream flaky: {type(e).__name__}: {e}")

    assert isinstance(page, SearchPage)
    assert isinstance(page.results, list)
    if page.results:
        first = page.results[0]
        assert first.adapter == "nist_nvd"
        assert first.source_kind == SourceKind.WEB
        assert first.title or first.external_id
        # external_id should look like a CVE id when present.
        if first.external_id:
            assert first.external_id.startswith("CVE-")
