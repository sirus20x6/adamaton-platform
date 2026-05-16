"""Unit tests for the LinkUp plugin (live test gated on PLUGIN_LIVE_TEST)."""

from __future__ import annotations

import os
from datetime import datetime

import pytest

from dr_plugin_sdk import SearchPage as SDKSearchPage, SearchResult as SDKSearchResult
from dr_plugin_sdk.decorators import _registry

from dr_plugin_search_linkup.adapter import (
    FetchedDoc as LegacyFetchedDoc,
    SearchPage as LegacySearchPage,
    SearchResult as LegacySearchResult,
    SourceKind as LegacySourceKind,
)
from dr_plugin_search_linkup.plugin import Plugin


def test_plugin_instantiates_and_registers_rpcs() -> None:
    os.environ.pop("LINKUP_API_KEY", None)
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
                    adapter="linkup",
                    external_id="def456",
                    title="Hello",
                    url="https://example.com/b",
                    abstract="snip",
                    raw={"type": "text"},
                    source_kind=LegacySourceKind.WEB,
                ),
            ],
            next_cursor=None,
            total_estimated=1,
        )

    monkeypatch.setattr(p._adapter, "search", _fake_search)
    page = await p.query(q="hello", limit=5)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) == 1
    assert page.results[0].url == "https://example.com/b"


async def test_fetch_translates_doc(monkeypatch: pytest.MonkeyPatch) -> None:
    p = Plugin()

    async def _fake_fetch(result):
        return LegacyFetchedDoc(
            adapter="linkup",
            external_id=result.external_id,
            url=result.url,
            title=result.title,
            content_type="text/plain",
            body="body",
            source_tier="api",
            metadata={"linkup_type": "text"},
        )

    monkeypatch.setattr(p._adapter, "fetch", _fake_fetch)
    sr = SDKSearchResult(
        external_id="def456",
        adapter="linkup",
        title="Hello",
        url="https://example.com/b",
    )
    doc = await p.fetch(result=sr)
    assert doc.body == b"body"
    assert doc.metadata.get("linkup_type") == "text"


@pytest.mark.skipif(
    not (os.getenv("PLUGIN_LIVE_TEST") and os.getenv("LINKUP_API_KEY")),
    reason="live test requires PLUGIN_LIVE_TEST=1 and LINKUP_API_KEY",
)
async def test_query_live() -> None:
    p = Plugin()
    page = await p.query(q="transformer architecture", limit=3)
    assert isinstance(page, SDKSearchPage)
    assert len(page.results) >= 1
