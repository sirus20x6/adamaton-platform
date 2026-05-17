"""Smoke test for the local filesystem search plugin.

Seeds a ``tmp_path`` with three small files and queries the adapter
directly — no network, no PDF parsing required.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from dr_plugin_search_localfs.adapter import LocalFSAdapter
from dr_plugin_sdk.types import SearchPage, SourceKind


pytestmark = pytest.mark.asyncio


def _seed(tmp_path: Path) -> None:
    (tmp_path / "hello.md").write_text(
        "# Hello\n\nThis note mentions pomegranate twice — pomegranate jam.\n",
        encoding="utf-8",
    )
    (tmp_path / "notes.txt").write_text(
        "Random thoughts about apples and oranges. No fruit named here is special.\n",
        encoding="utf-8",
    )
    (tmp_path / "other.md").write_text(
        "Completely unrelated content about bicycles.\n",
        encoding="utf-8",
    )


async def test_query_finds_matching_file(tmp_path: Path) -> None:
    _seed(tmp_path)
    adapter = LocalFSAdapter(root_path=str(tmp_path))

    page = await adapter.search("pomegranate", limit=10, cursor=None, since=None)

    assert isinstance(page, SearchPage)
    assert len(page.results) == 1
    hit = page.results[0]
    assert hit.title == "hello.md"
    assert hit.adapter == "localfs"
    assert hit.source_kind == SourceKind.WIKI
    assert hit.url.startswith("file://")
    assert hit.url.endswith("hello.md")
    assert "pomegranate" in hit.abstract.lower()
    assert hit.raw["path"].endswith("hello.md")
    assert hit.raw["size"] > 0
    assert hit.score == 2.0  # "pomegranate" appears twice in hello.md
    assert hit.venue == "local filesystem"


async def test_query_no_match_returns_empty_page(tmp_path: Path) -> None:
    _seed(tmp_path)
    adapter = LocalFSAdapter(root_path=str(tmp_path))

    page = await adapter.search("zzz-nothing-matches-zzz", limit=10, cursor=None, since=None)

    assert isinstance(page, SearchPage)
    assert page.results == []
    assert page.next_cursor == ""
    assert page.total_estimated == 0


async def test_pagination_cursor_advances(tmp_path: Path) -> None:
    # Three files all contain "the" — gives us something to paginate.
    (tmp_path / "a.md").write_text("the the the\n", encoding="utf-8")
    (tmp_path / "b.md").write_text("the the\n", encoding="utf-8")
    (tmp_path / "c.md").write_text("the\n", encoding="utf-8")
    adapter = LocalFSAdapter(root_path=str(tmp_path))

    first = await adapter.search("the", limit=2, cursor=None, since=None)
    assert len(first.results) == 2
    assert first.next_cursor == "2"
    assert first.total_estimated == 3
    # Sorted by count desc: a.md (3) then b.md (2) on the first page.
    assert first.results[0].title == "a.md"
    assert first.results[1].title == "b.md"

    second = await adapter.search("the", limit=2, cursor=first.next_cursor, since=None)
    assert len(second.results) == 1
    assert second.results[0].title == "c.md"
    assert second.next_cursor == ""
