"""Unit tests for the Jina plugin (live test gated on PLUGIN_LIVE_TEST)."""

from __future__ import annotations

import os
from datetime import datetime

import pytest

from dr_plugin_sdk import SearchPage as SDKSearchPage, SearchResult as SDKSearchResult
from dr_plugin_sdk.decorators import _registry

from dr_plugin_search_jina.adapter import (
    FetchedDoc as LegacyFetchedDoc,
    SearchPage as LegacySearchPage,
    SearchResult as LegacySearchResult,
    SourceKind as LegacySourceKind,
)
from dr_plugin_search_jina.plugin import Plugin


def test_plugin_instantiates_and_registers_rpcs() -> None:
    # Jina key is optional; instantiation must work without one.
    os.environ.pop("JINA_API_KEY", None)
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
                    adapter="jina",
                    external_id="ghi789",
                    title="Hello",
                    url="https://example.com/c",
                    abstract="markdown body",
                    raw={"description": "alt"},
                    source_kind=LegacySourceKind.WEB,
                ),
            ],
            next_cursor=None,
            total_estimated=1,
        )

    monkeypatch.setattr(p._adapter, "search", _fake_search)
    page = await p.query(q="hello", limit=5)
    assert isinstance(page, SDKSearchPage)
    assert page.results[0].abstract == "markdown body"


async def test_fetch_translates_doc(monkeypatch: pytest.MonkeyPatch) -> None:
    p = Plugin()

    async def _fake_fetch(result):
        return LegacyFetchedDoc(
            adapter="jina",
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/markdown",
            body="md body",
            source_tier="api",
            metadata={},
        )

    monkeypatch.setattr(p._adapter, "fetch", _fake_fetch)
    sr = SDKSearchResult(
        external_id="ghi789",
        adapter="jina",
        title="Hello",
        url="https://example.com/c",
    )
    doc = await p.fetch(result=sr)
    assert doc.body == b"md body"
    assert doc.content_type == "text/markdown"


@pytest.mark.skipif(
    not os.getenv("PLUGIN_LIVE_TEST"),
    reason="live test requires PLUGIN_LIVE_TEST=1 (JINA_API_KEY optional)",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query(q="transformer architecture", limit=3)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) >= 1
