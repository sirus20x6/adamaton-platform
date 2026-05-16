"""Unit tests for the SearXNG plugin (live test gated on PLUGIN_LIVE_TEST)."""

from __future__ import annotations

import os
from datetime import datetime

import pytest

from dr_plugin_sdk import SearchPage as SDKSearchPage, SearchResult as SDKSearchResult
from dr_plugin_sdk.decorators import _registry

from dr_plugin_search_searxng.adapter import (
    FetchedDoc as LegacyFetchedDoc,
    SearchPage as LegacySearchPage,
    SearchResult as LegacySearchResult,
    SourceKind as LegacySourceKind,
)
from dr_plugin_search_searxng.plugin import Plugin


def test_plugin_instantiates_and_registers_rpcs() -> None:
    # Adapter constructor doesn't make a network call, so it's fine if
    # SEARXNG_BASE_URL is unset (defaults to http://searxng:8080).
    p = Plugin()
    reg = _registry(p)
    assert reg.get("SearchQuery") == "query"
    assert reg.get("SearchFetch") == "fetch"


async def test_query_returns_sdk_page(monkeypatch: pytest.MonkeyPatch) -> None:
    p = Plugin()

    async def _fake_search(query: str, *, limit: int, cursor: str | None, since: datetime | None):
        return LegacySearchPage(
            results=[
                LegacySearchResult(
                    adapter="searxng",
                    external_id="jkl012",
                    title="Result",
                    url="https://example.com/d",
                    abstract="snippet",
                    raw={"engines": ["google"]},
                    source_kind=LegacySourceKind.WEB,
                ),
            ],
            next_cursor="2",
            total_estimated=42,
        )

    monkeypatch.setattr(p._adapter, "search", _fake_search)
    page = await p.query(q="hello", limit=5)
    assert isinstance(page, SDKSearchPage)
    assert page.next_cursor == "2"
    assert page.total_estimated == 42


async def test_fetch_translates_doc(monkeypatch: pytest.MonkeyPatch) -> None:
    p = Plugin()

    async def _fake_fetch(result):
        return LegacyFetchedDoc(
            adapter="searxng",
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/markdown",
            body="# H\n\nbody\n",
            source_tier="html",
            metadata={"engines": ["google"]},
        )

    monkeypatch.setattr(p._adapter, "fetch", _fake_fetch)
    sr = SDKSearchResult(
        external_id="jkl012",
        adapter="searxng",
        title="Result",
        url="https://example.com/d",
        raw={"engines": ["google"]},
    )
    doc = await p.fetch(result=sr)
    assert doc.body.startswith(b"# H")
    assert doc.source_tier == "html"


@pytest.mark.skipif(
    not (os.getenv("PLUGIN_LIVE_TEST") and (os.getenv("SEARXNG_BASE_URL") or os.getenv("SEARXNG_URL"))),
    reason="live test requires PLUGIN_LIVE_TEST=1 and SEARXNG_BASE_URL/SEARXNG_URL",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query(q="transformer architecture", limit=3)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) >= 1
