"""Smoke tests for the wiki_crawler plugin.

Real crawls hit the network and are blocked by Cloudflare in CI; we
stub the crawler classes with in-memory fakes that yield a fixed list
of WikiPage objects.
"""

from __future__ import annotations

import pytest

from dr_plugin_search_wiki_crawler import adapter as adapter_mod
from dr_plugin_search_wiki_crawler.adapter import (
    WikiCrawlerAdapter,
    _parse_query,
    _wikipage_to_result,
)
from dr_plugin_search_wiki_crawler.wiki_crawler import WikiPage


# ----- query parsing ---------------------------------------------------


def test_parse_query_default_source() -> None:
    src, slug = _parse_query("Sigmar")
    assert src == "lexicanum"
    assert slug == "Sigmar"


def test_parse_query_explicit_source() -> None:
    src, slug = _parse_query("tvtropes:/pmwiki/pmwiki.php/Main/SpaceMarines")
    assert src == "tvtropes"
    assert slug == "/pmwiki/pmwiki.php/Main/SpaceMarines"


def test_parse_query_unknown_prefix_falls_through() -> None:
    # ``arxiv:1234`` isn't a wiki source; treat the whole string as a slug.
    src, slug = _parse_query("arxiv:1234")
    assert src == "lexicanum"
    assert slug == "arxiv:1234"


# ----- wikipage -> result conversion -----------------------------------


def test_wikipage_to_result_extracts_first_paragraph() -> None:
    page = WikiPage(
        source="lexicanum",
        title="Sigmar",
        url="https://wh40k.lexicanum.com/wiki/Sigmar",
        body_markdown="# Heading\n\nSigmar is the patron god of the Empire.\n\nMore prose.",
        metadata={"slug": "Sigmar"},
    )
    r = _wikipage_to_result(page, "wiki_crawler")
    assert r.external_id == "Sigmar"
    assert r.title == "Sigmar"
    assert r.url.endswith("/wiki/Sigmar")
    assert "patron god" in (r.abstract or "")


def test_wikipage_to_result_handles_empty_body() -> None:
    page = WikiPage(
        source="fandom",
        title="X",
        url="https://example.com/wiki/X",
        body_markdown="",
        metadata={"slug": "X"},
    )
    r = _wikipage_to_result(page, "wiki_crawler")
    assert r.external_id == "X"
    assert r.abstract is None


# ----- adapter integration (with stubbed crawlers) ---------------------


class _FakeCrawler:
    """Yields a fixed list of pages regardless of seeds."""

    def __init__(self, pages: list[WikiPage]) -> None:
        self._pages = pages

    async def crawl(self, **_kwargs):
        for p in self._pages:
            yield p

    async def aclose(self) -> None:
        return None


@pytest.fixture
def stub_crawlers(monkeypatch: pytest.MonkeyPatch):
    pages = [
        WikiPage(
            source="lexicanum",
            title="Sigmar",
            url="https://wh40k.lexicanum.com/wiki/Sigmar",
            body_markdown="Sigmar is the warrior-god of the Empire.",
            metadata={"slug": "Sigmar"},
        ),
        WikiPage(
            source="lexicanum",
            title="Karl Franz",
            url="https://wh40k.lexicanum.com/wiki/Karl_Franz",
            body_markdown="Karl Franz is the Emperor of Sigmar's Empire.",
            metadata={"slug": "Karl_Franz"},
        ),
        WikiPage(
            source="lexicanum",
            title="Magnus",
            url="https://wh40k.lexicanum.com/wiki/Magnus_the_Pious",
            body_markdown="Magnus the Pious defeated the Great War Against Chaos.",
            metadata={"slug": "Magnus_the_Pious"},
        ),
    ]

    def _fake_lex(**kwargs):
        return _FakeCrawler(pages)

    def _fake_tvt(**kwargs):
        return _FakeCrawler(pages)

    def _fake_fan(**kwargs):
        return _FakeCrawler(pages)

    monkeypatch.setattr(adapter_mod, "LexicanumCrawler", _fake_lex)
    monkeypatch.setattr(adapter_mod, "TVTropesCrawler", _fake_tvt)
    monkeypatch.setattr(adapter_mod, "FandomCrawler", _fake_fan)
    return pages


@pytest.mark.asyncio
async def test_search_returns_capped_results(stub_crawlers) -> None:
    a = WikiCrawlerAdapter(max_pages_per_query=10)
    page = await a.search("Sigmar", limit=2)
    assert len(page.results) == 2
    assert page.results[0].title == "Sigmar"
    # 3 stub pages, limit=2 → cursor advances to 2 and signals more.
    assert page.next_cursor == "2"


@pytest.mark.asyncio
async def test_search_cursor_paginates(stub_crawlers) -> None:
    a = WikiCrawlerAdapter(max_pages_per_query=10)
    page = await a.search("Sigmar", limit=2, cursor="2")
    assert len(page.results) == 1
    assert page.next_cursor is None  # exhausted


@pytest.mark.asyncio
async def test_search_respects_max_pages_per_query_cap(stub_crawlers) -> None:
    # max_pages_per_query=2 caps the BFS even if the caller wants more.
    a = WikiCrawlerAdapter(max_pages_per_query=2)
    page = await a.search("Sigmar", limit=10)
    assert len(page.results) == 2


# ----- plugin wiring ---------------------------------------------------


def test_plugin_class_has_search_handlers() -> None:
    # Import here so the @plugin decorator runs against the real manifest.
    from dr_plugin_search_wiki_crawler.plugin import Plugin

    registry = getattr(Plugin, "_dr_rpc_registry", {})
    assert "SearchQuery" in registry
    assert "SearchFetch" in registry


def test_manifest_capabilities() -> None:
    from dr_plugin_search_wiki_crawler.plugin import Plugin

    caps = getattr(Plugin, "_dr_capabilities", [])
    assert "search.query" in caps
    assert "search.fetch" in caps
