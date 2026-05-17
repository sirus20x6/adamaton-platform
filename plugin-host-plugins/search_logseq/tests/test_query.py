"""Smoke test for the Logseq graph search plugin.

Seeds a tiny graph (one regular page + one journal entry) in ``tmp_path``
and verifies that the adapter finds matches, parses the journal date
from the underscored filename, and paginates as expected.
"""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path

import pytest

from dr_plugin_search_logseq.adapter import LogseqAdapter
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


def _seed_graph(root: Path) -> None:
    (root / "pages").mkdir(parents=True)
    (root / "journals").mkdir(parents=True)
    (root / "assets").mkdir(parents=True)
    (root / "logseq").mkdir(parents=True)

    (root / "pages" / "test.md").write_text(
        "# Test Page\n\nThis page mentions the keyword pineapple in passing.\n",
        encoding="utf-8",
    )
    (root / "journals" / "2024_01_15.md").write_text(
        "- woke up\n- ate pineapple for breakfast\n- read about transformers\n",
        encoding="utf-8",
    )
    # Skip-dir contents must never appear in results.
    (root / "assets" / "ignored.md").write_text("pineapple should not match\n", encoding="utf-8")
    (root / "logseq" / "config.md").write_text("pineapple again\n", encoding="utf-8")


async def test_query_finds_page_and_journal(tmp_path: Path) -> None:
    _seed_graph(tmp_path)
    adapter = LogseqAdapter(graph_path=str(tmp_path), include_journals=True)
    page = await adapter.search("pineapple", limit=10)

    assert isinstance(page, SearchPage)
    assert page.total_estimated == 2
    assert len(page.results) == 2

    # Sorted newest-first: journal date is 2024-01-15; page mtime is "now"
    # (well after 2024), so the page should come first.
    titles = [r.title for r in page.results]
    assert "Test Page" in titles
    # The journal entry has no `# heading`, so its title falls back to the stem.
    assert "2024_01_15" in titles

    journal = next(r for r in page.results if r.title == "2024_01_15")
    assert journal.adapter == "logseq"
    assert journal.source_kind == SourceKind.WIKI
    assert journal.url.startswith("file://")
    assert journal.raw["is_journal"] is True
    assert journal.published_at == datetime(2024, 1, 15, tzinfo=timezone.utc)
    assert "pineapple" in journal.abstract.lower()
    assert journal.venue == tmp_path.name

    regular = next(r for r in page.results if r.title == "Test Page")
    assert regular.raw["is_journal"] is False
    # No-skip-dir contamination.
    for r in page.results:
        assert "/assets/" not in r.raw["path"]
        assert "/logseq/" not in r.raw["path"]


async def test_query_respects_include_journals_flag(tmp_path: Path) -> None:
    _seed_graph(tmp_path)
    adapter = LogseqAdapter(graph_path=str(tmp_path), include_journals=False)
    page = await adapter.search("pineapple", limit=10)
    assert page.total_estimated == 1
    assert [r.title for r in page.results] == ["Test Page"]


async def test_query_filters_by_since(tmp_path: Path) -> None:
    _seed_graph(tmp_path)
    adapter = LogseqAdapter(graph_path=str(tmp_path), include_journals=True)
    # The journal is dated 2024-01-15; this cutoff is after that, so the
    # journal should drop out. The page's mtime is "now" so it stays.
    cutoff = datetime(2024, 6, 1, tzinfo=timezone.utc)
    page = await adapter.search("pineapple", limit=10, since=cutoff)
    assert [r.title for r in page.results] == ["Test Page"]


async def test_query_paginates_by_offset(tmp_path: Path) -> None:
    _seed_graph(tmp_path)
    adapter = LogseqAdapter(graph_path=str(tmp_path), include_journals=True)

    first = await adapter.search("pineapple", limit=1)
    assert len(first.results) == 1
    assert first.next_cursor == "1"

    second = await adapter.search("pineapple", limit=1, cursor=first.next_cursor)
    assert len(second.results) == 1
    assert second.next_cursor == ""
    # The two pages should not overlap.
    assert first.results[0].external_id != second.results[0].external_id


async def test_query_returns_empty_when_graph_missing(tmp_path: Path) -> None:
    adapter = LogseqAdapter(graph_path=str(tmp_path / "does-not-exist"))
    page = await adapter.search("anything", limit=5)
    assert isinstance(page, SearchPage)
    assert page.results == []
    assert page.total_estimated == 0
    assert page.next_cursor == ""
