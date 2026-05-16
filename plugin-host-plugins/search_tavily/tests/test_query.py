"""Unit tests for the Tavily plugin.

The unit path stubs the underlying adapter so we exercise the SDK
wiring (decorators + dataclass coercion) without a live API call. A
live integration test runs when ``PLUGIN_LIVE_TEST=1`` and a real
``TAVILY_API_KEY`` is present.
"""

from __future__ import annotations

import os
from datetime import datetime, timezone

import pytest

from dr_plugin_sdk import SearchPage as SDKSearchPage, SearchResult as SDKSearchResult
from dr_plugin_sdk.decorators import _registry

from dr_plugin_search_tavily.adapter import (
    SearchPage as LegacySearchPage,
    SearchResult as LegacySearchResult,
    SourceKind as LegacySourceKind,
)
from dr_plugin_search_tavily.plugin import Plugin


def test_plugin_instantiates_and_registers_rpcs() -> None:
    # No API key needed: the constructor doesn't call the network.
    os.environ.pop("TAVILY_API_KEY", None)
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
                    adapter="tavily",
                    external_id="abc123",
                    title="Hello",
                    url="https://example.com/a",
                    abstract="snippet",
                    raw={"score": 0.9, "published_date": "2026-01-01"},
                    score=0.9,
                    source_kind=LegacySourceKind.WEB,
                ),
            ],
            next_cursor=None,
            total_estimated=1,
        )

    monkeypatch.setattr(p._adapter, "search", _fake_search)
    page = await p.query(q="hello world", limit=5)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) == 1
    r = page.results[0]
    assert isinstance(r, SDKSearchResult)
    assert r.external_id == "abc123"
    assert r.url == "https://example.com/a"
    assert r.abstract == "snippet"


async def test_fetch_translates_doc(monkeypatch: pytest.MonkeyPatch) -> None:
    from dr_plugin_search_tavily.adapter import FetchedDoc as LegacyFetchedDoc

    p = Plugin()

    async def _fake_fetch(result):
        return LegacyFetchedDoc(
            adapter="tavily",
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/plain",
            body="cleaned body",
            source_tier="api",
            metadata={"score": 0.5, "published_date": None},
        )

    monkeypatch.setattr(p._adapter, "fetch", _fake_fetch)
    sr = SDKSearchResult(
        external_id="abc123",
        adapter="tavily",
        title="Hello",
        url="https://example.com/a",
        abstract="snippet",
    )
    doc = await p.fetch(result=sr)
    assert doc.body == b"cleaned body"
    assert doc.url == "https://example.com/a"
    assert doc.source_tier == "api"


@pytest.mark.skipif(
    not (os.getenv("PLUGIN_LIVE_TEST") and os.getenv("TAVILY_API_KEY")),
    reason="live test requires PLUGIN_LIVE_TEST=1 and TAVILY_API_KEY",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query(q="transformer architecture", limit=3)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) >= 1
